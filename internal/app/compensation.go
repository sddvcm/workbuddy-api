// compensation.go 面板侧「一键领取额外补偿」：对所有 WorkBuddy 账号批量领取
// DeepSeek 模型服务异常的 500 积分补偿。
//
// ## 为什么单独一个入口
//
// 补偿是**按账号**发放的（2026-10-19 实测：同一批 7 个账号里既有 claimed=false
// 也有 claimed=true），而个人中心一次只能登录一个账号。账号多的用户要逐个
// 登录、逐个点领取，很容易错过 10/22 的截止时间。
//
// ## 与成长任务的区别：同步而非异步
//
// 成长任务一轮几十个请求（含真实对话），必须异步。领取补偿每账号只有两个请求
// （查状态 + 领取），10 个账号也就几秒，同步返回即可，前端不需要轮询。
//
// ## 只对 WorkBuddy 国内版生效
//
// 实测走的是 www.codebuddy.cn 的 billing 域。国际版（workbuddy.ai）是否提供
// 同一端点未经实测，故不纳入 —— 混进来只会得到一堆无意义的报错。
package app

import (
	"log"
	"strings"

	"workbuddy-api/internal/auth"
	"workbuddy-api/internal/pool"
	"workbuddy-api/internal/provider"
	"workbuddy-api/internal/upstream"
)

// compensationClaimer 由支持补偿领取的上游客户端实现（当前为 workbuddy 渠道）。
// 与 growthTaskRunner 同一思路：App 不关心具体是哪个渠道的客户端。
type compensationClaimer interface {
	CompensationStatus(a *auth.Auth) (*upstream.CompensationStatus, error)
	ClaimCompensation(a *auth.Auth) (int64, error)
}

// 领取结果状态码（面板按这个分色/分文案）。
const (
	compClaimed     = "claimed"     // 本次新领取成功
	compAlready     = "already"     // 此前已领取（无需再点）
	compInactive    = "inactive"    // 活动未开启或已结束
	compUnsupported = "unsupported" // 账号所在渠道不提供该活动
	compError       = "error"       // 领取失败
)

// CompensationResult 单个账号的领取结果。
type CompensationResult struct {
	UID     string `json:"uid"`
	Label   string `json:"label"`              // 面板显示名（昵称，缺省用 uid 前 8 位）
	Status  string `json:"status"`             // 见上方状态码
	Credit  int64  `json:"credit"`             // 本次到账积分（already/inactive 为 0）
	EndTime string `json:"end_time,omitempty"` // 活动截止时间（上游回执原样透出）
	Msg     string `json:"msg,omitempty"`      // 失败原因或补充说明
}

// CompensationRun 一次批量领取的汇总（面板渲染 + 日志共用）。
type CompensationRun struct {
	Accounts int                  `json:"accounts"` // 参与账号数
	Claimed  int                  `json:"claimed"`  // 本次新领取
	Already  int                  `json:"already"`  // 此前已领取
	Inactive int                  `json:"inactive"` // 活动未生效
	Error    int                  `json:"error"`    // 失败
	Credit   int64                `json:"credit"`   // 本次合计到账积分
	Results  []CompensationResult `json:"results"`
}

// compTarget 一个待领取账号（渠道运行时 + 账号 + 面板展示名）。
type compTarget struct {
	rt  *Runtime
	a   *auth.Auth
	st  pool.Status
}

// ClaimCompensationAll 批量领取补偿积分。
//
// uid 为空 = 所有 WorkBuddy 国内版账号；指定 uid 则只处理该账号（用于先拿一个
// 账号试水，与成长任务的 scope 语义一致）。
//
// 单个账号失败不中断整批；账号之间不加人为延迟（总共就两个请求，
// 加延迟只会让面板上的按钮看起来卡住）。
func (a *App) ClaimCompensationAll(uid string) CompensationRun {
	targets := a.compensationTargets(uid)
	run := CompensationRun{Accounts: len(targets), Results: make([]CompensationResult, 0, len(targets))}
	for _, tgt := range targets {
		res := a.claimCompensationOne(tgt)
		switch res.Status {
		case compClaimed:
			run.Claimed++
			run.Credit += res.Credit
		case compAlready:
			run.Already++
		case compInactive:
			run.Inactive++
		default:
			run.Error++
		}
		run.Results = append(run.Results, res)
	}
	log.Printf("compensation claim done accounts=%d claimed=%d already=%d inactive=%d error=%d credit=%d",
		run.Accounts, run.Claimed, run.Already, run.Inactive, run.Error, run.Credit)
	return run
}

// claimCompensationOne 单账号：先只读查状态，再按需领取。
func (a *App) claimCompensationOne(tgt compTarget) CompensationResult {
	res := CompensationResult{UID: tgt.a.UID, Label: accountLabel(tgt.a, tgt.st)}
	claimer, ok := tgt.rt.Upstream.(compensationClaimer)
	if !ok {
		res.Status = compUnsupported
		res.Msg = "该渠道不提供补偿领取"
		return res
	}
	st, err := claimer.CompensationStatus(tgt.a)
	if err != nil {
		res.Status = compError
		res.Msg = truncateMsg(err.Error())
		return res
	}
	if st == nil || !st.Active {
		res.Status = compInactive
		res.Msg = "活动未开启或已结束"
		return res
	}
	res.EndTime = st.EndTime
	if st.Claimed {
		// 已领取：不再发写请求（重复领取只会拿到业务错误，白污染日志）。
		res.Status = compAlready
		res.Credit = st.CreditNum
		return res
	}
	credit, err := claimer.ClaimCompensation(tgt.a)
	if err != nil {
		res.Status = compError
		res.Msg = truncateMsg(err.Error())
		return res
	}
	if credit <= 0 {
		// 领取回执不带数额时，用状态查询里声明的积分数兜底（实测 credit_num=500）。
		credit = st.CreditNum
	}
	res.Status = compClaimed
	res.Credit = credit
	return res
}

// compensationTargets 收集待领取账号（顺序与面板一致，与 taskTargets 同一套遍历）。
//
// 与 taskTargets 的差别：不做「是否支持成长任务」过滤，改为只收 WorkBuddy
// 国内版 —— 补偿活动挂在 codebuddy.cn 的 billing 域下，别的渠道调用只会 404。
func (a *App) compensationTargets(uid string) []compTarget {
	a.mu.Lock()
	defer a.mu.Unlock()

	if strings.TrimSpace(uid) != "" {
		rt, au := a.findRuntimeAuth(uid)
		if rt == nil || au == nil || rt.Kind != provider.WorkBuddy || rt.Alias {
			return nil
		}
		return []compTarget{{rt: rt, a: au}}
	}

	var out []compTarget
	seen := map[string]bool{}
	for _, kind := range a.orderedChannels() {
		rt := a.runtimes[kind]
		if rt == nil || rt.Alias || rt.Pool == nil || kind != provider.WorkBuddy {
			continue
		}
		for _, s := range rt.Pool.List() {
			if s.Disabled || seen[s.UID] {
				continue
			}
			au := rt.Pool.AuthByUID(s.UID)
			if au == nil {
				continue
			}
			seen[s.UID] = true
			out = append(out, compTarget{rt: rt, a: au, st: s})
		}
	}
	return out
}

// accountLabel 面板显示名：优先账号卡片昵称，缺省用 uid 前 8 位。
func accountLabel(a *auth.Auth, st pool.Status) string {
	if strings.TrimSpace(st.Nickname) != "" {
		return st.Nickname
	}
	if strings.TrimSpace(a.Nickname) != "" {
		return a.Nickname
	}
	if len(a.UID) >= 8 {
		return a.UID[:8]
	}
	return a.UID
}

// truncateMsg 错误文案截断（面板上一行放不下长报文）。
func truncateMsg(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}
