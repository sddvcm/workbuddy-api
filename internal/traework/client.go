package traework

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"workbuddy-api/internal/auth"
	"workbuddy-api/internal/idle"
	"workbuddy-api/internal/provider"
)

var sessionDeadMarkers = []string{"login", "token 失效", "token invalid", "session", "unauthorized", "401"}

// Classify 按 HTTP 状态码 + body 判定错误类别。
// 状态码优先于 body 硬余额标记：429/401 的响应体也会携带业务码 1005
// （上游报错格式不受本端控制），先判 1005 会把限流/登录失效误判成硬余额、
// 触发 12h 硬冷却，且 401 会绕过自愈重登——对齐 qodercn/qodercom/qwenwork
// 的「状态码 → hard 标记」判定顺序。
func Classify(status int, body string) provider.ErrKind {
	lower := strings.ToLower(body)
	if status == http.StatusUnauthorized {
		for _, m := range sessionDeadMarkers {
			if strings.Contains(lower, strings.ToLower(m)) {
				return provider.ErrSessionDead
			}
		}
		return provider.ErrSessionDead
	}
	if status == http.StatusTooManyRequests {
		return provider.ErrSoftRate
	}
	if strings.Contains(body, `"code":1005`) || (strings.Contains(body, "1005") && strings.Contains(lower, "plan")) {
		return provider.ErrHardCredit
	}
	if status == http.StatusNotFound {
		return provider.ErrNotFound
	}
	if status >= 500 {
		return provider.ErrServer
	}
	if status >= 400 {
		return provider.ErrClient
	}
	return provider.ErrNone
}

// Client Trae SOLO 上游 HTTP 客户端。
type Client struct {
	HTTP       *http.Client
	StreamHTTP *http.Client
	AgentHost  string
	UgHost     string
	OAuthHost  string
	// PricingHost 定价接口基址；空则回落到 WorkHost。
	// 单列为字段是为了让测试可指向本地 mock。
	PricingHost       string
	ClientID          string
	CheckinRetryDelay time.Duration // 9074 限流后的重试等待；生产默认 8s
	// IdleTimeout 聊天 SSE 流中空闲超时（StreamHTTP 无总时长上限，靠本字段兜底
	// 「上游中途卡死」）。<=0 表示禁用（测试用）。
	IdleTimeout time.Duration

	// Function 对话/模型列表接口的 function 值（solo_work_lite / solo_agent）。
	Function string
	// PricingFunctions 定价接口 functions 查询参数（各渠道口径不同）。
	PricingFunctions string
	// PricingChannel 定价条目落库的渠道标签（traework / traecode）。
	PricingChannel string
	// PricingPrimary 渠道主 function，去重时优先（即对话真实扣费的那一组）。
	PricingPrimary string
}

func New() *Client {
	tr := &http.Transport{MaxIdleConns: 100, MaxIdleConnsPerHost: 20, IdleConnTimeout: 90 * time.Second, ResponseHeaderTimeout: 120 * time.Second}
	return &Client{HTTP: &http.Client{Timeout: 120 * time.Second, Transport: tr}, StreamHTTP: &http.Client{Transport: tr}, AgentHost: AgentHost, UgHost: UgHost, OAuthHost: OAuthHost, PricingHost: WorkHost, ClientID: ClientID, CheckinRetryDelay: 8 * time.Second, IdleTimeout: idle.DefaultTimeout, Function: Function, PricingFunctions: PricingFunctionsWork, PricingChannel: "traework", PricingPrimary: PricingPrimaryWork}
}

// NewTraeCode 返回 TraeCode（代码版）专用 Client：function=solo_agent。
//
// TraeCode 与 TraeWork 是**同一上游的不同 function**，账号体系相同，
// 因此二者共用一份凭证（调用方应共享同一个 pool，避免 refresh token 轮换冲突）；
// 差异只在 function 与定价分组口径。
func NewTraeCode() *Client {
	c := New()
	c.Function = FunctionCode
	c.PricingFunctions = PricingFunctionsCode
	c.PricingChannel = "traecode"
	c.PricingPrimary = PricingPrimaryCode
	return c
}

// pricingBase 定价接口基址（未显式设置时回落到 WorkHost）。
func (c *Client) pricingBase() string {
	if c.PricingHost != "" {
		return c.PricingHost
	}
	return WorkHost
}

func (c *Client) agentBase() string { return c.AgentHost }
func (c *Client) ugBase() string    { return c.UgHost }
func (c *Client) oauthBase() string { return c.OAuthHost }

// maxJSONBody doJSON 单次响应的读取上限。
//
// 上限的意义不是「够不够用」，而是给上游异常返回一个内存上界。目录接口
// （get_detail_param）在 solo_agent 下实测 1.28MB，而旧上限 1MB 会把 JSON
// 截成半截、解析失败后静默回退静态兜底表（面板只显示 16 个模型，issue #41）。
const maxJSONBody = 8 << 20

func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// 多读 1 字节用于判定是否被截断：读满 maxJSONBody+1 说明上游响应超限，
	// 此时显式报错——否则半截 JSON 会被 json.Unmarshal 误报成语法错误（issue #41）。
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxJSONBody+1))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &provider.Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	if len(raw) > maxJSONBody {
		return nil, fmt.Errorf("traework: response body exceeds %d bytes", maxJSONBody)
	}
	return raw, nil
}

func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	oldRefresh := a.RefreshToken
	log.Printf("traework refresh start uid=%s", a.UID)
	if strings.TrimSpace(a.RefreshToken) == "" {
		err := fmt.Errorf("no refreshToken")
		log.Printf("traework refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	host := a.ApiHost
	if host == "" {
		host = c.oauthBase()
	}
	body := map[string]any{"ClientID": c.ClientID, "RefreshToken": a.RefreshToken, "ClientSecret": "-", "UserID": ""}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, host+EpExchange, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	OAuthHeaders(req)
	data, err := c.doJSON(req)
	if err != nil {
		log.Printf("traework refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	var resp struct {
		Result struct {
			Token               string `json:"Token"`
			TokenExpireAt       int64  `json:"TokenExpireAt"`
			TokenExpireDuration int64  `json:"TokenExpireDuration"`
			RefreshToken        string `json:"RefreshToken"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		err = fmt.Errorf("exchange parse: %w", err)
		log.Printf("traework refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	if resp.Result.Token == "" {
		err := fmt.Errorf("refresh_failed: no token in response — re-login required")
		log.Printf("traework refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	a.AccessToken = resp.Result.Token
	if resp.Result.RefreshToken != "" {
		a.RefreshToken = resp.Result.RefreshToken
	}
	if resp.Result.TokenExpireAt > 0 {
		a.ExpiresAt = normalizeExpiresAt(resp.Result.TokenExpireAt)
	} else if resp.Result.TokenExpireDuration > 0 {
		d := time.Duration(resp.Result.TokenExpireDuration)
		if resp.Result.TokenExpireDuration > 1e9 { // 上游通常是毫秒
			d *= time.Millisecond
		} else {
			d *= time.Second
		}
		a.ExpiresAt = time.Now().Add(d).Unix()
	}
	log.Printf("traework refresh success uid=%s refresh_rotated=%t expires_at=%d", a.UID, a.RefreshToken != oldRefresh, a.ExpiresAt)
	return nil
}

func normalizeExpiresAt(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	req, err := http.NewRequest(http.MethodPost, c.agentBase()+EpChat, bytes.NewReader(PrepareBody(body, c.Function)))
	if err != nil {
		return nil, 0, nil, err
	}
	SOLOHeaders(req, a, true)
	req, cancel := idle.WithCancel(req)
	hc := c.HTTP
	if c.StreamHTTP != nil {
		hc = c.StreamHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		cancel()
		log.Printf("traework chat_stream uid=%s: transport error: %v", a.UID, err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		cancel()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("traework chat_stream uid=%s: upstream %d %s body=%s", a.UID, resp.StatusCode, kind, truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, nil
	}
	// 成功分支：cancel 所有权交给 idle.Monitor（其 Close 会 cancel；静默超时也会 cancel）。
	return idle.Monitor(resp.Body, c.IdleTimeout, cancel), resp.StatusCode, nil, nil
}

func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	// traework 上游 llm_utils_chat 强制 stream=true（见 PrepareBody），
	// 所有模型均为流式模式；非流式请求由本地 Aggregate() 缓冲 SSE 后聚合。
	//
	// 双池合并：get_detail_param 的目录不保证按 function 过滤，而 chat 按
	// function 选池——只拉本渠道池会出现「列表里有、调用 4001」的错位。
	// 故两个池都拉一遍（先本渠道池、后另一池），按 config_name 先到先得
	// 去重，并把每个模型记入其实际归属池（PrepareBody 据此覆盖 function）。
	// 本渠道池失败照旧报错；另一池失败只记日志、用本渠道池结果继续。
	seen := make(map[string]bool, 64)
	out := make([]provider.ModelInfo, 0, 64)
	// 顺序固定：本渠道池先到先得（归属登记与去重都吃顺序），不能用 map 遍历。
	pools := []string{c.Function, otherFunction(c.Function)}
	for i, fn := range pools {
		primary := i == 0
		list, err := c.fetchModelsForPool(a, fn)
		if err != nil {
			if primary {
				return nil, err
			}
			log.Printf("traework models: %s 池拉取失败（继续用 %s 池结果）: %v", fn, c.Function, err)
			continue
		}
		for _, cfg := range list {
			name := strings.TrimSpace(cfg.ConfigName)
			if name == "" || seen[name] {
				continue
			}
			// 跳过自定义模型：部分模型上游不返回 is_custom_model，直接用前缀匹配兜底
			if cfg.DisplayConfig.IsCustomModel || strings.HasPrefix(name, "custom_model_") {
				log.Printf("traework skip custom model: %s", name)
				continue
			}
			seen[name] = true
			recordModelFunc(name, fn)
			ctx, maxTok, images, fromAPI := cfg.metaOf()
			out = append(out, provider.ModelInfo{
				ID:             name,
				Name:           cfg.DisplayConfig.DisplayName,
				ContextWindow:  ctx,
				MaxTokens:      maxTok,
				SupportsImages: images,
				ContextFromAPI: fromAPI,
			})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	return out, nil
}

// fetchModelsForPool 拉取单个 function 池的模型配置（mode_type=nil 返回全部，
// 按 config_name 去重避免流式/非流式重复——池内去重，跨池去重在 FetchModels）。
func (c *Client) fetchModelsForPool(a *auth.Auth, function string) ([]poolEntry, error) {
	body := map[string]any{"function": function, "config_names": nil, "need_prompt": false, "current_config_info": nil, "poly_prompt": true, "mode_type": nil, "agent_type": nil}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, c.agentBase()+EpModels, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	SOLOHeaders(req, a, false)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		ConfigInfoList []poolEntry `json:"config_info_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	return resp.ConfigInfoList, nil
}

// poolEntry 单池目录条目（get_detail_param）。
//
// 上游实测（2026-10-13，HTTP 200/1.35MB/71 条）下发的元数据远多于本工具此前的解析：
//   - context_window_tokens.dev / .max —— 上下文档位（两档；部分模型 max 缺省甚至 dev 缺省）
//   - display_config.multimodal         —— 图像输入能力
//   - model_detail_list[].max_tokens    —— 输出上限
//   - display_config.model_capability   —— reasoning_model / chat_model
//
// 此前只取 config_name + display_name，导致 traework/traecode 全部 118 个模型在
// /v1/models 上既无 context_length 也无 max_output_tokens、图片能力恒为 false。
// 与官方池「模型面绑死上游」的口径一致：这里只补元数据，不增删模型。
//
// 两个容量字段用 json.RawMessage 承接（同 qodercn.parseContextConfig 的手法）：它们
// 只是展示用元数据，若上游把形状从对象改成标量/数组，强类型结构会让**整张目录**
// 解析失败（fetchModelsForPool 报错 → 本渠道池整体拉不到）。形状突变时只丢元数据。
type poolEntry struct {
	ConfigName    string          `json:"config_name"`
	ContextWindow json.RawMessage `json:"context_window_tokens"` // 形状 {"dev":n,"max":n}
	DisplayConfig struct {
		DisplayName   string `json:"display_name"`
		IsCustomModel bool   `json:"is_custom_model"` // 自定义模型（第三方代理）需额外授权
		Multimodal    bool   `json:"multimodal"`      // 图像输入能力（上游声明）
	} `json:"display_config"`
	ModelDetailList json.RawMessage `json:"model_detail_list"` // 形状 [{"max_tokens":n},...]
}

// metaOf 把上游池条目的元数据折算为 ModelInfo 的容量/能力字段。
//
// 口径（对齐 provider.ModelInfo 的「不编造」原则）：
//  1. ContextWindow 取 max 档；max 为 0 时回落 dev；两者皆 0 则**不填**（保持未知，
//     消费方据 ContextFromAPI 判断来源，面板不把未知当确定值展示）。
//  2. MaxTokens 取 model_detail_list[0].max_tokens（上游每条模型组只有一组参数）；
//     为 0 则**不填**。
//  3. SupportsImages 直接取 display_config.multimodal —— 上游明确声明的布尔值，
//     与 workbuddyai 的 imageOK() 同源语义（已确认 true 才置 true）。
//  4. ContextFromAPI 仅在确实解析到上游容量字段时置 true。
//
// 两处 Unmarshal 失败（含字段缺失/为 null/形状突变）一律静默跳过：元数据是增量信息。
func (e *poolEntry) metaOf() (ctx, maxTok int64, images, fromAPI bool) {
	var cw struct {
		Dev int64 `json:"dev"` // 开发档（通常 200000）
		Max int64 `json:"max"` // 最大档（可能缺省为 0）
	}
	if json.Unmarshal(e.ContextWindow, &cw) == nil {
		ctx = cw.Max
		if ctx == 0 {
			ctx = cw.Dev
		}
	}
	var dl []struct {
		MaxTokens int64 `json:"max_tokens"`
	}
	if json.Unmarshal(e.ModelDetailList, &dl) == nil && len(dl) > 0 && dl[0].MaxTokens > 0 {
		maxTok = dl[0].MaxTokens
	}
	images = e.DisplayConfig.Multimodal
	fromAPI = ctx > 0 || maxTok > 0
	return
}

// FetchModelPricing 从 /api/remote/v1/models 拉取模型积分倍率。
// 按 config_name 去重，解析 features.consumption_rate.rate。
func (c *Client) FetchModelPricing(a *auth.Auth) ([]provider.ModelPricing, error) {
	url := c.pricingBase() + EpModelsPricing + "?functions=" + c.PricingFunctions + "&show_custom_model=true"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+a.AccessToken)
	req.Header.Set("X-Trae-Client-Type", "web")
	req.Header.Set("X-Trae-User-Timezone", "Asia/Shanghai")
	req.Header.Set("X-Preferenced-Language", "zh-cn")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Referer", "https://work.trae.cn/")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// 8MB 上限（本地补丁 0005）：与 doJSON 同步放宽——模型定价列表随模型数膨胀，防同类截断。
	// 多读 1 字节判定超限：超限时显式报错，而非截成半截后由 json.Unmarshal 误报语法错误（对齐 doJSON）。
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxJSONBody+1))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("trae pricing api status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	if len(raw) > maxJSONBody {
		return nil, fmt.Errorf("traework: pricing response body exceeds %d bytes", maxJSONBody)
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			List []struct {
				Function string `json:"function"`
				Models   []struct {
					Name        string `json:"name"`
					DisplayName string `json:"display_name"`
					Features    string `json:"features"` // JSON 字符串
				} `json:"models"`
			} `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("trae pricing parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("trae pricing api code=%d", env.Code)
	}
	// 去重：同一模型可能出现在多个 function 分组下，且**倍率未必一致**
	// （实测豆包 Seed-2.1-Pro：solo_agent=0.08 / _remote=0.8）。
	// 对话按主 function 计费，故主 function 的结果优先，非主 function 仅作兜底。
	byName := make(map[string]provider.ModelPricing)
	for _, fn := range env.Data.List {
		isPrimary := c.PricingPrimary != "" && fn.Function == c.PricingPrimary
		for _, m := range fn.Models {
			rate := parseTraeFeatures(m.Features)
			if rate <= 0 {
				continue
			}
			if _, seen := byName[m.Name]; !seen || isPrimary {
				byName[m.Name] = provider.ModelPricing{
					Model:   m.Name,
					Channel: c.PricingChannel,
					Rate:    rate,
				}
			}
		}
	}
	out := make([]provider.ModelPricing, 0, len(byName))
	for _, p := range byName {
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("trae pricing api returned empty")
	}
	return out, nil
}

// parseTraeFeatures 解析 features JSON 字符串中的 consumption_rate.rate。
// 若有 discount 则优先取折扣价。
func parseTraeFeatures(features string) float64 {
	if features == "" {
		return 0
	}
	var f struct {
		ConsumptionRate struct {
			Enable bool `json:"enable"`
			Data   struct {
				Rate float64 `json:"rate"`
			} `json:"data"`
		} `json:"consumption_rate"`
		Discount struct {
			Enable bool `json:"enable"`
			Data   struct {
				ConsumptionRate float64 `json:"consumption_rate"`
			} `json:"data"`
		} `json:"discount"`
	}
	if err := json.Unmarshal([]byte(features), &f); err != nil {
		return 0
	}
	// 折扣价优先
	if f.Discount.Enable && f.Discount.Data.ConsumptionRate > 0 {
		return f.Discount.Data.ConsumptionRate
	}
	if f.ConsumptionRate.Enable && f.ConsumptionRate.Data.Rate > 0 {
		return f.ConsumptionRate.Data.Rate
	}
	return 0
}

func (c *Client) CheckinStatus(a *auth.Auth) (checkedIn bool, credits int64, enable bool, err error) {
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpCheckinStatus, bytes.NewReader([]byte("{}")))
	if err != nil {
		return false, 0, false, err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		log.Printf("traework checkin status failed uid=%s err=%v", a.UID, err)
		return false, 0, false, err
	}
	var resp struct {
		CheckedIn bool   `json:"checked_in"`
		Credits   int64  `json:"credits"`
		Enable    bool   `json:"enable"`
		Code      int    `json:"code"`
		Message   string `json:"message"`
		Msg       string `json:"msg"`
		Success   *bool  `json:"success"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		err = fmt.Errorf("checkin status parse: %w", err)
		log.Printf("traework checkin status failed uid=%s err=%v", a.UID, err)
		return false, 0, false, err
	}
	if resp.Code != 0 {
		err := fmt.Errorf("checkin status code=%d msg=%s", resp.Code, checkinResponseMessage(resp.Message, resp.Msg))
		log.Printf("traework checkin status failed uid=%s err=%v", a.UID, err)
		return false, 0, false, err
	}
	if resp.Success != nil && !*resp.Success {
		err := fmt.Errorf("checkin status failed: %s", checkinResponseMessage(resp.Message, resp.Msg))
		log.Printf("traework checkin status failed uid=%s err=%v", a.UID, err)
		return false, 0, false, err
	}
	log.Printf("traework checkin status uid=%s checked_in=%t credits=%d enable=%t", a.UID, resp.CheckedIn, resp.Credits, resp.Enable)
	return resp.CheckedIn, resp.Credits, resp.Enable, nil
}

// CheckinClaim 领取签到额度。
//
// ## 为什么要换设备号重试
//
// claim 接口是**按设备号**判定的，而额度发放是**按账号**的。这两者不一致时，
// 一个当天本该领到额度的账号会被连带拒绝：
//
//	9074 = 瞬时限流（不是「设备号未注册」）—— 换号或稍后重试可通过
//	9095 = 「当前设备今日已签」—— 换一个新号即可正常入账
//
// 9095 最容易踩坑：多账号共用同一设备号时（实测 2026-10-12，
// 两个账号都是 44842564…），先签的那个用掉当天额度，
// 后签的会**连撞 9095、一分也领不到**，而账号本身完全正常。
//
// 换号重试（claimWithRotatedDevice）是本项目自行补齐的能力，
// 上游协议未直接提供；2026-10-12 接入签到链路。
func (c *Client) CheckinClaim(a *auth.Auth) error {
	log.Printf("traework checkin claim start uid=%s device=%s", a.UID, shortDevice(a.DeviceID))

	// 前置额度：用于判定「上游返回 code=0 时是否真的入账」。
	// 必须在发任何请求**之前**取，否则已经入账了再取就是后置值。
	// 取失败不阻断 —— 额度接口偶发失败不该让签到失败，此时放弃入账校验。
	prefUsage, prefOK := int64(0), false
	if v, err := c.UserEntUsage(a); err == nil {
		prefUsage, prefOK = v, true
	} else {
		log.Printf("traework checkin claim uid=%s 取前置额度失败（跳过入账校验）err=%v", a.UID, err)
	}

	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpCheckinClaim, bytes.NewReader([]byte("{}")))
		if err != nil {
			log.Printf("traework checkin claim failed uid=%s err=%v", a.UID, err)
			return err
		}
		UgHeaders(req, a)
		data, err := c.doJSON(req)
		if err != nil {
			log.Printf("traework checkin claim failed uid=%s err=%v", a.UID, err)
			return err
		}
		code, msg, success, err := parseCheckinResponse(data)
		if err != nil {
			log.Printf("traework checkin claim failed uid=%s err=%v", a.UID, err)
			return err
		}

		// 9074 = 瞬时限流：原地等 CheckinRetryDelay 后**用同一设备号**再试一次。
		//
		// 为什么只等一次就转换号：9074 是限流而非「号非法」，等一会儿有可能过；
		// 但若仍不通，继续用同一个号反复试是浪费（换个号往往立刻就通）。
		// attempt==1 时直接落到下面的换号分支，不再原地重试。
		if code == CheckinRateLimitCode && attempt == 0 {
			if delay := c.CheckinRetryDelay; delay > 0 {
				log.Printf("traework checkin claim rate-limited uid=%s code=9074 retry_after=%s", a.UID, delay)
				time.Sleep(delay)
				continue
			}
		}
		if code == CheckinRateLimitCode {
			log.Printf("traework checkin claim 9074 uid=%s device=%s（瞬时限流，尝试换号重试）",
				a.UID, shortDevice(a.DeviceID))
			if ok, gained := c.claimWithRotatedDevice(a); ok {
				log.Printf("traework checkin claim 9074-rotated-success uid=%s gained=%d", a.UID, gained)
				return nil
			}
			return fmt.Errorf("checkin claim rate limited after rotation: code=%d msg=%s", code, msg)
		}

		// 9095：「当前设备今日已签」—— 设备维度的拒绝。
		// 若该账号当天确实还没领到额度，换一个新设备号即可入账。
		if code == CheckinAlreadyClaimedCode {
			log.Printf("traework checkin claim 9095 uid=%s device=%s（尝试换号重试）",
				a.UID, shortDevice(a.DeviceID))
			if ok, gained := c.claimWithRotatedDevice(a); ok {
				log.Printf("traework checkin claim 9095-rotated-success uid=%s gained=%d（换设备号后成功）",
					a.UID, gained)
				return nil
			}
			// 换号仍未入账 → 今日确实已领（claimWithRotatedDevice 内部会回读额度确认）
			return fmt.Errorf("checkin already claimed: %s", msg)
		}

		if code != 0 {
			err := fmt.Errorf("checkin claim code=%d msg=%s", code, msg)
			log.Printf("traework checkin claim failed uid=%s err=%v", a.UID, err)
			return err
		}
		if success != nil && !*success {
			err := fmt.Errorf("checkin claim failed: %s", msg)
			log.Printf("traework checkin claim failed uid=%s err=%v", a.UID, err)
			return err
		}
		log.Printf("traework checkin claim response uid=%s code=%d msg=%s device=%s",
			a.UID, code, msg, shortDevice(a.DeviceID))
		// ⚠️ code==0 **不等于**入账：当天已领过额度的账号换号后同样返回 0。
		// 多账号共用设备号时，这会造成「显示已签到、实际一分没领」的静默失败
		//（2026-10-12 事故形态）。故必须以**额度是否真的增加**为准。
		if !prefOK {
			return nil // 前置额度没取到，无法判定，采信 code
		}
		after, aerr := c.UserEntUsage(a)
		if aerr != nil {
			// 上游已明确 code=0，此时把额度查询失败当成「没领到」会误伤正常账号。
			log.Printf("traework checkin claim uid=%s code=0 但取后置额度失败（采信 code）err=%v", a.UID, aerr)
			return nil
		}
		if after > prefUsage {
			log.Printf("traework checkin claim uid=%s 入账确认：%d -> %d（+%d）",
				a.UID, prefUsage, after, after-prefUsage)
			return nil
		}
		log.Printf("traework checkin claim uid=%s code=0 但额度未变（%d -> %d），判为今日已领",
			a.UID, prefUsage, after)
		return fmt.Errorf("checkin already claimed: %s", msg)
	}
	return fmt.Errorf("checkin claim rate limited: code=%d", CheckinRateLimitCode)
}

// claimWithRotatedDevice 在原设备号被拒（9074/9095）后，用**多个新生成的合法设备号**
// 依次重试 claim，直到**额度真的增加**为止。
//
// ## 判定依据是「额度是否真的增加」，而不是 code==0
//
// 实测：已领过的账号换号后 claim 仍返回 code=0，但额度纹丝不动。
// 若以 code==0 为成功判据，会把「今天确实已经领过」误报成「刚领到」——
// 上层据此显示的积分增量会是假的。
//
// ## 为什么要多次重试
//
// 换号请求本身有一定概率撞上 9074（瞬时限流）。只试一次正好撞上就放弃，
// 会让本可成功的账号当天签不到（v0.6.9 实测踩过：第 1 次撞 9074 后放弃，
// 同一天手工重试随机号连续多次都是 code=0 成功 +100）。
//
// ## 不污染凭据
//
// 用 defer 还原 a.DeviceID —— 本次调用只在请求头里临时换号，
// 不写回 auth 文件（否则每次签到都换一个号，凭据就失去意义了）。
//
// 返回 (是否真的入账, 增加的额度)。
func (c *Client) claimWithRotatedDevice(a *auth.Auth) (bool, int64) {
	before, err := c.UserEntUsage(a)
	if err != nil {
		log.Printf("traework rotate-claim uid=%s 取前置额度失败 err=%v", a.UID, err)
		return false, 0
	}

	orig := a.DeviceID
	defer func() { a.DeviceID = orig }() // 不污染凭证

	var lastCode int
	var lastMsg string
	for i := 0; i < maxRotateAttempts; i++ {
		alt := randDeviceID()
		a.DeviceID = alt

		req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpCheckinClaim, bytes.NewReader([]byte("{}")))
		if err != nil {
			return false, 0
		}
		UgHeaders(req, a)
		data, err := c.doJSON(req)
		if err != nil {
			log.Printf("traework rotate-claim uid=%s alt=%s 第 %d 次请求失败 err=%v",
				a.UID, shortDevice(alt), i+1, err)
			continue
		}
		code, msg, success, perr := parseCheckinResponse(data)
		if perr != nil {
			log.Printf("traework rotate-claim uid=%s alt=%s 第 %d 次响应解析失败 err=%v",
				a.UID, shortDevice(alt), i+1, perr)
			continue
		}
		lastCode, lastMsg = code, msg

		// 9074 = 瞬时限流：换个号（或稍后）再试，**不是**终止条件。
		if code == CheckinRateLimitCode {
			log.Printf("traework rotate-claim uid=%s alt=%s 第 %d 次 code=9074（瞬时限流，继续换号）msg=%s",
				a.UID, shortDevice(alt), i+1, msg)
			continue
		}
		// 9095 = 该新号也被判「今日已签」，再换。
		if code == CheckinAlreadyClaimedCode {
			log.Printf("traework rotate-claim uid=%s alt=%s 第 %d 次 code=9095（继续换号）",
				a.UID, shortDevice(alt), i+1)
			continue
		}
		if code != 0 || (success != nil && !*success) {
			log.Printf("traework rotate-claim uid=%s alt=%s 第 %d 次 code=%d msg=%s（未通过）",
				a.UID, shortDevice(alt), i+1, code, msg)
			continue
		}

		// code==0 仍不代表入账 —— 必须以额度变化为准（见函数头说明）。
		after, err := c.UserEntUsage(a)
		if err != nil {
			log.Printf("traework rotate-claim uid=%s 取后置额度失败（保守判为未入账）err=%v", a.UID, err)
			return false, 0
		}
		if after <= before {
			log.Printf("traework rotate-claim uid=%s alt=%s code=0 但额度未变（%d -> %d），判为今日已领",
				a.UID, shortDevice(alt), before, after)
			return false, 0
		}
		log.Printf("traework rotate-claim uid=%s alt=%s 第 %d 次入账成功：%d -> %d（+%d）",
			a.UID, shortDevice(alt), i+1, before, after, after-before)
		return true, after - before
	}

	log.Printf("traework rotate-claim uid=%s 尝试 %d 个设备号均未成功（最后 code=%d msg=%s）",
		a.UID, maxRotateAttempts, lastCode, lastMsg)
	return false, 0
}

// randDeviceID 生成一个格式合法的 16 位数字设备号。
//
// 实测该接口只校验「16 位数字」这一格式，不校验是否为本机注册号 ——
// 随机号能让当天未领额度的账号成功入账。用 crypto/rand 保证分布良好，
// 并用固定数字表把随机字节映射到 0-9，避免产生前导 0 之外的异常字符。
func randDeviceID() string {
	var b [16]byte
	const digits = "0123456789"
	out := make([]byte, 16)
	for i := 0; i < 16; i++ {
		if _, err := rand.Read(b[i : i+1]); err != nil {
			// crypto/rand 失败是极端情况；用时间兜底保证格式合法，
			// 设备号本身不承载安全性（此处只要求「看起来像合法设备号」）。
			out[i] = digits[time.Now().UnixNano()%10]
			continue
		}
		out[i] = digits[int(b[i])%10]
	}
	return string(out)
}

// shortDevice 截断设备号用于日志（设备号属半敏感信息，不打全）。
func shortDevice(s string) string {
	if len(s) <= 6 {
		return s
	}
	return s[:6] + "…"
}

func (c *Client) DailyCheckin(a *auth.Auth) error {
	log.Printf("traework checkin start uid=%s", a.UID)
	checked, _, enable, err := c.CheckinStatus(a)
	if err != nil {
		return err
	}
	if checked {
		log.Printf("traework checkin already uid=%s", a.UID)
		return fmt.Errorf("已签到")
	}
	if !enable {
		err := fmt.Errorf("checkin disabled")
		log.Printf("traework checkin rejected uid=%s err=%v", a.UID, err)
		return err
	}
	if err := c.CheckinClaim(a); err != nil {
		return err
	}
	checked, _, _, err = c.CheckinStatus(a)
	if err != nil {
		return fmt.Errorf("checkin verification: %w", err)
	}
	if !checked {
		err := fmt.Errorf("checkin verification failed: checked_in=false")
		log.Printf("traework checkin failed uid=%s err=%v", a.UID, err)
		return err
	}
	log.Printf("traework checkin verified uid=%s", a.UID)
	return nil
}

func parseCheckinResponse(data []byte) (code int, msg string, success *bool, err error) {
	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
		Success *bool  `json:"success"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, "", nil, fmt.Errorf("checkin response parse: %w", err)
	}
	return resp.Code, checkinResponseMessage(resp.Message, resp.Msg), resp.Success, nil
}

func checkinResponseMessage(message, msg string) string {
	if strings.TrimSpace(message) != "" {
		return strings.TrimSpace(message)
	}
	return strings.TrimSpace(msg)
}

// UserResource 查询账号当前可花费积分余额（= UserEntUsage 的可消耗口径，仅 ep=0 池）。
func (c *Client) UserResource(a *auth.Auth) (remain int64, err error) { return c.UserEntUsage(a) }

// UserEntUsage 查询可消耗余额（pool 路由依据）。
// 内部走 fetchEntUsage：只累加 available_endpoint==0 的包，ep=1 官方客户端专用池不计入。
func (c *Client) UserEntUsage(a *auth.Auth) (remain int64, err error) {
	// 网页版 web_user_ent_usage：require_usage=true 返回每个包的 usage.credits_amount（实际用量）。
	_, remain, err = c.fetchEntUsage(a)
	return remain, err
}

// entPackage 上游权益包条目（UserEntUsage / UserResourceDetail 共用结构）。
type entPackage struct {
	EntitlementBaseInfo struct {
		// AvailableEndpoint 可用端点：0=通用池，1=官方客户端专用池。
		// 2026-09-23 起上游不再下发 ep=1（专用池也标成 0），该字段仅作历史兜底。
		AvailableEndpoint int `json:"available_endpoint"`
		// ProductID 商品 ID：209=200 档每日签到（官方客户端专用，本工具扣不到），
		// 208=150 档每日签到（通用）、221=每月登录（通用）。这是当前唯一能区分
		// 「专用/通用」的稳定判据（2026-09-23 三账号实测：209 的 used 恒为 0）。
		ProductID int `json:"product_id"`
		Quota     struct {
			CreditsLimit float64 `json:"credits_limit"`
		} `json:"quota"`
		// EntitlementID 条目唯一标识（如 "340864129538"/"checkin_20260817_..."），ledger 差分对账用。
		EntitlementID string `json:"entitlement_id"`
		PackageName   string `json:"package_name"`
		PackageType   string `json:"package_type"`
	} `json:"entitlement_base_info"`
	DisplayDesc string `json:"display_desc"`
	GroupName   string `json:"group_name"`
	GroupType   int    `json:"group_type"`
	// ExpireTime 到期 Unix 秒；上游对无到期条目下发 0。
	ExpireTime int64 `json:"expire_time"`
	Usage      struct {
		CreditsAmount float64 `json:"credits_amount"`
	} `json:"usage"`
}

// usable 判定该包是否属于本工具可消耗的额度池。
//
// 判据（2026-09-26 更新，issue #44）：只有 available_endpoint==1（上游显式声明的
// 专用端点池）不可用，其余一律可消耗。
//
// 历史沿革：
//   - 2026-09-18：专用池下发 ep=1，判据 = ep==0；
//   - 2026-09-23（f6f41a4）：上游不再下发 ep=1，改判 pid==209；
//   - 2026-09-26（issue #44）：pid==209 判据被证伪——pid=209 是「200 档每日签到」，
//     上游已把它升级为通用积分。本仓自有日志铁证：2026-09-23 15:20–16:10 该账号
//     连发 99 次对话，期间 remain（208/221 池）纹丝不动停在 3086，而「不可用」小计
//     从 2200 降到 1846（-354，正是那 99 次对话的消耗量）——
//     即**扣费实际就发生在这个被判为不可用的池上**。把它排除只会让
//     pool 按虚低的余额选号、面板把真实可用的积分标成「不可用」。
//     故撤回 pid==209 判据，仅保留 ep==1 作历史兜底（上游若重新显式声明则仍尊重）。
func (p entPackage) usable() bool {
	return p.EntitlementBaseInfo.AvailableEndpoint != 1
}

// fetchEntUsage 调用上游积分接口，返回全部条目 + 可消耗余额（仅 usable() 判定为可用的包）。
// 不可消耗余额由调用方对条目按 Usable 标记汇总（provider.Summarize），本函数不重复算。
//
// 可用性判据见 entPackage.usable：仅 available_endpoint==1 不可用（issue #44）。
// 早期实现用 group_type!=1 判定，会把「用户福利」等误计入；后改为
// available_endpoint==0，2026-09-23 起该字段也失效（专用池被标成 0），
// 一度改用 product_id==209，2026-09-26 证伪后撤回（详见 usable 注释）。
func (c *Client) fetchEntUsage(a *auth.Auth) ([]entPackage, int64, error) {
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpEntUsage, bytes.NewReader([]byte(`{"require_usage":true}`)))
	if err != nil {
		return nil, 0, err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, 0, err
	}
	var resp struct {
		UserEntitlementPackList []entPackage `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, 0, fmt.Errorf("ent usage parse: %w", err)
	}
	var usable int64
	for _, p := range resp.UserEntitlementPackList {
		if p.usable() {
			usable += packRemain(p)
		}
	}
	return resp.UserEntitlementPackList, usable, nil
}

// packRemain 单个权益包的剩余额度（limit - used，负值钳 0）。
func packRemain(p entPackage) int64 {
	limit := int64(p.EntitlementBaseInfo.Quota.CreditsLimit)
	used := int64(p.Usage.CreditsAmount)
	if remain := limit - used; remain > 0 {
		return remain
	}
	return 0
}

// UserResourceDetail 查询 TraeWork 积分明细（每个套餐条目）。
// 返回的 remain 为可消耗余额（pool 路由依据），items 包含全部条目。
// 每个条目带 expire_at（到期日）与 usable（是否属于本工具可消耗的 ep=0 池）。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	packs, usable, err := c.fetchEntUsage(a)
	if err != nil {
		return 0, nil, err
	}
	items := make([]provider.ResourceItem, 0, len(packs))
	for _, p := range packs {
		// 零额度条目（如免费的 0 限额包）不展示：既无信息量，又会把列表搅得很长。
		if p.EntitlementBaseInfo.Quota.CreditsLimit <= 0 {
			continue
		}
		name := p.GroupName
		if name == "" {
			name = p.DisplayDesc
		}
		if name == "" {
			name = p.EntitlementBaseInfo.PackageName
		}
		if name == "" {
			name = p.EntitlementBaseInfo.PackageType
		}
		if name == "" {
			name = fmt.Sprintf("套餐 (group_type=%d)", p.GroupType)
		}
		items = append(items, provider.ResourceItem{
			Name:     name,
			Total:    int64(p.EntitlementBaseInfo.Quota.CreditsLimit),
			Used:     int64(p.Usage.CreditsAmount),
			Remain:   packRemain(p),
			ExpireAt: unixDate(p.ExpireTime),
			// entitlement_id 是上游稳定标识，供 ledger 差分对账（过期/消耗归因）
			Key:    p.EntitlementBaseInfo.EntitlementID,
			Usable: p.usable(),
		})
	}
	return usable, items, nil
}

// unixDate 把上游 Unix 秒转为 UTC+8 的 YYYY-MM-DD；0/负值（上游未下发到期时间）返回空串。
// unixDate 把 Unix 秒转为 **RFC3339 精确时刻**（UTC+8）；sec<=0（上游表示无到期）返回空串。
//
// 历史：旧实现返回 "YYYY-MM-DD"，丢了时分秒。traework 的每日签到 expire_time
// 实测是次日 "00:00:0x"（零点后几秒），而每月登录积分是 "23:59:59"——两种都有。
// 截断成日期后两者长得一样，面板无法区分「今晚就死」和「明晚才死」
// （2026-10-19 实测）。现在保留完整时刻供剩余时长分桶。
//
// 注：ledger 伪键用 entitlement_id（真 ID，不含日期），故本改动零抖动。
func unixDate(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).In(softRateResetLoc).Format(time.RFC3339)
}

func (c *Client) GetUserInfo(a *auth.Auth) (uid, nickname, enterpriseID string, err error) {
	host := a.ApiHost
	if host == "" {
		host = c.oauthBase()
	}
	body := map[string]any{"ReqSource": "IDE", "IDEVersion": IdeVersion}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, host+EpUserInfo, bytes.NewReader(raw))
	if err != nil {
		return "", "", "", err
	}
	OAuthHeaders(req)
	req.Header.Set("X-Cloudide-Token", a.JWT())
	data, err := c.doJSON(req)
	if err != nil {
		return "", "", "", err
	}
	var resp struct {
		Result struct {
			UserID       string `json:"UserID"`
			ScreenName   string `json:"ScreenName"`
			EnterpriseID string `json:"EnterpriseID"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", "", "", fmt.Errorf("userinfo parse: %w", err)
	}
	return resp.Result.UserID, resp.Result.ScreenName, resp.Result.EnterpriseID, nil
}

func (c *Client) Classify(status int, body string) provider.ErrKind { return Classify(status, body) }
func (c *Client) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	return StreamWithModel(w, r, model)
}
func (c *Client) Aggregate(r io.Reader, model string) (map[string]any, error) {
	return AggregateWithModel(r, model)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
