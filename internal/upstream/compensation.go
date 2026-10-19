// compensation.go WorkBuddy「DeepSeek 模型服务异常额外补偿 500 积分」的查询与领取。
//
// 背景（issue #147 的后续）：用户收到站内信/活动卡片「DeepSeek 模型服务异常额外补偿
// 500 积分，请于 10/22 前领取」，希望多账号一次领完。此前只在 checkin_activity.go 的
// 注释里记了「补偿不走签到端点」，没有实现 —— 本文件补齐。
//
// 端点来源（2026-10-19 实测，非猜测）：个人中心
// https://www.codebuddy.cn/profile/plans-usage 的前端包里可读到
//
//	POST /billing/meter/compensation-status   // 查询：active / claimed / credit_num / end_time
//	POST /billing/meter/claim-compensation    // 领取
//
// 前端页面用 fetch + Cookie 会话调这两个路径；本仓改用 Bearer + X-User-Id
// （与签到、费率等 billing 接口同一套头），实测同样可用。
//
// ⚠️ 路径**不带** `/v2` 前缀：实测 `/v2/billing/meter/compensation-status` 返回
// 404 Route Not Found，而不带前缀的同一路径返回 200。签到的 checkin-activity-status
// 反而带 `/v2` —— 同一个 billing 域下两种前缀并存，别想当然地统一。
//
// 实测回执（2026-10-19，本机 7 账号中的 2 个，全程只读，未发领取）：
//
//	POST https://www.codebuddy.cn/billing/meter/compensation-status  body {}
//	{"code":0,"msg":"OK","requestId":"…","data":{
//	  "claimed":false,"active":true,"credit_num":500,"validity_period":1,
//	  "end_time":"2026-10-22 23:59:59"}}
//
// 同一批账号里既有 claimed=false 也有 claimed=true —— 说明补偿是**按账号**发放的，
// 多账号批量领取有真实收益（这正是本功能的出发点）。
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"workbuddy-api/internal/auth"
)

// compensationStatusPath / compensationClaimPath 补偿活动的查询与领取端点。
// 注意：不带 /v2 前缀，理由见文件头（带 /v2 实测 404）。
const (
	compensationStatusPath = "/billing/meter/compensation-status"
	compensationClaimPath  = "/billing/meter/claim-compensation"
)

// CompensationStatus 补偿活动在一个账号上的状态。
type CompensationStatus struct {
	Active         bool   `json:"active"`           // 活动是否对该账号生效
	Claimed        bool   `json:"claimed"`          // 是否已领取（幂等键）
	CreditNum      int64  `json:"credit_num"`       // 补偿积分数（实测 500）
	ValidityPeriod int64  `json:"validity_period"`  // 到账后的有效月数
	EndTime        string `json:"end_time"`         // 领取截止时间，如 "2026-10-22 23:59:59"
}

// CompensationStatus 查询单个账号的补偿领取状态（只读）。
func (c *Client) CompensationStatus(a *auth.Auth) (*CompensationStatus, error) {
	req, err := http.NewRequest(http.MethodPost, c.billingBase(a)+compensationStatusPath,
		bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	BillingHeaders(req, a)
	data, err := c.doJSONBilling(req)
	if err != nil {
		return nil, err
	}
	var st CompensationStatus
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("compensation status parse: %w", err)
	}
	return &st, nil
}

// ClaimCompensation 领取补偿积分。
//
// 调用方应先查 CompensationStatus：只对 active && !claimed 的账号调用，
// 避免给已领取/无活动的账号发写请求（与签到「先只读探测再按需写」同一思路）。
//
// 返回的 error 已带上游业务码（doJSONBilling 统一处理 code!=0）。
// 「已领取」会用 isAlreadyClaimedCompensation 识别，调用方应视为成功而非失败。
func (c *Client) ClaimCompensation(a *auth.Auth) (int64, error) {
	req, err := http.NewRequest(http.MethodPost, c.billingBase(a)+compensationClaimPath,
		bytes.NewReader([]byte("{}")))
	if err != nil {
		return 0, err
	}
	BillingHeaders(req, a)
	data, err := c.doJSONBilling(req)
	if err != nil {
		if isAlreadyClaimedCompensation(err) {
			// 并发场景：别处刚领过。按已完成处理，积分数额取不到就用 0，
			// 由调用方用状态查询回填（见 ClaimCompensationReport）。
			return 0, nil
		}
		return 0, err
	}
	// 回执的 data 形制未在上游页面里展开（前端只判 code==0），
	// 故这里尽量解析但解析不出也不算失败：credit_num 以 status 查询为准。
	var got struct {
		CreditNum int64 `json:"credit_num"`
	}
	_ = json.Unmarshal(data, &got)
	return got.CreditNum, nil
}

// isAlreadyClaimedCompensation 判定错误是否为「已领取」形态。
// 上游未返回过该文案（实测两个账号分别是未领/已领，已领的账号不会再去领），
// 这里按活动文案的通告形态预置匹配，宁多勿漏 —— 误判为「已领取」的代价
// 只是面板少显示一个错误，误判为失败的代价是用户重复点击。
func isAlreadyClaimedCompensation(err error) bool {
	if err == nil {
		return false
	}
	for _, m := range []string{"已领取", "已申领", "已领取过", "already claim", "already received"} {
		if containsFold(err.Error(), m) {
			return true
		}
	}
	return false
}
