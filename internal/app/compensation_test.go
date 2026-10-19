// 一键领取额外补偿的面板侧行为锁定。
//
// 覆盖三条主路径与两条边界：
//   - 未领取 → 发写请求 → claimed，且计数/积分汇总正确；
//   - 已领取 → **不发写请求**（避免无谓的业务错误噪音）；
//   - 活动未生效 → 不发写请求；
//   - 单账号失败不中断整批；
//   - 只收 WorkBuddy 国内版：别的渠道混进来应得到 unsupported。
package app

import (
	"errors"
	"testing"

	"workbuddy-api/internal/auth"
	"workbuddy-api/internal/config"
	"workbuddy-api/internal/pool"
	"workbuddy-api/internal/provider"
	"workbuddy-api/internal/upstream"
)

// compFakeUpstream 在既有 fakeUpstream 上补两个补偿方法（同包复用其 provider.Upstream 实现）。
type compFakeUpstream struct {
	*fakeUpstream
	st       *upstream.CompensationStatus
	stErr    error
	claimErr error
	credit   int64
	claims   int // 写请求计数（核心断言）
}

func (f *compFakeUpstream) CompensationStatus(a *auth.Auth) (*upstream.CompensationStatus, error) {
	if f.stErr != nil {
		return nil, f.stErr
	}
	return f.st, nil
}

func (f *compFakeUpstream) ClaimCompensation(a *auth.Auth) (int64, error) {
	f.claims++
	if f.claimErr != nil {
		return 0, f.claimErr
	}
	return f.credit, nil
}

// newCompTestApp 构造只含 WorkBuddy 国内版的最小 App，并挂上假上游。
func newCompTestApp(t *testing.T, up provider.Upstream, uids ...string) *App {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StateFile = dir + "/state.json"
	cfg.AuthDir = dir

	p := pool.New(dir + "/state-workbuddy.json")
	for _, uid := range uids {
		p.Add(&auth.Auth{Kind: "workbuddy", UID: uid, AccessToken: "t", RefreshToken: "r"})
	}
	a, err := New(Options{
		ConfigPath: dir + "/config.json",
		Config:     cfg,
		Runtimes: map[provider.Kind]*Runtime{
			provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: p, Upstream: up},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(a.Close)
	return a
}

// TestClaimCompensationUnclaimedClaimsAndAggregates 未领取 → 领取 → 汇总正确。
func TestClaimCompensationUnclaimedClaimsAndAggregates(t *testing.T) {
	up := &compFakeUpstream{
		fakeUpstream: &fakeUpstream{},
		st:           &upstream.CompensationStatus{Active: true, Claimed: false, CreditNum: 500, EndTime: "2026-10-22 23:59:59"},
		credit:       500,
	}
	a := newCompTestApp(t, up, "u1", "u2")

	run := a.ClaimCompensationAll("")
	if run.Accounts != 2 || run.Claimed != 2 || run.Credit != 1000 {
		t.Fatalf("汇总不符：%+v（应 2 账号 / 2 新领 / 1000 积分）", run)
	}
	if up.claims != 2 {
		t.Errorf("写请求应发 2 次（每账号一次），实得 %d", up.claims)
	}
	for _, r := range run.Results {
		if r.Status != compClaimed || r.Credit != 500 || r.EndTime != "2026-10-22 23:59:59" {
			t.Errorf("账号 %s 结果不符：%+v", r.UID, r)
		}
		if r.Label == "" {
			t.Errorf("账号 %s 缺 Label（面板要显示）", r.UID)
		}
	}
}

// TestClaimCompensationAlreadyClaimedSkipsWrite 已领取 → 不发写请求。
func TestClaimCompensationAlreadyClaimedSkipsWrite(t *testing.T) {
	up := &compFakeUpstream{
		fakeUpstream: &fakeUpstream{},
		st:           &upstream.CompensationStatus{Active: true, Claimed: true, CreditNum: 500},
	}
	a := newCompTestApp(t, up, "u1")

	run := a.ClaimCompensationAll("")
	if run.Already != 1 || run.Claimed != 0 || run.Credit != 0 {
		t.Fatalf("汇总不符：%+v（应 1 already / 0 claimed / 0 积分）", run)
	}
	if up.claims != 0 {
		t.Errorf("已领取时不应发写请求，实得 %d 次", up.claims)
	}
}

// TestClaimCompensationInactiveSkipsWrite 活动未生效 → 不发写请求。
func TestClaimCompensationInactiveSkipsWrite(t *testing.T) {
	up := &compFakeUpstream{
		fakeUpstream: &fakeUpstream{},
		st:           &upstream.CompensationStatus{Active: false, Claimed: false},
	}
	a := newCompTestApp(t, up, "u1")

	run := a.ClaimCompensationAll("")
	if run.Inactive != 1 || up.claims != 0 {
		t.Fatalf("活动未生效：inactive=%d claims=%d，应 1 / 0", run.Inactive, up.claims)
	}
}

// TestClaimCompensationStatusErrorIsolated 状态查询失败 → 该账号记 error，不中断整批。
func TestClaimCompensationStatusErrorIsolated(t *testing.T) {
	up := &compFakeUpstream{fakeUpstream: &fakeUpstream{}, stErr: errors.New("boom")}
	a := newCompTestApp(t, up, "u1", "u2")

	run := a.ClaimCompensationAll("")
	if run.Error != 2 || run.Claimed != 0 {
		t.Fatalf("两个账号都应记 error：%+v", run)
	}
	if up.claims != 0 {
		t.Errorf("查询失败时不应发写请求，实得 %d", up.claims)
	}
	if run.Results[0].Msg == "" {
		t.Error("失败结果应带 Msg（面板要显示原因）")
	}
}

// TestClaimCompensationScopeSingleUID 指定 uid 只处理该账号（先试水一个的用法）。
func TestClaimCompensationScopeSingleUID(t *testing.T) {
	up := &compFakeUpstream{
		fakeUpstream: &fakeUpstream{},
		st:           &upstream.CompensationStatus{Active: true},
		credit:       500,
	}
	a := newCompTestApp(t, up, "u1", "u2")

	run := a.ClaimCompensationAll("u2")
	if run.Accounts != 1 || run.Claimed != 1 {
		t.Fatalf("scope=u2 应只处理 1 个账号：%+v", run)
	}
	if run.Results[0].UID != "u2" {
		t.Errorf("处理的 uid=%q，应为 u2", run.Results[0].UID)
	}
}

// TestClaimCompensationNonWorkBuddyUnsupported 只有 WorkBuddy 国内版参与：
// 别的渠道（即使有账号）不应被纳入，否则只会换来 404。
func TestClaimCompensationNonWorkBuddyUnsupported(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StateFile = dir + "/state.json"
	cfg.AuthDir = dir

	wb := pool.New(dir + "/state-workbuddy.json")
	wb.Add(&auth.Auth{Kind: "workbuddy", UID: "u1", AccessToken: "t", RefreshToken: "r"})
	other := pool.New(dir + "/state-traework.json")
	other.Add(&auth.Auth{Kind: "traework", UID: "t1", AccessToken: "t", RefreshToken: "r"})

	up := &compFakeUpstream{fakeUpstream: &fakeUpstream{}, st: &upstream.CompensationStatus{Active: true}, credit: 500}
	a, err := New(Options{
		ConfigPath: dir + "/config.json",
		Config:     cfg,
		Runtimes: map[provider.Kind]*Runtime{
			provider.WorkBuddy:  {Kind: provider.WorkBuddy, Pool: wb, Upstream: up},
			provider.TraeWork:   {Kind: provider.TraeWork, Pool: other, Upstream: up},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(a.Close)

	run := a.ClaimCompensationAll("")
	if run.Accounts != 1 || run.Results[0].UID != "u1" {
		t.Fatalf("应只收 WorkBuddy 账号：%+v", run)
	}
}
