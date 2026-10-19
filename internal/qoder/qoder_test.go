package qoder

import (
	"encoding/json"
	"testing"
)

func TestNormalizeModelName(t *testing.T) {
	cases := map[string]string{
		"Qwen3.8-Max":      "qwen3.8-max",
		"DeepSeek-V4-Pro":  "deepseek-v4-pro",
		"GLM-5.3":          "glm-5.3",
		"Kimi-K2.7-Code":   "kimi-k2.7-code",
		"MiniMax-M2.7":     "minimax-m2.7",
		"Auto":             "auto",
		"Qwen3.8 Max Test": "qwen3.8-max-test",
	}
	for in, want := range cases {
		if got := NormalizeModelName(in); got != want {
			t.Errorf("NormalizeModelName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestModelKeyDynamicPrecedence(t *testing.T) {
	c := New()
	c.modelMu.Lock()
	c.modelMap = map[string]string{"glm-5.3": "gmodel-new", "custom": "ckey"}
	c.modelMu.Unlock()
	if got := c.modelKey("glm-5.3"); got != "gmodel-new" {
		t.Errorf("dynamic should win, got %q", got)
	}
	if got := c.modelKey("custom"); got != "ckey" {
		t.Errorf("dynamic custom got %q", got)
	}
	if got := c.modelKey("deepseek-v4-pro"); got != "dmodel" { // 动态缺失 → 静态兜底
		t.Errorf("static fallback got %q", got)
	}
}

func TestBuildAgentBodyDeveloperToSystem(t *testing.T) {
	// developer 角色应改写为 system；其余消息不受影响；原数据不被污染。
	msgs := []map[string]any{
		{"role": "developer", "content": "你是助手"},
		{"role": "system", "content": "保持简洁"},
		{"role": "user", "content": "你好"},
	}
	raw, err := buildAgentBody(msgs, "dmodel", nil, nil, false, "", 0)
	if err != nil {
		t.Fatalf("buildAgentBody: %v", err)
	}
	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Messages) != 3 {
		t.Fatalf("message count = %d, want 3", len(body.Messages))
	}
	if body.Messages[0]["role"] != "system" {
		t.Errorf("developer should become system, got %v", body.Messages[0]["role"])
	}
	if body.Messages[1]["role"] != "system" {
		t.Errorf("system unchanged, got %v", body.Messages[1]["role"])
	}
	if body.Messages[2]["role"] != "user" {
		t.Errorf("user unchanged, got %v", body.Messages[2]["role"])
	}
	// 原数据不被污染
	if msgs[0]["role"] != "developer" {
		t.Errorf("source mutated: role=%v", msgs[0]["role"])
	}
	// prompt 取最后一条 user 内容
	var prompt struct {
		ChatContext struct {
			Text struct {
				Text string `json:"text"`
			} `json:"text"`
		} `json:"chat_context"`
	}
	_ = json.Unmarshal(raw, &prompt)
	if prompt.ChatContext.Text.Text != "你好" {
		t.Errorf("prompt = %q, want 你好", prompt.ChatContext.Text.Text)
	}
}

// TestParseSceneModelsContextWindow 验证场景三级回退 + context_config 解析。
// 回归：旧实现只读 chat 场景（且缺字段时硬失败）、完全丢弃 context_config。
func TestParseSceneModelsContextWindow(t *testing.T) {
	// ① 只有 assistant 场景 → 三级回退命中（旧实现会报 "no chat scene"）
	raw := map[string]json.RawMessage{
		"assistant": json.RawMessage(`[
			{"key":"qmodel_38max","display_name":"Qwen3.8-Max","enable":true,"is_default":true,
			 "is_reasoning":true,"is_vl":true,"max_input_tokens":180000,"price_factor":0.5,
			 "format":"openai","source":"system",
			 "context_config":{"default":{"is_default":true,"token_count":200000},
			                   "1M":{"is_default":false,"token_count":1000000}}},
			{"key":"dmodel","display_name":"DeepSeek-V4-Pro","enable":true,"max_input_tokens":96000,
			 "price_factor":0.1,"context_config":[]},
			{"key":"off","display_name":"OFF","enable":false}
		]`),
	}
	ms, err := parseSceneModels(raw)
	if err != nil {
		t.Fatalf("parseSceneModels: %v", err)
	}
	if len(ms) != 2 {
		t.Fatalf("enabled len = %d, want 2 (off 应被过滤)", len(ms))
	}
	// ② is_default 那项胜出，而不是 max_input_tokens 的 180000
	if ms[0].ContextWindow != 200000 {
		t.Errorf("is_default token_count not parsed: %+v", ms[0])
	}
	if ms[0].Format != "openai" || ms[0].Source != "system" {
		t.Errorf("format/source not parsed: %+v", ms[0])
	}
	// ③ 形状不符（数组）只损失本字段，不应让整批解析失败
	if ms[1].ContextWindow != 0 {
		t.Errorf("malformed context_config should degrade to 0: %+v", ms[1])
	}

	// ④ chat 缺失、developer 存在 → 回退 developer
	raw = map[string]json.RawMessage{
		"developer": json.RawMessage(`[{"key":"dk","display_name":"Dev","enable":true}]`),
	}
	if ms, err := parseSceneModels(raw); err != nil || len(ms) != 1 || ms[0].Key != "dk" {
		t.Errorf("developer fallback: %v %v", ms, err)
	}

	// ⑤ 三场景都空 → 报错
	raw = map[string]json.RawMessage{"chat": json.RawMessage(`[]`)}
	if _, err := parseSceneModels(raw); err == nil {
		t.Error("empty scenes should error")
	}

	// ⑥ assistant 优先于 chat
	raw = map[string]json.RawMessage{
		"chat":      json.RawMessage(`[{"key":"ck","enable":true}]`),
		"assistant": json.RawMessage(`[{"key":"ak","enable":true}]`),
	}
	if ms, err := parseSceneModels(raw); err != nil || len(ms) != 1 || ms[0].Key != "ak" {
		t.Errorf("assistant precedence: %v %v", ms, err)
	}
}

// TestBuildAgentBodyFormatSourceFromUpstream 验证 model_config 带上上游的 format/source。
// 回归：旧实现的 model_config 只有 {key,is_reasoning}，完全不下发 source
// （DEVELOPMENT.md §8 已记为 issue #32：旧 Qoder 思考过程不暴露）。
func TestBuildAgentBodyFormatSourceFromUpstream(t *testing.T) {
	body, err := buildAgentBody([]map[string]any{{"role": "user", "content": "hi"}},
		"k1", &DynamicModel{Key: "k1", Format: "up-format", Source: "up-source"}, nil, false, "", 0)
	if err != nil {
		t.Fatalf("buildAgentBody: %v", err)
	}
	var parsed struct {
		ModelConfig map[string]any `json:"model_config"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if parsed.ModelConfig["key"] != "k1" {
		t.Errorf("model_config.key = %v", parsed.ModelConfig["key"])
	}
	if parsed.ModelConfig["format"] != "up-format" {
		t.Errorf("model_config.format = %v, want upstream value", parsed.ModelConfig["format"])
	}
	if parsed.ModelConfig["source"] != "up-source" {
		t.Errorf("model_config.source = %v, want upstream value", parsed.ModelConfig["source"])
	}

	// 静态表兜底路径（mc == nil）→ 用兜底常量
	body, err = buildAgentBody([]map[string]any{{"role": "user", "content": "hi"}},
		"dmodel", nil, nil, false, "", 0)
	if err != nil {
		t.Fatalf("buildAgentBody(nil entry): %v", err)
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal body(nil entry): %v", err)
	}
	if parsed.ModelConfig["format"] != defaultFormat || parsed.ModelConfig["source"] != defaultSource {
		t.Errorf("nil entry should use fallback: %+v", parsed.ModelConfig)
	}
}

// TestModelEntryLookup 验证 key→条目表缓存与未命中语义（静态表兜底时为 nil）。
func TestModelEntryLookup(t *testing.T) {
	c := New()
	if e := c.modelEntry("dmodel"); e != nil {
		t.Errorf("no entries yet should be nil, got %+v", e)
	}
	c.setModelEntries(map[string]DynamicModel{"dmodel": {Key: "dmodel", Source: "system"}}, nil)
	e := c.modelEntry("dmodel")
	if e == nil || e.Source != "system" {
		t.Errorf("entry lookup failed: %+v", e)
	}
	if e := c.modelEntry("nope"); e != nil {
		t.Errorf("missing key should be nil, got %+v", e)
	}
}

// TestBuildAgentBodyContextLength 验证 context_length 注入（issue #27）。
func TestBuildAgentBodyContextLength(t *testing.T) {
	mc := &DynamicModel{Key: "k", MaxInputTokens: 180000, ContextWindow: 200000}
	raw, err := buildAgentBody([]map[string]any{{"role": "user", "content": "hi"}},
		"k", mc, nil, false, "", 400000)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Parameters  map[string]any `json:"parameters"`
		ModelConfig map[string]any `json:"model_config"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body.Parameters["context_length"] != float64(400000) {
		t.Errorf("context_length = %v", body.Parameters["context_length"])
	}
	if body.ModelConfig["max_input_tokens"] != float64(400000) {
		t.Errorf("max_input_tokens = %v", body.ModelConfig["max_input_tokens"])
	}
	// window=0 且无 effort → 无 parameters 字段
	raw, err = buildAgentBody([]map[string]any{{"role": "user", "content": "hi"}},
		"k", mc, nil, false, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var body2 map[string]any
	if err := json.Unmarshal(raw, &body2); err != nil {
		t.Fatal(err)
	}
	if _, has := body2["parameters"]; has {
		t.Errorf("parameters should be absent when no window/effort: %v", body2["parameters"])
	}
}

// TestResolveContextWindow TZ 校验 + 本仓默认最大档。
func TestResolveContextWindow(t *testing.T) {
	mc := &DynamicModel{Key: "m", MaxInputTokens: 180000, ContextWindow: 200000,
		AvailableWindows: []int64{200000, 400000, 1000000}}
	if got := resolveContextWindow(400000, mc); got != 400000 {
		t.Errorf("valid: got %d", got)
	}
	// 非法值与未指定 → 最大档（本仓默认，非官方 is_default）
	if got := resolveContextWindow(999, mc); got != 1000000 {
		t.Errorf("invalid → max: got %d, want 1000000", got)
	}
	if got := resolveContextWindow(0, mc); got != 1000000 {
		t.Errorf("default → max: got %d, want 1000000", got)
	}
}
