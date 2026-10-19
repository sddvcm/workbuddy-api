package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// platformEncrypts 报告当前平台是否真的会加密（Windows 为 true，其余为 false）。
// 用它来区分「加解密往返」与「明文直通」两类断言，让同一份用例跨平台可跑。
func platformEncrypts() bool { return EncryptSecret("probe-value") != "probe-value" }

// TestSecretRoundTrip 加解密往返。
//
// Windows：走真实 DPAPI（密文与当前用户绑定）。
// 其他平台：两个函数都是恒等，往返同样成立 —— 断言对两边都有效。
func TestSecretRoundTrip(t *testing.T) {
	plain := "eyJhbGciOiJIUzI1NiJ9.fake-payload.signature-中文测试"
	enc := EncryptSecret(plain)
	if enc == "" {
		t.Fatal("加密结果为空")
	}
	if got := DecryptSecret(enc); got != plain {
		t.Errorf("往返不一致：got %q want %q", got, plain)
	}
	// 幂等：对已是密文的值再加密不得叠加前缀。
	if again := EncryptSecret(enc); again != enc {
		t.Error("对已加密值重复加密改变了内容")
	}
	// 明文输入必须原样返回（兼容更早期直接存明文的 auth 文件）。
	if got := DecryptSecret(plain); got != plain {
		t.Errorf("明文输入未原样返回：%q", got)
	}
	// 空值安全。
	if EncryptSecret("") != "" || DecryptSecret("") != "" {
		t.Error("空值处理异常")
	}
}

// TestParseDecryptsDPAPIToken ★ 本文件的核心回归用例。
//
// 场景（2026-10-11 实测故障）：账号凭据文件（无论来自手工复制还是旧的
// `--import-from` 导入——该功能现已移除）里，Windows 上 token 是 DPAPI
// 加密的（`dpapi:` 前缀），若解析时不解密，面板所有账号积分显示为 0。
// 日志里 workbuddy 账号返回 openresty 的 **401 HTML**，
// traework 返回业务 401（refresh token is invalid）。
//
// 根因：把 "dpapi:AQAAANCM..." 原样当作 Bearer token 发出去。
//
// 本用例断言 Parse 必须把密文解成明文，防止这个缺陷回归。
// 注意用例与「凭据从哪来」无关 —— 即使用户手工放置 auths/*.json，
// 也可能拿到旧项目导出的 DPAPI 密文，故该路径必须始终正确。
func TestParseDecryptsDPAPIToken(t *testing.T) {
	if !platformEncrypts() {
		t.Skip("当前平台不加密（非 Windows），该场景由 secure_other_test.go 覆盖")
	}
	plainAT := "at-plain-for-parse"
	plainRT := "rt-plain-for-parse"
	body := []byte(`{"auth":{"accessToken":"` + EncryptSecret(plainAT) +
		`","refreshToken":"` + EncryptSecret(plainRT) +
		`","expiresAt":1893456000,"domain":"www.codebuddy.cn"},` +
		`"account":{"uid":"u1","nickname":"测试账号"}}`)

	a, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.AccessToken != plainAT {
		t.Errorf("accessToken 未解密：got %q", a.AccessToken)
	}
	if a.RefreshToken != plainRT {
		t.Errorf("refreshToken 未解密：got %q", a.RefreshToken)
	}
	if a.AccessToken == "" || strings.HasPrefix(a.AccessToken, dpapiPrefix) {
		t.Error("accessToken 仍是密文或为空 —— 出站请求会带着密文换回 401")
	}
}

// TestLoadDirDecryptsImportedAuth 端到端模拟「导入旧项目 auths 目录后启动」：
// 文件里是密文，加载出来的内存对象必须是明文。
func TestLoadDirDecryptsImportedAuth(t *testing.T) {
	if !platformEncrypts() {
		t.Skip("当前平台不加密（非 Windows）")
	}
	dir := t.TempDir()
	plain := "imported-access-token-0123456789"
	body := `{"auth":{"accessToken":"` + EncryptSecret(plain) +
		`","refreshToken":"` + EncryptSecret("rt") +
		`","expiresAt":1893456000,"domain":"www.codebuddy.cn"},` +
		`"account":{"uid":"u9","nickname":"导入的账号"}}`
	fp := filepath.Join(dir, "workbuddy-u9.json")
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	list, err := LoadWorkBuddyDir(dir, "cn")
	if err != nil {
		t.Fatalf("LoadWorkBuddyDir: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("加载到 %d 个账号，期望 1 个", len(list))
	}
	if list[0].AccessToken != plain {
		t.Errorf("加载出的 token 未解密：got %q", list[0].AccessToken)
	}
}

// TestSaveAtomicEncryptsToken 与 Parse 用例互为反面：写盘必须是密文。
func TestSaveAtomicEncryptsToken(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	a := &Auth{
		AccessToken: "at-secret-value", RefreshToken: "rt-secret-value",
		UID: "u1", ExpiresAt: 1893456000, Domain: "www.codebuddy.cn", FilePath: fp,
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Auth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("落盘内容不是合法 JSON：%v", err)
	}
	// 重要：加密只影响落盘，**不得污染内存中的运行时 token**，
	// 否则紧接着用内存 token 发请求就会失败。
	if a.AccessToken != "at-secret-value" {
		t.Errorf("SaveAtomic 污染了内存 token：%q", a.AccessToken)
	}
	// 与下方 DPAPI 前缀断言同一套守卫：非 Windows 平台 EncryptSecret 是恒等函数，
	// 落盘天然就是明文，这条断言必须让路（否则 Linux/macOS 上必红）。
	// 那些平台的行为由 secure_other_test.go 覆盖。
	if platformEncrypts() && doc.Auth.AccessToken == "at-secret-value" {
		t.Error("落盘的 accessToken 是明文")
	}
	if platformEncrypts() {
		if !strings.HasPrefix(doc.Auth.AccessToken, dpapiPrefix) {
			t.Errorf("落盘 token 缺少 %s 前缀：%q", dpapiPrefix, doc.Auth.AccessToken)
		}
		if !strings.HasPrefix(doc.Auth.RefreshToken, dpapiPrefix) {
			t.Errorf("落盘 refreshToken 缺少 %s 前缀", dpapiPrefix)
		}
	}
}

// TestSaveThenLoadRoundTrip 完整闭环：存盘 → 读盘，明文一致。
// 这条覆盖「加密与解密两侧实现不对称」这类最难发现的缺陷
// （例如只加密写入、忘记解密读取 —— 正是本次故障的形态）。
func TestSaveThenLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	wantAT := "at-roundtrip-" + strings.Repeat("x", 48)
	wantRT := "rt-roundtrip-" + strings.Repeat("y", 48)
	a := &Auth{
		AccessToken: wantAT, RefreshToken: wantRT,
		UID: "u1", ExpiresAt: 1893456000, Domain: "www.codebuddy.cn", FilePath: fp,
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.AccessToken != wantAT {
		t.Errorf("闭环 accessToken 不一致：got %q want %q", got.AccessToken, wantAT)
	}
	if got.RefreshToken != wantRT {
		t.Errorf("闭环 refreshToken 不一致：got %q", got.RefreshToken)
	}
}

// TestSaveAtomicTwiceDoesNotNestEncryption 重复保存不得层层套加密。
// EncryptSecret 对已带前缀的值原样返回，这条用例把它钉住 ——
// 否则每次刷新 token 都会再包一层，文件迅速膨胀且再也解不开。
func TestSaveAtomicTwiceDoesNotNestEncryption(t *testing.T) {
	if !platformEncrypts() {
		t.Skip("当前平台不加密（非 Windows）")
	}
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	a := &Auth{AccessToken: "at-once", UID: "u1", ExpiresAt: 1893456000, Domain: "www.codebuddy.cn", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(fp)

	// 用读回的对象再存一次（刷新 token 后写回的典型路径）。
	// 注意 Parse 本身不设置 FilePath（那是 Load*Dir 的职责），这里手工补上。
	got, err := Parse(first)
	if err != nil {
		t.Fatal(err)
	}
	got.FilePath = fp
	if err := got.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(fp)

	var a1, a2 struct {
		Auth struct {
			AccessToken string `json:"accessToken"`
		} `json:"auth"`
	}
	_ = json.Unmarshal(first, &a1)
	_ = json.Unmarshal(second, &a2)
	if strings.Count(a2.Auth.AccessToken, dpapiPrefix) != 1 {
		t.Errorf("重复保存后前缀出现 %d 次（期望 1）：%q",
			strings.Count(a2.Auth.AccessToken, dpapiPrefix), a2.Auth.AccessToken)
	}
	if len(a2.Auth.AccessToken) > len(a1.Auth.AccessToken)+8 {
		t.Errorf("重复保存后长度异常增长：%d → %d", len(a1.Auth.AccessToken), len(a2.Auth.AccessToken))
	}
}
