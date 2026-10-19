package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy-api/internal/raccoon"
)

// 本测试锁定一个**真实 bug**（2026-10-05 用户报告）：
//
//	小浣熊走「浏览器授权登录」时，`internal/raccoon/protocol*.go` 定义了
//	`CallbackFlag = "--raccoon-callback"`（登录期间写入注册表命令行），
//	`raccoon.SaveCallback` 也实现好了 —— 但 **cmd/workbuddy-api/main.go 从未解析这个 flag**。
//
// 后果（不报错、不崩溃，但功能完全不可用）：
//  1. 用户在网页点授权 → Windows 按注册表命令行拉起 `workbuddy-api.exe --raccoon-callback <深链>`
//  2. 该进程**不知道**自己是回调子进程，于是又启动了一整份 daemon
//     —— 端口被已有实例占用（日志 `listen … bind: Only one usage…`），
//     而且它的启动自愈看到「残留的协议改写」→ 立刻 `RestoreProtocol` + `ClearCallback`
//  3. 授权码从未落盘，常驻进程的轮询永远读不到 → 一路等到 5 分钟超时
//
// 这与 R34（新增渠道漏了 `go xxxSch.Run()`）是同一类的「声明了却没接线」疏漏。
//
// 修复：main.go 在 chdir 之后、任何初始化之前调用 `handleRaccoonCallback(os.Args[1:])`，
// 命中就「落盘后退出」，绝不启动第二份服务/托盘。

func TestRaccoonCallbackEntryPointIsHandled(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读 main.go 失败: %v", err)
	}
	code := string(src)

	// ① main() 必须调用 handleRaccoonCallback
	if !strings.Contains(code, "handleRaccoonCallback(os.Args[1:])") {
		t.Fatalf("❌ main.go 未调用 handleRaccoonCallback —— " +
			"小浣熊授权回调会被当成普通启动，拉起第二份 daemon 并删掉回调文件（账号加不进来）\n" +
			"修复：在 main() 的 os.Chdir(workDir()) 之后补 " +
			"`if handled, err := handleRaccoonCallback(os.Args[1:]); handled { … os.Exit(0) }`")
	}

	// ② 该调用必须在任何服务初始化之前——否则仍会短暂启动第二份 daemon。
	//    以「启动自愈 / 监听」等关键词首次出现的位置为参照。
	idx := strings.Index(code, "handleRaccoonCallback(os.Args[1:])")
	for _, later := range []string{"app.New(", "net.Listen(", "systray."} {
		if p := strings.Index(code, later); p >= 0 && p < idx {
			t.Errorf("❌ handleRaccoonCallback 位于 %q 之后 —— 回调子进程会先启动服务再退出\n"+
				"修复：把它提前到 os.Chdir(workDir()) 之后、任何初始化之前", later)
		}
	}

	// ③ 该分支必须退出进程（否则会继续走守护流程）。
	if !strings.Contains(code[idx:], "os.Exit") {
		t.Errorf("❌ handleRaccoonCallback 分支没有 os.Exit —— 回调子进程会继续启动 daemon")
	}

	// ④ 判据必须用 raccoon.CallbackFlag 常量，而不是散落的字面量。
	if !strings.Contains(code, "raccoon.CallbackFlag") {
		t.Errorf("❌ 未引用 raccoon.CallbackFlag —— flag 名一旦改名，这里会静默失联")
	}
}

// TestRaccoonCallbackHandlerBehavior 直接跑 handler 本体：
// 参数不匹配不接管；匹配则落盘由 SaveCallback 读取的回调文件。
//
// 该 handler 从 CWD 的 config.json 推导 state 目录，故先把 CWD 切到临时目录，
// 避免往仓库 data/ 里写测试产物（结束后恢复）。
func TestRaccoonCallbackHandlerBehavior(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	// 不匹配：不管（含空参数、普通启动参数）。
	for _, args := range [][]string{nil, {}, {"--no-tray"}, {"--autostart"}} {
		if handled, _ := handleRaccoonCallback(args); handled {
			t.Errorf("args=%v 不应被当作回调处理", args)
		}
	}

	// 匹配但缺深链：接管且报错（不能静默吞掉）。
	if handled, err := handleRaccoonCallback([]string{raccoon.CallbackFlag}); !handled || err == nil {
		t.Errorf("缺深链应 handled=true 且 err!=nil，得到 handled=%v err=%v", handled, err)
	}

	// 匹配但深链非法：照样接管并落盘（带 Err 字段），让主进程能立刻报错而不是干等超时。
	if handled, err := handleRaccoonCallback([]string{raccoon.CallbackFlag, "not-a-deeplink"}); !handled || err != nil {
		t.Fatalf("非法深链应 handled=true 且落盘成功（解析错误写进 Err），得到 handled=%v err=%v", handled, err)
	}

	// 正常深链：落盘成功（载荷文件含 code/state，且不含原始深链）。
	const dl = "office-raccoon://auth/callback?code=ABC123&state=st1"
	handled, err := handleRaccoonCallback([]string{raccoon.CallbackFlag, dl})
	if !handled || err != nil {
		t.Fatalf("正常深链应 handled=true 且无错，得到 handled=%v err=%v", handled, err)
	}
	raw, rerr := os.ReadFile(filepath.Join(raccoon.StateDir(""), raccoon.CallbackFileName))
	if rerr != nil {
		t.Fatalf("回调未落盘（SaveCallback 未生效）：%v", rerr)
	}
	var p raccoon.CallbackPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("回调文件应为合法 JSON：%v", err)
	}
	if p.Code != "ABC123" || p.State != "st1" || p.Err != "" {
		t.Fatalf("回调内容不符：%+v", p)
	}
	if strings.Contains(string(raw), "office-raccoon://") {
		t.Fatalf("回调文件不应保存原始深链：%s", raw)
	}
}
