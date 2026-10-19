// models.go 动态模型获取：COSY 签名 GET /algo/api/v2/model/list?Encode=1，
// 拿 chat scene 的 key 列表 → provider.ModelInfo。
// 移植自 qoderwork2api internal/upstream/models.go。
package qoder

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"workbuddy-api/internal/auth"
	"workbuddy-api/internal/provider"
)

// DynamicModel 上游模型条目（chat/assistant/developer 场景同构）。
type DynamicModel struct {
	Key            string  `json:"key"`
	DisplayName    string  `json:"display_name"`
	Enable         bool    `json:"enable"`
	IsDefault      bool    `json:"is_default"`
	IsReasoning    bool    `json:"is_reasoning"`
	IsVL           bool    `json:"is_vl"`
	MaxInputTokens int64   `json:"max_input_tokens"`
	PriceFactor    float64 `json:"price_factor"`
	Format         string  `json:"format"` // 上游声明的协议形态
	Source         string  `json:"source"` // 上游声明来源；model_config.source 即思考总开关
	ContextWindow  int64   `json:"-"`      // context_config 默认档（is_default）的 token_count
	// AvailableWindows context_config 全部档位（升序）；空表示无档位表。
	AvailableWindows []int64 `json:"-"`
}

// 上游未下发时的兜底值（与 qodercn/qodercom 一致）。
const (
	defaultFormat = "openai"
	defaultSource = "system"
)

// contextConfig 形状：{"<label>": {"is_default":bool,"token_count":int}}
type contextConfig map[string]struct {
	IsDefault  bool  `json:"is_default"`
	TokenCount int64 `json:"token_count"`
}

// tokenCount 取上游声明的上下文窗口：**只认标了 is_default 的那一档**。
// 上游实测总是恰好标一档默认，未标默认时不猜——返回 0 让上层回退 max_input_tokens（保守方向）。
// 多个档同时标默认时取最小值（确定性 + 保守），不依赖 map 迭代序。
func (cc contextConfig) tokenCount() int64 {
	var best int64
	for _, cfg := range cc {
		if !cfg.IsDefault || cfg.TokenCount <= 0 {
			continue
		}
		if best == 0 || cfg.TokenCount < best {
			best = cfg.TokenCount
		}
	}
	return best
}

// windows 返回 context_config 全部档位（升序、去重、>0），供选档校验。
func (cc contextConfig) windows() []int64 {
	seen := make(map[int64]struct{}, len(cc))
	out := make([]int64, 0, len(cc))
	for _, cfg := range cc {
		if cfg.TokenCount <= 0 {
			continue
		}
		if _, ok := seen[cfg.TokenCount]; ok {
			continue
		}
		seen[cfg.TokenCount] = struct{}{}
		out = append(out, cfg.TokenCount)
	}
	// 插入排序（档位数 ≤4，无需 sort 包）
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// modelWire 上游模型条目：在 DynamicModel 之外带上 context_config。
// DynamicModel.ContextWindow 是 json:"-"，只能由此处解析后回填。
type modelWire struct {
	DynamicModel
	ContextConfig json.RawMessage `json:"context_config"`
}

// parseContextConfig 解析 context_config → (默认档, 全部档位)。
// 用 RawMessage 承接：形状不符时只损失本字段，不会让整批模型解析失败。
func (w modelWire) parseContextConfig() (int64, []int64) {
	if len(w.ContextConfig) == 0 {
		return 0, nil
	}
	var cc contextConfig
	if err := json.Unmarshal(w.ContextConfig, &cc); err != nil {
		return 0, nil
	}
	return cc.tokenCount(), cc.windows()
}

// parseSceneModels 解析模型列表响应：assistant→developer→chat 三级回退。
// 上游把模型挪场景时（如迁到 assistant）不至于硬失败。
func parseSceneModels(apiResp map[string]json.RawMessage) ([]DynamicModel, error) {
	for _, scene := range []string{"assistant", "developer", "chat"} {
		raw, ok := apiResp[scene]
		if !ok {
			continue
		}
		var wires []modelWire
		if err := json.Unmarshal(raw, &wires); err != nil {
			continue
		}
		enabled := make([]DynamicModel, 0, len(wires))
		for _, w := range wires {
			if !w.Enable || w.Key == "" {
				continue
			}
			m := w.DynamicModel
			m.ContextWindow, m.AvailableWindows = w.parseContextConfig() // 默认档 + 全档位
			enabled = append(enabled, m)
		}
		if len(enabled) > 0 {
			return enabled, nil
		}
	}
	return nil, fmt.Errorf("no enabled models in assistant/developer/chat scenes")
}

// fetchModels 调上游动态模型接口。
// GET 无 body，签名用空串 ""（非 "{}"，后者 403 Signature invalid）。
func (c *Client) fetchModels(a *auth.Auth) ([]DynamicModel, error) {
	dt := a.JWT()
	if dt == "" {
		return nil, fmt.Errorf("no dt- available")
	}
	rawURL := c.gatewayBase() + EpModels
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	sess, err := NewCosySession(a.MachineID, a.MachineToken, a.MachineType, a.Nickname, a.UID, dt, a.RefreshToken)
	if err != nil {
		return nil, fmt.Errorf("cosy session: %w", err)
	}
	if err := sess.ApplyHeaders(req, "", rawURL, a.UID, false, ""); err != nil {
		return nil, fmt.Errorf("cosy headers: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var apiResp map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	return parseSceneModels(apiResp)
}

// FetchModels 实现 provider.Upstream：动态模型 → provider.ModelInfo。
// 客户端名 = display_name 规范化（无 display_name 用 key 兜底）。
// 同时把 客户端名→key 映射与 key→条目表缓存到 Client，供 ChatStream 路由与取 format/source。
func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	dyn, err := c.fetchModels(a)
	if err != nil {
		return nil, err
	}
	mm := make(map[string]string, len(dyn))
	entries := make(map[string]DynamicModel, len(dyn))
	out := make([]provider.ModelInfo, 0, len(dyn))
	for _, m := range dyn {
		name := NormalizeModelName(m.DisplayName)
		if name == "" {
			name = m.Key
		}
		mm[name] = m.Key
		entries[m.Key] = m
		mi := provider.ModelInfo{
			ID:            name,
			Name:          m.DisplayName,
			ContextWindow: 180000,
			// is_vl 即上游的视觉能力声明；is_reasoning 为思考模式。
			SupportsImages:    m.IsVL,
			SupportsReasoning: m.IsReasoning,
		}
		// 上下文窗口：广告宁高勿低——max(档位表) > is_default 档 > max_input_tokens
		// （与 resolveContextWindow 默认档一致，对齐 workbuddy2api 口径）
		if n := len(m.AvailableWindows); n > 0 {
			mi.ContextWindow = m.AvailableWindows[n-1]
			mi.ContextFromAPI = true
		} else if m.ContextWindow > 0 {
			mi.ContextWindow = m.ContextWindow
			mi.ContextFromAPI = true
		} else if m.MaxInputTokens > 0 {
			mi.ContextWindow = m.MaxInputTokens
			mi.ContextFromAPI = true // 接口真实返回
		}
		out = append(out, mi)
	}
	c.setModelEntries(entries, mm)
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	return out, nil
}

// FetchModelPricing 实现 provider.Upstream：Qoder 模型带 price_factor，
// 直接复用 FetchModels 的倍率字段。
func (c *Client) FetchModelPricing(a *auth.Auth) ([]provider.ModelPricing, error) {
	dyn, err := c.fetchModels(a)
	if err != nil {
		return nil, err
	}
	out := make([]provider.ModelPricing, 0, len(dyn))
	for _, m := range dyn {
		name := NormalizeModelName(m.DisplayName)
		if name == "" {
			name = m.Key
		}
		out = append(out, provider.ModelPricing{
			Model:   name,
			Channel: "qoder",
			Rate:    m.PriceFactor,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("pricing api returned empty models")
	}
	return out, nil
}

// NormalizeModelName 把 display_name 转成 OpenAI 风格客户端名：
// 小写、空格/下划线转连字符、保留点号（版本号）、去重连字符。
// "Qwen3.8-Max" → "qwen3.8-max"；"GLM-5.3" → "glm-5.3"。
func NormalizeModelName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	b.Grow(len(s))
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.':
			b.WriteRune(r)
			prevDash = false
		case r == ' ' || r == '_' || r == '-':
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		default:
			b.WriteRune(r)
			prevDash = false
		}
	}
	return strings.Trim(b.String(), "-")
}
