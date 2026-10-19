// Package pool 账号池：内存索引 + 冷却/禁用状态机 + state.json 持久化。
// 挑选策略：healthy 账号中剩余积分最多者。
package pool

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"workbuddy-api/internal/auth"
)

// CoolKind 冷却类型。
type CoolKind int

const (
	CoolHard       CoolKind = iota // 余额不足 → 长冷却
	CoolSoft                       // 429 → 短冷却
	CoolErr                        // 连续错误 → 中冷却
	CoolLowBalance                 // 余额低于平均 60% → 15min 冷却
)

func (k CoolKind) String() string {
	switch k {
	case CoolHard:
		return "hard_credit"
	case CoolSoft:
		return "soft_rate"
	case CoolErr:
		return "error_threshold"
	case CoolLowBalance:
		return "low_balance"
	}
	return "unknown"
}

// Status 单个账号对外暴露的状态（脱敏）。
type Status struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	// Credits 本工具可消耗的积分余额（pool 路由依据）。
	Credits int64 `json:"credits"`
	// ExpiringCredits 可消耗余额中 24h 内（实际口径：到期日≤明天）到期的部分。
	// Pick() 先按临期优先消耗快过期的积分，再按总可用余额排序；仅面板展示。
	// 0 表示无临期或渠道不区分（如 Qoder 不下发到期时间）。
	ExpiringCredits int64 `json:"expiring_credits,omitempty"`
	// UnusableCredits 账号名下有、但本工具用不了的积分（如 TraeWork 的 ep=1 专用池）。
	// 仅用于面板展示，不参与路由；0 表示该渠道不区分或没有此类额度。
	UnusableCredits int64 `json:"unusable_credits,omitempty"`
	// CreditsStale 标记余额口径不可信：state 文件是旧版本格式（无 unusable 字段）
	// 或尚未完成首次成功刷新。UI 应显示「待刷新」而非把旧值/0 当真值。
	// 自动刷新循环首次成功写入后即清除。
	CreditsStale   bool      `json:"credits_stale,omitempty"`
	Cooling        bool      `json:"cooling"`
	Until          time.Time `json:"until,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	Disabled       bool      `json:"disabled"`
	ErrCount       int       `json:"err_count,omitempty"`
	LastCheckinOK  bool      `json:"last_checkin_ok,omitempty"`
	LastCheckinAt  time.Time `json:"last_checkin_at,omitempty"`
	LastCheckinMsg string    `json:"last_checkin_msg,omitempty"`
	// SessionDead 登录态失效（401/登录已失效）：挑号跳过 + 面板「需重新登录」徽标（#134）。
	// 与 Disabled（用户手动停用）分离，避免机器状态混入用户控制项。
	SessionDead       bool      `json:"session_dead,omitempty"`
	SessionDeadReason string    `json:"session_dead_reason,omitempty"`
	// SessionDeadAt 标记**首次**进入失效态的时刻，是「重登后自动清除」的时间锚
	// （clearReloggedLocked 比较 auth 文件 mtime 与它）。已标记时不得刷新成 now，
	// 否则重登后再一次陈旧凭证失败就会把锚顶到文件之后 ⇒ 永不清除（#134 死锁）。
	// 导出到 Status 只有一个用途：让上述约束可被回归测试直接断言。
	SessionDeadAt time.Time `json:"session_dead_at,omitempty"`
	// ModelCooling 账号在个别模型上的独立冷却（429/6004 按模型限，账号整体仍可用）。
	// 仅包含仍处于未来的条目；空 = 无模型级冷却。供路由过滤与面板展示。
	ModelCooling map[string]time.Time `json:"model_cooling,omitempty"`
	// LastUsedAt 最近一次真正发起调用的时间，零值 = 从未调用。仅面板展示。
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
}

type entry struct {
	a        *auth.Auth
	credits  int64
	expiring int64 // 临期（快到期）的可消耗积分，Pick() 第一排序键
	unusable int64
	// creditsStale 余额口径不可信（旧版 state 或从未成功刷新过）。
	// 仅影响面板展示，不参与 Pick() 路由。
	creditsStale bool
	disabled     bool
	reason       string
	until        time.Time
	errCount     int
	// modelCool 模型级冷却：model → 解冻时刻（429/6004 上游按模型限额，
	// 只冷却撞墙的模型，账号对其他模型仍可用）。条目过期后延迟清理。
	modelCool map[string]time.Time

	lastCheckinOK  bool
	lastCheckinAt  time.Time
	lastCheckinMsg string

	// lastUsedAt 最近一次**真正发起调用**的时间，零值 = 从未调用过。
	//
	// 由 NotifyUsed 在请求发出前打点，不是 Pick 时自动记录 —— 挑号不等于
	// 用了它：token 刷新失败、首个模型冷却重试、定价刷新都会调 Pick，却未必
	// 真的把请求送出去。在「发送前」打点才与用户看到的「调用中」一致。
	lastUsedAt time.Time

	sessionDead       bool
	sessionDeadReason string
	sessionDeadAt     time.Time // 标记建立时间：用于「重登后自动清除」的新旧比较
}

// NotifyUsed 记录一次实际调用，供面板显示「调用中」。
//
// 时间窗的意义：调用是流式的（可能持续几十秒），面板不可能精确知道何时结束。
// 故用「最近 N 秒内被打点过」近似为「正在调用」—— 见 InUseWindow。
//
// 轮询策略下顺带推进游标：让「刚用的号」在下一轮里排到最后，实现真正的
// 轮换。若不在此处推进，rrLast 只在 pick 时更新，连续命中同一账号会导致
// 游标实际不前进。
func (p *Pool) NotifyUsed(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	e.lastUsedAt = time.Now()
	if p.strategy == StrategyRoundRobin {
		p.rrLast = uid
	}
}

func (e *entry) healthy(now time.Time) bool {
	if e.disabled || e.sessionDead {
		return false
	}
	if !e.until.IsZero() && now.Before(e.until) {
		return false
	}
	return true
}

// healthyForModel 在 healthy 基础上再过滤模型级冷却：model 为空时退化为 healthy。
func (e *entry) healthyForModel(now time.Time, model string) bool {
	if !e.healthy(now) {
		return false
	}
	if model == "" {
		return true
	}
	t, ok := e.modelCool[model]
	return !ok || !now.Before(t)
}

// stateFile 持久化格式。
// version 用于识别旧版本状态文件：v2.2.0 及之前无 unusable 字段，
// 读入后这些账号的余额口径不可信（pool 会置 creditsStale）。
// 缺失/零值视为 1（保持向后兼容：老文件不报错）。
type stateFile struct {
	Version  int                     `json:"version,omitempty"`
	RRLast   string                  `json:"rr_last,omitempty"` // roundrobin 游标：上次选中的 uid
	Accounts map[string]accountState `json:"accounts"`
}

// stateVersion 当前**余额口径**版本。v2: unusable（可用/不可用拆分）；v3: expiring（临期额度）。
//
// ⚠️ 只在「余额字段的语义/集合发生变化」时才 +1 —— 读入时 `Version < stateVersion`
// 会把所有账号标记为 creditsStale（面板显示「待刷新」）。纯附加字段（如 rr_last）
// **不能**动这个常量，否则每次加个无关字段就会让全部账号的余额口径变成不可信。
const stateVersion = 3

type accountState struct {
	Credits           int64                `json:"credits"`
	Expiring          int64                `json:"expiring,omitempty"`
	Unusable          int64                `json:"unusable,omitempty"`
	Disabled          bool                 `json:"disabled"`
	Reason            string               `json:"reason,omitempty"`
	Until             time.Time            `json:"until,omitempty"`
	ModelCool         map[string]time.Time `json:"model_cooling,omitempty"`
	LastCheckinOK     bool                 `json:"last_checkin_ok,omitempty"`
	LastCheckinAt     time.Time            `json:"last_checkin_at,omitempty"`
	LastCheckinMsg    string               `json:"last_checkin_msg,omitempty"`
	SessionDead       bool                 `json:"session_dead,omitempty"`
	SessionDeadReason string               `json:"session_dead_reason,omitempty"`
	SessionDeadAt     time.Time            `json:"session_dead_at,omitempty"`
}

// Pool 账号池。
type Pool struct {
	mu      sync.RWMutex
	byUID   map[string]*entry
	stateFp string
	// strategy 挑号策略（见 strategy.go）。零值按 StrategyExpire 解释。
	strategy Strategy
	// rrLast roundrobin 游标：上次选中的 uid。落盘，重启后继续往下轮。
	rrLast string
}

// New 构建池；stateFp 非空时尝试加载旧状态。
func New(stateFp string) *Pool {
	p := &Pool{byUID: map[string]*entry{}, stateFp: stateFp, strategy: StrategyExpire}
	if stateFp != "" {
		p.load()
	}
	return p
}

// Add 加入账号；已存在则保留原状态、更新凭证。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[a.UID]; ok {
		e.a = a // 保留 credits/cooling 状态
		return
	}
	p.byUID[a.UID] = &entry{a: a}
}

// SyncToDir 用最新扫描结果对齐池：新账号加入、消失的账号剔除（状态保留）。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	for _, a := range auths {
		seen[a.UID] = true
		if e, ok := p.byUID[a.UID]; ok {
			e.a = a
		} else {
			p.byUID[a.UID] = &entry{a: a}
		}
	}
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
		}
	}
	p.clearReloggedLocked(auths)
}

// clearReloggedLocked 重新登录后（auth 文件被登录流程重写）清除 SessionDead 标记。
// 必要性（#134）：旧版对 session dead 走 Disable，标记被持久化到 state 文件；
// 用户重新登录、RefreshToken 也刷成功了，但池里只换 e.a，标记仍在 ⇒ 账号永不被挑中。
// 判据：auth 文件修改时间晚于「标记建立时间」（无标记建立时间则退化为「文件比标记新」）。
// 调用方须持有 p.mu。
func (p *Pool) clearReloggedLocked(auths []*auth.Auth) {
	for _, a := range auths {
		e, ok := p.byUID[a.UID]
		if !ok || !e.sessionDead {
			continue
		}
		// FilePath 为空（内存构造的凭证）无从判断，保守保留标记。
		if a.FilePath == "" {
			continue
		}
		st, err := os.Stat(a.FilePath)
		if err != nil {
			continue
		}
		if st.ModTime().After(e.sessionDeadAt) {
			e.sessionDead = false
			e.sessionDeadReason = ""
			e.sessionDeadAt = time.Time{}
			log.Printf("session dead cleared by relogin uid=%s", a.UID)
		}
	}
}

// Pick 返回按当前策略挑出的账号；无可用返回 nil。
func (p *Pool) Pick() *auth.Auth {
	return p.PickExcluding(nil)
}

// PickExcluding 同上，但跳过 tried 中的 uid（请求级轮换）。
func (p *Pool) PickExcluding(tried map[string]bool) *auth.Auth {
	return p.PickExcludingForModel(tried, "")
}

// PickForModel 返回对指定模型可用的 healthy 账号（过滤模型级冷却）；
// model 为空时等价 Pick()。供聊天路由使用——429/6004 按模型限额后，
// 同账号其他模型仍可参与选号。
func (p *Pool) PickForModel(model string) *auth.Auth {
	return p.PickExcludingForModel(nil, model)
}

// PickExcludingForModel 同 PickExcluding，但额外跳过「该模型处于模型级冷却」的账号。
//
// 按当前 Strategy 挑号（见 strategy.go）。roundrobin 需要写游标，
// 故这里用写锁而不是基线的读锁 —— 挑选在每次请求的头部发生，锁持有时间极短。
//
// ⚠️ tried 排空后**必须**返回 nil，不能回退到全体账号：调用方
// （server/handler.go 的 MaxRotate 循环）正是靠 nil 判定「已轮完一圈，结束重试」。
// 一旦这里回退，循环会拿到已试过的账号并继续，错误被重复触发且永不退出。
func (p *Pool) PickExcludingForModel(tried map[string]bool, model string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()

	cands := p.candidates(now, tried, model)
	if len(cands) == 0 {
		return nil
	}

	switch p.strategy {
	case StrategyRoundRobin:
		return p.pickRoundRobinLocked(cands)
	case StrategyCredits:
		return p.pickCreditsLocked(cands)
	default: // StrategyExpire（默认）
		return p.pickExpireLocked(cands)
	}
}

// candidates 收集当前可用账号（未禁用、未冷却、不在 tried、模型未冷却）。
// 调用方需持有 p.mu。
func (p *Pool) candidates(now time.Time, tried map[string]bool, model string) []*entry {
	out := make([]*entry, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// pickCreditsLocked 优先积分：可用余额最多者；同分取 UID 较小者（稳定排序，
// 避免同分账号因 map 遍历顺序随机漂移）。
func (p *Pool) pickCreditsLocked(cands []*entry) *auth.Auth {
	best := cands[0]
	for _, e := range cands[1:] {
		if e.credits > best.credits ||
			(e.credits == best.credits && e.a.UID < best.a.UID) {
			best = e
		}
	}
	return best.a
}

// pickExpireLocked 临期优先：临期额度多者先烧（expiring 降序 → credits 降序 → UID）。
//
// 临期>0 的账号恒排在临期=0 之前，即使后者总余额更高——目的是优先烧掉快过期的
// 积分。expiring 仅统计可消耗额度，且 expiring ≤ credits，不会虚高。
//
// 这套比较键是历史默认行为，保留为默认策略以免升级即改变选号结果。
func (p *Pool) pickExpireLocked(cands []*entry) *auth.Auth {
	best := cands[0]
	for _, e := range cands[1:] {
		if e.expiring != best.expiring {
			if e.expiring > best.expiring {
				best = e
			}
			continue
		}
		if e.credits != best.credits {
			if e.credits > best.credits {
				best = e
			}
			continue
		}
		if e.a.UID < best.a.UID {
			best = e
		}
	}
	return best.a
}

// pickRoundRobinLocked 负载均衡：按 UID 排序后从上次选中项的下一个开始轮转。
//
// 游标随请求推进并落盘：重启后继续从上次位置往下轮，而不是从头开始。
func (p *Pool) pickRoundRobinLocked(cands []*entry) *auth.Auth {
	sort.Slice(cands, func(i, j int) bool { return cands[i].a.UID < cands[j].a.UID })
	start := 0
	if p.rrLast != "" {
		for i, e := range cands {
			if e.a.UID == p.rrLast {
				start = (i + 1) % len(cands)
				break
			}
		}
	}
	chosen := cands[start]
	p.rrLast = chosen.a.UID
	p.saveLocked()
	return chosen.a
}

// entryBetter e 是否应排在 b 之前（临期优先策略的比较键，供诊断/测试复用）。
func entryBetter(e, b *entry) bool {
	if e.expiring != b.expiring {
		return e.expiring > b.expiring
	}
	if e.credits != b.credits {
		return e.credits > b.credits
	}
	return e.a.UID < b.a.UID
}

// SetStrategy 切换挑号策略（面板保存后热生效）。
func (p *Pool) SetStrategy(s Strategy) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.strategy == s {
		return
	}
	p.strategy = s
	p.saveLocked()
}

// GetStrategy 返回当前策略。
func (p *Pool) GetStrategy() Strategy {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.strategy == "" {
		return StrategyExpire
	}
	return p.strategy
}

// SetCreditDetail 更新账号积分：usable 为可消耗余额，expiring 为其中临期部分，
// unusable 为账号名下有但本工具用不了的额度（如 TraeWork 的 ep=1 专用池），仅面板展示。
// usable+expiring 是 Pick() 的排序依据。写入即视为余额口径可信，清除 creditsStale。
func (p *Pool) SetCreditDetail(uid string, usable, expiring, unusable int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = usable
		e.expiring = expiring
		e.unusable = unusable
		e.creditsStale = false
	}
	p.saveLocked()
}

// Cooldown 冷却账号至 now+d。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Now().Add(d)
		e.reason = reason
		e.errCount = 0
	}
	p.saveLocked()
}

// ClearPenalty 清除账号的冷却与错误计数（不动 disabled，停用由用户控制）。
// 用途：单账号渠道在启动时自愈历史脏数据——旧版本会把唯一账号
// 因网络抖动/429 冷却，而该渠道无号可轮换，冷却即等于整条渠道下线。
func (p *Pool) ClearPenalty(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		if e.until.IsZero() && e.errCount == 0 {
			return // 无惩罚可清：不触发无意义的落盘
		}
		e.until = time.Time{}
		e.errCount = 0
		if !e.disabled {
			e.reason = ""
		}
	}
	p.saveLocked()
}

// CooldownModel 模型级冷却：只把 uid 账号的指定模型冻到 now+d，账号对其他模型仍可用。
// 上游 429/6004 实测按模型限额（「您也可以切换其他模型继续使用」），账号级冷却粒度
// 过粗会把同号健康模型一起拖死。
// 升级兜底：若该账号已有 ≥1 个模型仍处于冷却中，本次视为「短时间内第二次撞墙」——
// 上游可能实际按账号计时（多模型同时 6004），直接升级为整号冷却到最晚重置点，
// 防止逐模型把每个模型都撞一遍 429 才发现是整号被限。返回是否发生了升级。
func (p *Pool) CooldownModel(uid, model string, d time.Duration, reason string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	now := time.Now()
	if e.modelCool == nil {
		e.modelCool = map[string]time.Time{}
	}
	e.modelCool[model] = now.Add(d)
	// 统计仍处于未来的模型级冷却，顺便清理过期条目
	active, latest := 0, time.Time{}
	for m, t := range e.modelCool {
		if !now.Before(t) {
			delete(e.modelCool, m)
			continue
		}
		active++
		if t.After(latest) {
			latest = t
		}
	}
	escalated := false
	if active >= 2 {
		e.until = latest
		e.reason = reason + "（多模型接连限流，升级整号冷却）"
		e.errCount = 0
		e.modelCool = nil
		escalated = true
	}
	p.saveLocked()
	return escalated
}

// Disable 永久禁用（session 死亡），需人工重登后手工恢复或文件替换。
func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = true
		e.reason = reason
	}
	p.saveLocked()
}

// SetDisabled 设置账号禁用/启用状态。
func (p *Pool) SetDisabled(uid string, d bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = d
		if d {
			e.reason = "手动停用"
		} else {
			e.reason = ""
		}
	}
	p.saveLocked()
}

// ReenableIfCredits 签到后解冻：仅当 remain > 0 且账号处于冷却（非禁用）时恢复。
// expiring 为临期额度小计，unusable 为不可消耗额度小计（仅面板展示）。写入即视为余额口径可信。
func (p *Pool) ReenableIfCredits(uid string, remain, expiring, unusable int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = remain
		e.expiring = expiring
		e.unusable = unusable
		e.creditsStale = false
		if remain > 0 && !e.disabled {
			e.until = time.Time{}
			e.reason = ""
			e.errCount = 0
		}
	}
	p.saveLocked()
}

// RecordCheckin 记录一次签到结果（含错误信息），随 state.json 持久化。
func (p *Pool) RecordCheckin(uid string, ok bool, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// ⚠️ 这里的第二个返回值**不能**命名为 ok：那会遮蔽同名参数 ok bool（issue #157），
	// 使 e.lastCheckinOK 恒写 true、`if ok` 恒成立（每次都清掉 session_dead）、
	// 且 `else if sessionDeadLike(msg)` 成为死代码 ⇒ 失效账号永远不显示「需重新登录」。
	// 全仓同名遮蔽仅此一处（其余 `if e, ok := ...` 所在函数没有名为 ok 的参数）。
	if e, found := p.byUID[uid]; found {
		e.lastCheckinOK = ok
		e.lastCheckinAt = time.Now()
		e.lastCheckinMsg = msg
		if ok {
			// 签到成功 = 登录态有效：清除失效标记（#134）
			e.sessionDead = false
			e.sessionDeadReason = ""
			e.sessionDeadAt = time.Time{}
		} else if sessionDeadLike(msg) {
			// 401/登录失效形态：标记待重登（面板徽标 + 重新登录入口）。
			// sessionDeadAt 只在「首次进入失效态」时写：它是重登后自动清除的时间锚
			// （clearReloggedLocked 比较 auth 文件 mtime），若每次失败都刷新成 now，
			// 重登后再来一次陈旧凭证的失败就会把锚推到文件之后 ⇒ 永不清除（#134 死锁）。
			// 与 MarkSessionDead 同口径。
			if !e.sessionDead {
				e.sessionDeadAt = time.Now()
			}
			e.sessionDead = true
			e.sessionDeadReason = msg
		}
	}
	p.saveLocked()
}

// sessionDeadLike 判断签到失败信息是否为登录态失效形态（401/登录已失效/网关 401 页）。
func sessionDeadLike(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "401") ||
		strings.Contains(lower, "unauthorized") ||
		strings.Contains(msg, "登录已失效") ||
		strings.Contains(msg, "登录已过期")
}

// MarkSessionDead 标记账号登录态失效（401/登录已失效）：挑号阶段跳过、面板显示
// 「需重新登录」徽标与一键重新登录入口（#134）。与 Disable（用户手动停用）分离，
// 避免机器状态混入用户控制项后用户看不出该做什么。
//
// 已标记时**也要更新原因**（issue #157 附带瑕疵）：失效原因会演进
// （如从 `refresh: xxx` 变成上游 `12153:refresh token is invalid`），旧实现直接 return
// 会让面板 tooltip 永远显示首次的旧原因。但 `sessionDeadAt` **不刷新**——它是
// 「重登后自动清除」的时间锚（clearReloggedLocked 比较 auth 文件 mtime 与它）：
// 若反复刷新成 now，用户重登后再被陈旧内存凭证标一次，就会变成「文件比标记旧」
// 而永不清除，重回路 #134 的死锁。
func (p *Pool) MarkSessionDead(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	if !e.sessionDead {
		e.sessionDeadAt = time.Now()
	}
	changed := !e.sessionDead || e.sessionDeadReason != reason
	e.sessionDead = true
	e.sessionDeadReason = reason
	p.saveLocked()
	if changed {
		log.Printf("session dead marked uid=%s reason=%s", uid, reason)
	}
}

// ClearSessionDead 清除登录态失效标记（重新登录成功/签到或请求成功后回到可用态）。
func (p *Pool) ClearSessionDead(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok && e.sessionDead {
		e.sessionDead = false
		e.sessionDeadReason = ""
		e.sessionDeadAt = time.Time{}
		p.saveLocked()
		log.Printf("session dead cleared uid=%s", uid)
	}
}

// Remove 从池中移除账号（内存 + 状态文件）；auth 文件删除由调用方负责。
func (p *Pool) Remove(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.byUID, uid)
	p.saveLocked()
}

// NoteError 记录一次非余额/非 429 错误；达到 threshold 自动冷却 d 时长。
func (p *Pool) NoteError(uid string, threshold int, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount++
		if e.errCount >= threshold {
			e.until = time.Now().Add(d)
			e.reason = "consecutive errors"
			e.errCount = 0
		}
	}
	p.saveLocked()
}

// NoteSuccess 成功请求重置错误计数。
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount = 0
		if e.sessionDead {
			e.sessionDead = false
			e.sessionDeadReason = ""
			e.sessionDeadAt = time.Time{}
		}
	}
}

// Status 查询单账号状态。
func (p *Pool) Status(uid string) (Status, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return Status{}, false
	}
	return p.statusOf(uid, e), true
}

// AuthByUID 返回账号的完整凭证（给调度器/运维接口用）。
func (p *Pool) AuthByUID(uid string) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.a
	}
	return nil
}

// List 返回所有账号状态（按 UID 排序，稳定输出）。
func (p *Pool) List() []Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]Status, 0, len(uids))
	for _, uid := range uids {
		out = append(out, p.statusOf(uid, p.byUID[uid]))
	}
	return out
}

// InUseWindow「正在调用」的时间窗：最近这么多秒内被打点的账号视为调用中。
//
// 为什么用时间窗近似而不是真实状态机：ChatStream 是流式的，一次调用可能
// 持续几十秒，期间既无开始也无结束信号可挂。要准确追踪就得给每个请求
// 建上下文并在 handler 返回时清理，还要处理客户端断连、超时、panic ———
// 复杂度和收益不成正比（面板只需要知道「哪个号正在被用」）。
// 5 秒窗的副作用：连续小请求会让徽标一直亮着，这符合直觉（确实在频繁使用）。
const InUseWindow = 5 * time.Second

// InUse 判断某账号此刻是否处于「调用中」。
func InUse(lastUsedAt time.Time, now time.Time) bool {
	return !lastUsedAt.IsZero() && now.Sub(lastUsedAt) < InUseWindow
}

func (p *Pool) statusOf(uid string, e *entry) Status {
	now := time.Now()
	var mc map[string]time.Time
	for m, t := range e.modelCool {
		if now.Before(t) {
			if mc == nil {
				mc = map[string]time.Time{}
			}
			mc[m] = t
		}
	}
	return Status{
		UID:               uid,
		Nickname:          e.a.Nickname,
		Credits:           e.credits,
		ExpiringCredits:   e.expiring,
		UnusableCredits:   e.unusable,
		CreditsStale:      e.creditsStale,
		Cooling:           !e.until.IsZero() && now.Before(e.until),
		Until:             e.until,
		Reason:            e.reason,
		Disabled:          e.disabled,
		ErrCount:          e.errCount,
		LastCheckinOK:     e.lastCheckinOK,
		LastCheckinAt:     e.lastCheckinAt,
		LastCheckinMsg:    e.lastCheckinMsg,
		SessionDead:       e.sessionDead,
		SessionDeadReason: e.sessionDeadReason,
		SessionDeadAt:     e.sessionDeadAt,
		ModelCooling:      mc,
		LastUsedAt:        e.lastUsedAt,
	}
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

func (p *Pool) load() {
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		return
	}
	var sf stateFile
	if json.Unmarshal(raw, &sf) != nil {
		return
	}
	p.rrLast = sf.RRLast
	// 旧格式（v2.2.0 及之前无 unusable；v2 无 expiring）：余额口径与新版不一致（可用/不可用/临期拆分），
	// 读入后置 creditsStale，待自动刷新循环首刷时重算并清除。
	// 新文件带 version>=stateVersion，且带 unusable 字段才视为可信。
	stale := sf.Version < stateVersion
	for uid, s := range sf.Accounts {
		p.byUID[uid] = &entry{
			a:                 &auth.Auth{UID: uid}, // placeholder，Add 时会换成完整凭证
			credits:           s.Credits,
			expiring:          s.Expiring,
			unusable:          s.Unusable,
			creditsStale:      stale,
			disabled:          s.Disabled,
			reason:            s.Reason,
			until:             s.Until,
			modelCool:         s.ModelCool,
			lastCheckinOK:     s.LastCheckinOK,
			lastCheckinAt:     s.LastCheckinAt,
			lastCheckinMsg:    s.LastCheckinMsg,
			sessionDead:       s.SessionDead,
			sessionDeadReason: s.SessionDeadReason,
			sessionDeadAt:     s.SessionDeadAt,
		}
	}
}

func (p *Pool) saveLocked() {
	if p.stateFp == "" {
		return
	}
	sf := stateFile{Version: stateVersion, RRLast: p.rrLast, Accounts: map[string]accountState{}}
	now := time.Now()
	for uid, e := range p.byUID {
		var mc map[string]time.Time
		for m, t := range e.modelCool {
			if now.Before(t) { // 过期条目不落盘
				if mc == nil {
					mc = map[string]time.Time{}
				}
				mc[m] = t
			}
		}
		sf.Accounts[uid] = accountState{
			Credits:           e.credits,
			Expiring:          e.expiring,
			Unusable:          e.unusable,
			Disabled:          e.disabled,
			Reason:            e.reason,
			Until:             e.until,
			ModelCool:         mc,
			LastCheckinOK:     e.lastCheckinOK,
			LastCheckinAt:     e.lastCheckinAt,
			LastCheckinMsg:    e.lastCheckinMsg,
			SessionDead:       e.sessionDead,
			SessionDeadReason: e.sessionDeadReason,
			SessionDeadAt:     e.sessionDeadAt,
		}
	}
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(p.stateFp); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := p.stateFp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p.stateFp)
}
