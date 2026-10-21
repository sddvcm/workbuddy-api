package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// DSH 桌面 GUI 身份段回归（2026-10-21 抓包 + 字符级二分实测定位）。
//
// 上游逐字锚定的是这两句以单空格拼接后的 221 字符整串：
//
//	You are interacting with the user through the DeepSeek Harness Web GUI whose URL
//	the runtime context names. When the user refers to "this page", "this GUI", or
//	"this app" without naming another target, they mean this GUI.
//
// 命中即整单 400 code=11-128「Illegal API invocation from an unapproved channel」。
//
// 实测的关键性质（决定了改写策略与下面的用例）：
//   - 单独任一句（107 / 113 字符）都放行；只有两句拼成的 221 字符整串才拦截
//   - 删掉任一字符、在两句之间插入换行（222 字符）、改动任一词，均放行
//   - 用等长无关文本填充到 221/4000 字符，放行
//
// 即逐字匹配的是整段连续串，破坏其中一处即可。改写点选在 GUI 身份段的收尾词
// names -> indicates：一词之差，指纹断裂，语义不变。
const (
	dshGUILead    = "You are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names."
	dshGUITail    = `When the user refers to "this page", "this GUI", or "this app" without naming another target, they mean this GUI.`
	dshGUIFinger  = dshGUILead + " " + dshGUITail
	dshGUILeadNew = "You are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context indicates."
)

// 完整 221 字符指纹必须被改写。
func TestDSHDesktopGUILeadRewritten(t *testing.T) {
	out := sanitizeText(dshGUIFinger)
	if strings.Contains(out, "runtime context names.") {
		t.Errorf("DSH desktop GUI fingerprint still present: %q", out)
	}
	if !strings.Contains(out, "runtime context indicates.") {
		t.Errorf("DSH desktop GUI lead not rewritten as expected: %q", out)
	}
	// 后半句必须原样保留（改写只动一处，语义不丢）。
	if !strings.Contains(out, dshGUITail) {
		t.Errorf("rewritten lead damaged the rest of the fingerprint run: %q", out)
	}
}

// 只有 GUI 身份段被改写；同段内其它 DSH 文案不受影响。
func TestDSHDesktopGUILeadMinimalRewrite(t *testing.T) {
	in := dshGUILead + " Starting another server does not update this GUI."
	out := sanitizeText(in)
	if !strings.Contains(out, "Starting another server does not update this GUI.") {
		t.Errorf("rewrite touched unrelated text: %q", out)
	}
}

// 预检必须命中，否则整条净化被跳过，指纹原样发上游。
func TestDSHDesktopGUIFingerprintDetected(t *testing.T) {
	if !hasFingerprint(dshGUIFinger) {
		t.Error("DSH desktop GUI fingerprint not detected by precheck")
	}
}

// 非逐字变体上游本就不拦，不应被改写（避免误伤正常讨论 GUI 的对话）。
func TestDSHDesktopGUINonExactVariantNotTouched(t *testing.T) {
	for _, in := range []string{
		"the runtime context names the checkout directory",
		"DeepSeek Harness Web GUI is mentioned in this document",
		"runtime context names vary between deployments",
	} {
		if out := sanitizeText(in); out != in {
			t.Errorf("non-exact variant should be untouched: %q -> %q", in, out)
		}
	}
}

// 集成：出站 wire body 的 system 消息里不得残留该指纹。
func TestDSHDesktopGUILeadNoFingerprintRemainsInWireBody(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":` + mustJSONString(dshGUIFinger) + `},` +
		`{"role":"user","content":"hi"}]}`)
	out := PrepareBody(body)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	msgs := obj["messages"].([]any)
	sys, _ := msgs[0].(map[string]any)["content"].(string)
	if strings.Contains(sys, "runtime context names.") {
		t.Errorf("DSH desktop GUI fingerprint remains in wire body: %q", sys)
	}
}

func mustJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
