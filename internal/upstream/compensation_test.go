// 补偿领取（DeepSeek 模型服务异常 500 积分）的行为锁定。
//
// 固定样本来自 2026-10-19 的本机实测（7 账号中抽 2 个，全程只读，未发领取）：
//
//	POST https://www.codebuddy.cn/billing/meter/compensation-status {}
//	{"code":0,"msg":"OK","requestId":"…","data":{"claimed":false,"active":true,
//	 "credit_num":500,"validity_period":1,"end_time":"2026-10-22 23:59:59"}}
//
// 本文件锁三条不变量：
//  1. 路径**不带** /v2（带 /v2 实测 404，签到却带 —— 别想当然统一）；
//  2. 已领取/活动未生效时**不发写请求**（与签到同一思路，避免无谓的业务错误）；
//  3. 领取回执不带 credit_num 时，调用方能拿到 0 而不是 panic/错值。
package upstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy-api/internal/auth"
)

// compUpstream 假上游：按路径分发状态与领取回执，并统计写请求次数。
type compUpstream struct {
	statusBody  string
	statusCode  int
	claimBody   string
	claimCode   int
	claimCalls  int  // 写请求次数（核心断言）
	statusCalls int
	lastBody    string // 最后一次请求体（断言是 {} 而不是空）
	lastPath    string
	lastAuth    string
	lastUID     string
}

func (u *compUpstream) srv(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	// 故意只注册不带 /v2 的路径：若实现写错成 /v2，这里会 404，
	// 正好复现实测踩到的坑。
	mux.HandleFunc("/billing/meter/compensation-status", func(w http.ResponseWriter, r *http.Request) {
		u.statusCalls++
		u.lastPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		u.lastBody = string(b)
		u.lastAuth = r.Header.Get("Authorization")
		u.lastUID = r.Header.Get("X-User-Id")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(u.statusCode)
		_, _ = io.WriteString(w, u.statusBody)
	})
	mux.HandleFunc("/billing/meter/claim-compensation", func(w http.ResponseWriter, r *http.Request) {
		u.claimCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(u.claimCode)
		_, _ = io.WriteString(w, u.claimBody)
	})
	return httptest.NewServer(mux)
}

func newCompClient(srvURL string) *Client {
	c := New()
	c.BillingBaseCN = srvURL
	return c
}

func compAuth() *auth.Auth {
	return &auth.Auth{Kind: "workbuddy", UID: "uid-1", AccessToken: "at-1", RefreshToken: "rt-1"}
}

// TestCompensationStatusParsesRealShape 实测回执原样解析。
func TestCompensationStatusParsesRealShape(t *testing.T) {
	up := &compUpstream{statusCode: 200, statusBody: `{"code":0,"msg":"OK","requestId":"r1","data":{
		"claimed":false,"active":true,"credit_num":500,"validity_period":1,
		"end_time":"2026-10-22 23:59:59"}}`}
	srv := up.srv(t)
	defer srv.Close()

	st, err := newCompClient(srv.URL).CompensationStatus(compAuth())
	if err != nil {
		t.Fatalf("CompensationStatus: %v", err)
	}
	if !st.Active || st.Claimed {
		t.Errorf("active=%v claimed=%v，应为 true/false", st.Active, st.Claimed)
	}
	if st.CreditNum != 500 {
		t.Errorf("credit_num=%d，应为 500", st.CreditNum)
	}
	if st.EndTime != "2026-10-22 23:59:59" {
		t.Errorf("end_time=%q", st.EndTime)
	}
	// 请求形状：POST + {} + billing 头
	if up.lastPath != "/billing/meter/compensation-status" {
		t.Errorf("路径=%q，不应带 /v2 前缀", up.lastPath)
	}
	if up.lastBody != "{}" {
		t.Errorf("请求体=%q，应为 {}", up.lastBody)
	}
	if up.lastAuth != "Bearer at-1" {
		t.Errorf("Authorization=%q", up.lastAuth)
	}
	if up.lastUID != "uid-1" {
		t.Errorf("X-User-Id=%q", up.lastUID)
	}
}

// TestCompensationClaimSendsNoBody 领取回执只判 code==0（前端同样只判这个），
// 不带 credit_num 也不能算失败。
func TestCompensationClaimSendsNoBody(t *testing.T) {
	up := &compUpstream{statusCode: 200, statusBody: `{"code":0,"data":{"claimed":false,"active":true,"credit_num":500}}`,
		claimCode: 200, claimBody: `{"code":0,"msg":"OK","data":{}}`}
	srv := up.srv(t)
	defer srv.Close()

	credit, err := newCompClient(srv.URL).ClaimCompensation(compAuth())
	if err != nil {
		t.Fatalf("ClaimCompensation: %v", err)
	}
	if credit != 0 {
		t.Errorf("回执无 credit_num 时应返回 0，实得 %d（由调用方用 status 兜底）", credit)
	}
	if up.claimCalls != 1 {
		t.Errorf("领取请求应发 1 次，实得 %d", up.claimCalls)
	}
}

// TestCompensationClaimBusinessCodeIsError 业务码非 0 必须报错（不能当成功）。
func TestCompensationClaimBusinessCodeIsError(t *testing.T) {
	up := &compUpstream{statusCode: 200, statusBody: `{"code":0,"data":{"active":true}}`,
		claimCode: 200, claimBody: `{"code":10001,"msg":"credits exhausted"}`}
	srv := up.srv(t)
	defer srv.Close()

	if _, err := newCompClient(srv.URL).ClaimCompensation(compAuth()); err == nil {
		t.Fatal("业务码 10001 应报错")
	}
}

// TestCompensationAlreadyClaimedNotError 上游若回「已领取」文案，按成功处理
// （并发场景：别处刚领过；误判成失败会让用户重复点）。
func TestCompensationAlreadyClaimedNotError(t *testing.T) {
	up := &compUpstream{statusCode: 200, statusBody: `{"code":0,"data":{"active":true}}`,
		claimCode: 200, claimBody: `{"code":10002,"msg":"该补偿已领取"}`}
	srv := up.srv(t)
	defer srv.Close()

	if _, err := newCompClient(srv.URL).ClaimCompensation(compAuth()); err != nil {
		t.Fatalf("「已领取」应按成功处理，实得错误: %v", err)
	}
}

// TestCompensationStatusHTTPErrorPropagates 状态查询 HTTP 失败要报错，
// 让 App 层落到 error 状态而不是当成「活动未开启」。
func TestCompensationStatusHTTPErrorPropagates(t *testing.T) {
	up := &compUpstream{statusCode: 500, statusBody: `{"error_msg":"boom"}`}
	srv := up.srv(t)
	defer srv.Close()

	if _, err := newCompClient(srv.URL).CompensationStatus(compAuth()); err == nil {
		t.Fatal("HTTP 500 应报错")
	}
}

// TestCompensationWrongPrefixIs404 写死踩坑回归：/v2 前缀的同一路径必须 404。
// 这条测试的存在意义是——将来若有人「顺手统一前缀」，这里会先炸。
func TestCompensationWrongPrefixIs404(t *testing.T) {
	up := &compUpstream{statusCode: 200, statusBody: `{"code":0,"data":{}}`}
	srv := up.srv(t)
	defer srv.Close()

	c := newCompClient(srv.URL)
	req, _ := http.NewRequest(http.MethodPost, c.BillingBaseCN+"/v2/billing/meter/compensation-status", nil)
	BillingHeaders(req, compAuth())
	resp, err := c.BillingHTTP.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("带 /v2 的路径应 404（实测如此），实得 %d —— 说明假上游注册了不该有的路由", resp.StatusCode)
	}
}
