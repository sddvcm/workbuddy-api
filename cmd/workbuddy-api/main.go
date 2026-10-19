// Command workbuddy-api 系统托盘 daemon 入口。
// 单进程 = OpenAI 兼容 HTTP 服务 + 自动签到调度器 + 系统托盘 + 静态 Web UI。
// 双击 exe 启动常驻托盘；托盘菜单：打开主界面 / 刷新积分 / 查看日志 / 退出；
// 打开主界面或双击托盘 → 系统浏览器打开 Web UI。
//
// 另有一个**非守护**入口：`--raccoon-callback <深链>`（小浣熊**旧版**桌面协议登录的回调，
// 由 Windows 按 HKCU 的 office-raccoon 注册表唤起）。该分支只把深链解析后落盘再退出，
// **绝不**启动第二份服务/托盘——详见 handleRaccoonCallback。
//
// 注：小浣熊登录已于 R44 改为**网页版 CDP 捕获**（不再改写注册表），此入口保留
// 仅为了接住旧版本可能残留的注册表指向，属兼容死代码，不再有新流程使用。
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	// 内嵌 IANA 时区数据库。
	//
	// 精简基础镜像（alpine 等）与部分裸机 Linux 不提供 /usr/share/zoneinfo，
	// 此时 Go 无法解析 TZ=Asia/Shanghai，time.Local 会**静默退化为 UTC** ——
	// 定时签到将按 UTC 时刻触发（北京时间 17:00 / 05:00，而非 09:00 / 21:00），
	// 且日志里看不出任何异常。内嵌后二进制自带时区表，与运行环境解耦（约 +450KB）。
	_ "time/tzdata"

	"workbuddy-api/internal/app"
	"workbuddy-api/internal/auth"
	"workbuddy-api/internal/cline"
	"workbuddy-api/internal/codearts"
	"workbuddy-api/internal/config"
	"workbuddy-api/internal/custom"
	"workbuddy-api/internal/devdump"
	"workbuddy-api/internal/gateway"
	"workbuddy-api/internal/gemini"
	"workbuddy-api/internal/glm"
	"workbuddy-api/internal/kilo"
	"workbuddy-api/internal/ledger"
	"workbuddy-api/internal/lobsterai"
	"workbuddy-api/internal/loomy"
	"workbuddy-api/internal/loopguard"
	"workbuddy-api/internal/minimax"
	"workbuddy-api/internal/monkeycode"
	"workbuddy-api/internal/oczen"
	"workbuddy-api/internal/officeace"
	"workbuddy-api/internal/platform"
	"workbuddy-api/internal/pool"
	"workbuddy-api/internal/provider"
	"workbuddy-api/internal/qoder"
	"workbuddy-api/internal/qodercn"
	"workbuddy-api/internal/qodercom"
	"workbuddy-api/internal/qwenwork"
	"workbuddy-api/internal/raccoon"
	"workbuddy-api/internal/scheduler"
	"workbuddy-api/internal/server"
	"workbuddy-api/internal/stats"
	"workbuddy-api/internal/systray"
	"workbuddy-api/internal/traework"
	"workbuddy-api/internal/upstream"
	"workbuddy-api/internal/workbuddyai"
	"workbuddy-api/internal/zcode"
)

//go:embed all:web
var webFS embed.FS

//go:embed build/trayicon.ico
var trayIconICO []byte

func main() {
	// 工作目录：便携/CLI 形态固定为 exe 所在目录，保证相对路径配置（./auths ./data）稳定。
	// macOS .app bundle 内该目录只读、且随 app 替换被清空，故改用系统数据目录（见 workDir）。
	_ = os.Chdir(workDir())

	// 小浣熊授权登录的协议回调：由 Windows 按注册表命令行唤起本 exe。
	// 必须在**任何**初始化（config/日志/服务）之前拦下——它只落盘一个文件就退出，
	// 若走后面流程会启动第二份 daemon（日志与本进程抢锁、还可能在收到授权码后
	// 立刻把回调文件删掉），导致授权完成后账号加不进来。
	if handled, err := handleRaccoonCallback(os.Args[1:]); handled {
		if err != nil {
			log.Printf("小浣熊回调处理失败：%v", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	// 进程级单实例锁。
	//
	// ⚠️ 位置很关键：必须放在**浣熊回调之后**。回调是「非守护」调用
	// （`--raccoon-callback <深链>`），需要在守护进程已在运行时也能执行；
	// 若在它之前抢锁，回调进程会被拒 → 授权码落不了盘 → 登录链路断掉。
	//
	// 同时必须放在 HTTP 服务 / 调度器之前：本项目的基线曾出现过
	// 「先起 HTTP 再查锁」导致第二个实例 bind 失败、留下僵尸进程的问题。
	if !platform.AcquireSingleInstance("WorkBuddy-API-SingleInstance") {
		log.Printf("已有实例在运行，本实例退出")
		// 交互式启动时给用户一个可见反馈（无头模式只写日志）
		if !slices.Contains(os.Args[1:], "--no-tray") {
			platform.InfoBox("WorkBuddy-API",
				"程序已在运行。\n\n"+
					"请从右下角托盘图标打开管理面板，或先在托盘菜单中退出后再启动。")
		}
		os.Exit(0)
	}

	// --autostart 由开机自启项附带：开机启动不弹提示
	// --no-tray 无头模式（无桌面 Linux/服务器）
	//
	// 曾在这一段解析 `--import-from <dir>`（首次启动时从指定目录导入账号
	// 凭据）。该能力已整体移除 —— 程序不再从任何路径复制登录态数据。
	autostart := false
	noTray := false
	for _, arg := range os.Args[1:] {
		switch arg {
		case "--autostart":
			autostart = true
		case "--no-tray":
			noTray = true
		}
	}
	cfgPath := "config.json"
	// firstRun：本次启动时配置文件**本来不存在**（真首次启动）。
	// 用于「从旧项目导入账号凭据」的触发判据 —— 必须是真首次，
	// 否则每次启动都去探测旧目录，行为不可预期。
	//
	// 用 LoadOrCreate 而非 Load + 自行 Default()：后者会跳过 WILDWORK_* 环境变量
	// 覆盖，使「纯 env 部署」（容器/Docker Compose 的典型用法）静默失效。
	cfg, firstRun, err := config.LoadOrCreate(cfgPath)
	if err != nil {
		fatal("加载配置失败：%v\n\n请检查 config.json 后重新启动", err)
	}
	if firstRun {
		// 首次启动必须**明确告知 key 的值**，而不只是「已生成」。
		//
		// 为什么必须打印：config.json 是自动生成的，用户没参与这个决策，
		// 不知道自己的服务需要一个 key 才能接入 AI 客户端。等到他打开
		// 面板、客户端连不上、报 401，才会回头找这个值 —— 而无头模式下
		// 他可能根本没意识到要去面板找。
		//
		// 这也是本项目不设「固定默认 key」的原因之一：随机生成的值必须
		// 有明确的获取途径，否则等于没有。四个途径：本次日志、面板顶栏
		// （点击复制）、设置弹层、直接看 config.json。
		log.Printf("config.json 不存在，已按默认值 + WILDWORK_* 环境变量生成")
		if cfg.APIKey != "" {
			log.Printf("")
			log.Printf("  ┌─────────────────────────────────────────────")
			log.Printf("  │ 你的 API Key（接入 AI 客户端时填这个）")
			log.Printf("  │   %s", cfg.APIKey)
			log.Printf("  │")
			log.Printf("  │ 已随机生成并写入 config.json，请妥善保管 ——")
			log.Printf("  │ 它是访问接口的凭证，别放进公开仓库或分享给他人。")
			log.Printf("  │ 面板顶栏可点击复制，或随时在 ⚙ 设置中修改。")
			log.Printf("  └─────────────────────────────────────────────")
			log.Printf("")
		}
	}
	// 账号凭据一律从空开始：程序**不主动扫描或复制**任何目录下的 auths/。
	//
	// 曾经有个「首次启动自动从旧项目导入」的功能（探测写死的 D:\ 盘路径 +
	// 用户主目录），公开发布后被判定为必须移除，理由有三：
	//
	//  1. 写死的绝对路径把开发者的私人部署目录暴露在公开源码里；
	//  2. 扫描用户主目录属于**未经请求的数据获取** —— 用户没要求，程序却
	//     在翻他的磁盘，任何第三方安装同名软件都可能触发；
	//  3. 它复制的是登录态凭据（access_token / refresh_token），
	//     静默复制第三方账号的凭据到本地是明确的隐私问题。
	//
	// 现在唯一入口是用户在面板里手动登录，或手工把 auths/*.json 拷进来。
	// 管理面板鉴权：监听非环回地址（0.0.0.0 / 网卡 IP）时必须设置管理员密码，
	// 否则局域网任何设备都能无凭据打开面板、退出程序。启动层与面板保存层双重把关。
	if !cfg.Listen.IsLoopback() && strings.TrimSpace(cfg.AdminPass) == "" {
		fatal("监听 %s 已对外暴露，但 config.json 中未设置 admin_password。\n\n"+
			"请设置 admin_password（或在面板设置中修改监听地址）后重新启动；\n"+
			"若只想本机使用，把 listen.host 改回 127.0.0.1。", cfg.Listen.Addr())
	}
	if cfg.AdminPass != "" && len(cfg.AdminPass) < 8 {
		log.Printf("警告：管理员密码不足 8 位，建议使用更长口令（面板鉴权已生效）")
	}
	// API Key 为空 = **完全关闭接口鉴权**（httpauth.VerifyBearer 首行即放行）。
	// 这不是「弱密码」，而是「无密码」，但它与 admin_password 不同：
	// 后者管面板（改配置、退出程序），前者管 AI 客户端接口（消耗账号额度）。
	// 监听非环回时，空 key 等于把上游额度敞给整个局域网。
	//
	// 这里与 admin_password 同等对待 —— 拒绝启动而非仅告警：
	// 用户真正的意图几乎不会是「我要对外开一个无鉴权的接口」，
	// 若是误操作，静默启动会造成长期暴露且无人察觉。
	if strings.TrimSpace(cfg.APIKey) == "" {
		if !cfg.Listen.IsLoopback() {
			fatal("监听 %s 已对外暴露，但 config.json 中 api_key 为空（等于完全关闭接口鉴权）。\n\n"+
				"请设置 api_key，或把 listen.host 改回 127.0.0.1 仅本机使用。\n"+
				"提示：删掉 api_key 这个「字段」不会重新生成 —— 缺失字段会沿用"+
				"程序生成的随机值；只有显式写成空字符串 \"api_key\": \"\" 才会关闭鉴权。",
				cfg.Listen.Addr())
		}
		log.Printf("警告：api_key 为空，接口未启用鉴权（任何本机进程都能调用并消耗账号额度）")
	}

	// 思考链死循环检测（发癫拦截）开关：启动时按配置初始化，面板可运行时切换。
	loopguard.SetEnabled(cfg.Features.LoopGuard)
	if !cfg.Features.LoopGuard {
		log.Printf("思考链死循环检测已关闭（features.loop_guard=false）")
	}
	_ = os.MkdirAll(cfg.AuthDir, 0o755)
	_ = os.MkdirAll(filepath.Dir(cfg.StateFile), 0o755)

	// 多渠道运行时：workbuddy / traework / qoder 各自独立 pool + state + scheduler。
	stateDir := filepath.Dir(cfg.StateFile)
	// 旧版兼容：迁移 state.json → 分渠道 state-{kind}.json
	migrateStateFiles(cfg.StateFile, stateDir)

	wbAuths, err := auth.LoadWorkBuddyDir(cfg.AuthDir, cfg.Region)
	if err != nil {
		fatal("读取 WorkBuddy 账号目录失败：%v", err)
	}
	trAuths, err := auth.LoadTraeDir(cfg.AuthDir)
	if err != nil {
		fatal("读取 TraeWork 账号目录失败：%v", err)
	}
	qdAuths, err := auth.LoadQoderDir(cfg.AuthDir)
	if err != nil {
		fatal("读取 Qoder 账号目录失败：%v", err)
	}
	wbaAuths, err := auth.LoadWorkBuddyAiDir(cfg.AuthDir)
	if err != nil {
		fatal("读取 WorkBuddy 国际版账号目录失败：%v", err)
	}
	qwAuths, err := auth.LoadQwenWorkDir(cfg.AuthDir)
	if err != nil {
		fatal("读取千问办公账号目录失败：%v", err)
	}
	qcnAuths, err := auth.LoadQoderCNDir(cfg.AuthDir)
	if err != nil {
		fatal("读取 QoderCN 账号目录失败：%v", err)
	}
	qcmAuths, err := auth.LoadQoderCOMDir(cfg.AuthDir)
	if err != nil {
		fatal("读取 QoderCOM 账号目录失败：%v", err)
	}
	// 智谱清言：凭据是浏览器 Cookie 里的 chatglm_refresh_token（glm-*.json）。
	glmAuths, err := auth.LoadGLMDir(cfg.AuthDir)
	if err != nil {
		fatal("读取智谱清言账号目录失败：%v", err)
	}
	// MonkeyCode：同为「导入型」渠道，凭据由面板「从本机客户端导入」写入 auths/。
	mcAuths, err := auth.LoadMonkeyCodeDir(cfg.AuthDir)
	if err != nil {
		fatal("读取 MonkeyCode 账号目录失败：%v", err)
	}
	// 小浣熊：导入型渠道，凭据由面板「从本机客户端导入」或浏览器授权登录写入 auths/。
	rcAuths, err := auth.LoadRaccoonDir(cfg.AuthDir)
	if err != nil {
		fatal("读取小浣熊账号目录失败：%v", err)
	}
	// Loomy：导入型渠道，凭据由面板「从本机客户端导入」写入 auths/。
	lmAuths, err := auth.LoadLoomyDir(cfg.AuthDir)
	if err != nil {
		fatal("读取 Loomy 账号目录失败：%v", err)
	}
	// Cline：粘贴型渠道，凭据是官方 API Key（sk_…），由面板「粘贴 API Key」写入 auths/。
	// 一个 Cline 账号一把 Key；Key 不轮换 ⇒ 无 refresh、无保活、无签到。
	clAuths, err := auth.LoadClineDir(cfg.AuthDir)
	if err != nil {
		fatal("读取 Cline 账号目录失败：%v", err)
	}
	// OfficeAce：导入型渠道，凭据由面板从本机 OfficeAce 客户端文件发现或自助 OAuth 登录写入 auths/。
	oaAuths, err := auth.LoadOfficeAceDir(cfg.AuthDir)
	if err != nil {
		fatal("读取 OfficeAce 账号目录失败：%v", err)
	}
	// OpenCodeZen 匿名通道无凭证文件：全程只有一个虚拟账号，
	// 不经目录扫描、不参与 reload（见 app.reloadAccounts 的说明）。
	ocAuths := []*auth.Auth{oczen.AnonymousAuth()}
	// Kilo 免费池同样是免密渠道：没有凭证文件、没有登录流程，只有一个虚拟账号。
	kiAuths := []*auth.Auth{kilo.AnonymousAuth()}
	// MiniMax Code：正常多账号渠道，凭据由设备码登录写入 auths/minimax-*.json。
	mimAuths, err := auth.LoadMinimaxDir(cfg.AuthDir)
	if err != nil {
		fatal("读取 MiniMax 账号目录失败：%v", err)
	}
	// LobsterAI：正常多账号渠道，凭据由本机回调登录写入 auths/lobsterai-*.json。
	lobAuths, err := auth.LoadLobsterAIDir(cfg.AuthDir)
	if err != nil {
		fatal("读取 LobsterAI 账号目录失败：%v", err)
	}
	// ZCode：正常多账号渠道，凭据由服务端中介设备授权流登录写入 auths/zcode-*.json。
	zcdAuths, err := auth.LoadZCodeDir(cfg.AuthDir)
	if err != nil {
		fatal("读取 ZCode 账号目录失败：%v", err)
	}
	// Gemini（Google Code Assist）：正常多账号渠道，凭据由 loopback OAuth 登录写入 auths/gemini-*.json。
	gemAuths, err := auth.LoadGeminiDir(cfg.AuthDir)
	if err != nil {
		fatal("读取 Gemini 账号目录失败：%v", err)
	}
	// CodeArts（华为云 CodeArts Agent）：正常多账号渠道，凭据为 AK/SK + SecurityToken
	// （由 PKCE OAuth 登录写入 auths/codearts-*.json）。
	caAuths, err := auth.LoadCodeArtsDir(cfg.AuthDir)
	if err != nil {
		fatal("读取 CodeArts 账号目录失败：%v", err)
	}
	log.Printf("loaded accounts: workbuddy=%d %s, traework=%d, qoder=%d, qodercn=%d, qodercom=%d, workbuddyai=%d, qwenwork=%d, glm=%d, monkeycode=%d, raccoon=%d, loomy=%d, cline=%d, officeace=%d, kilo=%d(匿名), minimax=%d, lobsterai=%d, zcode=%d, gemini=%d, codearts=%d from %s",
		len(wbAuths), cfg.Region, len(trAuths), len(qdAuths), len(qcnAuths), len(qcmAuths), len(wbaAuths), len(qwAuths), len(glmAuths), len(mcAuths), len(rcAuths), len(lmAuths), len(clAuths), len(oaAuths), len(kiAuths), len(mimAuths), len(lobAuths), len(zcdAuths), len(gemAuths), len(caAuths), cfg.AuthDir)

	wbPool := pool.New(filepath.Join(stateDir, "state-workbuddy.json"))
	for _, a := range wbAuths {
		wbPool.Add(a)
	}
	trPool := pool.New(filepath.Join(stateDir, "state-traework.json"))
	for _, a := range trAuths {
		trPool.Add(a)
	}
	qdPool := pool.New(filepath.Join(stateDir, "state-qoder.json"))
	for _, a := range qdAuths {
		qoder.EnsureFingerprint(a) // 老凭证补机器指纹
		qdPool.Add(a)
	}
	wbaPool := pool.New(filepath.Join(stateDir, "state-workbuddyai.json"))
	for _, a := range wbaAuths {
		wbaPool.Add(a)
	}
	qwPool := pool.New(filepath.Join(stateDir, "state-qwenwork.json"))
	for _, a := range qwAuths {
		qwPool.Add(a)
	}
	qcnPool := pool.New(filepath.Join(stateDir, "state-qodercn.json"))
	for _, a := range qcnAuths {
		qodercn.EnsureFingerprint(a) // 老凭证补机器指纹
		qcnPool.Add(a)
	}
	qcmPool := pool.New(filepath.Join(stateDir, "state-qodercom.json"))
	for _, a := range qcmAuths {
		qodercom.EnsureFingerprint(a) // 老凭证补机器指纹
		qcmPool.Add(a)
	}
	// 智谱清言：与其它渠道同为多账号池（可加多个 refresh_token），
	// 但每个账号不可人工恢复（refresh_token 轮换后旧值即失效），
	// 故 Runtime 置 SingleAccount —— 见 runtimes 装配处的说明。
	glmPool := pool.New(filepath.Join(stateDir, "state-glm.json"))
	for _, a := range glmAuths {
		glmPool.Add(a)
	}
	mcPool := pool.New(filepath.Join(stateDir, "state-monkeycode.json"))
	for _, a := range mcAuths {
		mcPool.Add(a)
	}
	rcPool := pool.New(filepath.Join(stateDir, "state-raccoon.json"))
	for _, a := range rcAuths {
		rcPool.Add(a)
	}
	lmPool := pool.New(filepath.Join(stateDir, "state-loomy.json"))
	for _, a := range lmAuths {
		lmPool.Add(a)
	}
	// 挑号策略（config.strategy）统一下发到所有渠道的池。
	// 各渠道共用同一策略：策略是「怎么用账号」的全局偏好，不是渠道特性。
	// normalize 已校验过合法性，这里 ParseStrategy 不会失败。
	if st, ok := pool.ParseStrategy(cfg.Strategy); ok {
		for _, p := range []*pool.Pool{wbPool, trPool, qdPool, wbaPool, qwPool, qcnPool,
			qcmPool, glmPool, mcPool, rcPool, lmPool} {
			p.SetStrategy(st)
		}
		log.Printf("账号挑选策略：%s", st.Label())
	}
	// Cline：真多账号池（一把 Key = 一个 Cline 账号），故**不置 SingleAccount**，
	// 账号级惩罚与同请求换号（R29）全量适用。
	clPool := pool.New(filepath.Join(stateDir, "state-cline.json"))
	for _, a := range clAuths {
		clPool.Add(a)
	}
	// OfficeAce：真多账号池（每个华为账号一份凭据），不置 SingleAccount。
	oaPool := pool.New(filepath.Join(stateDir, "state-officeace.json"))
	for _, a := range oaAuths {
		oaPool.Add(a)
	}
	// oczen：整池只有一个匿名虚拟账号，state 仅用于记冷却（无积分、无签到）。
	//
	// ⚠️ 本仓已停用 Oczen 渠道：这段装配与 internal/oczen 一并保留（跟随上游，
	// 便于后续继续合并），但 serverRuntimes / Runtime 两处 map 里都没有它的条目，
	// 即无法路由、不进模型列表、不进费率面板。恢复渠道 = 解除那两处注释 +
	// 把下面启动日志里的 oczen 计数加回去。
	ocPool := pool.New(filepath.Join(stateDir, "state-oczen.json"))
	for _, a := range ocAuths {
		ocPool.Add(a)
	}
	// 安全网：匿名账号不允许被禁用或被冷却（任何历史 state 或异常路径写脏都可自愈）。
	// 冷却也要清：该渠道只有这一个账号且无号可轮换，冷却即等于整条渠道下线。
	ocPool.SetDisabled(oczen.AnonymousUID, false)
	ocPool.ClearPenalty(oczen.AnonymousUID)
	// kilo：与 oczen 同款——整池只有一个虚拟账号，state 仅用于记冷却（无积分、无签到）。
	// 同样要在启动时自愈：任何历史脏数据（冷却/禁用）都会让这条没有第二个账号的渠道直接下线。
	kiPool := pool.New(filepath.Join(stateDir, "state-kilo.json"))
	for _, a := range kiAuths {
		kiPool.Add(a)
	}
	kiPool.SetDisabled(kilo.AnonymousUID, false)
	kiPool.ClearPenalty(kilo.AnonymousUID)
	// MiniMax：真多账号池（每个 MiniMax 账号一份 OAuth 凭据），不置 SingleAccount。
	mimPool := pool.New(filepath.Join(stateDir, "state-minimax.json"))
	for _, a := range mimAuths {
		mimPool.Add(a)
	}
	// LobsterAI：真多账号池（每个有道账号一份凭据，含 keyfrom 四元组），不置 SingleAccount。
	lobPool := pool.New(filepath.Join(stateDir, "state-lobsterai.json"))
	for _, a := range lobAuths {
		lobPool.Add(a)
	}
	// ZCode：真多账号池（每个智谱账号一份凭据 + device_mid），不置 SingleAccount。
	zcdPool := pool.New(filepath.Join(stateDir, "state-zcode.json"))
	for _, a := range zcdAuths {
		zcdPool.Add(a)
	}
	// Gemini：真多账号池（每个 Google 账号一份 OAuth 凭据 + project），不置 SingleAccount。
	gemPool := pool.New(filepath.Join(stateDir, "state-gemini.json"))
	for _, a := range gemAuths {
		gemPool.Add(a)
	}
	// CodeArts：真多账号池（每个华为账号一份 AK/SK + SecurityToken），不置 SingleAccount。
	caPool := pool.New(filepath.Join(stateDir, "state-codearts.json"))
	for _, a := range caAuths {
		caPool.Add(a)
	}

	wbUp := upstream.New()
	wbUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	trUp := traework.New()
	trUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// TraeCode（代码版）：与 TraeWork 同一上游、共用账号，仅 function 不同（solo_agent）。
	trCodeUp := traework.NewTraeCode()
	trCodeUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	qdUp := qoder.New()
	qdUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	wbaUp := workbuddyai.New()
	wbaUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	qwUp := qwenwork.New()
	qwUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	qcnUp := qodercn.New()
	qcnUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	qcmUp := qodercom.New()
	qcmUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	mcUp := monkeycode.New()
	mcUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	rcUp := raccoon.New()
	rcUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	lmUp := loomy.New()
	lmUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	clUp := cline.New()
	// Cline：非流式 client 保留总超时（一问一答需要兜底）；StreamHTTP **不得**设 Timeout
	// ——R35：Client.Timeout 覆盖整个生命周期（含读 body），SSE 整个生成期都在读 body，
	// 长回答必被从中间掐断。共用 Transport 以复用连接池。
	clUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	clUp.StreamHTTP.Transport = clUp.HTTP.Transport
	oaUp := officeace.New()
	// OfficeAce 上游为华为 AgentArts 网关：恒 stream=true，长回答（含思维链）常见，
	// 非流式 client 保留总超时兜底；StreamHTTP 不设 Timeout（R35）。
	oaUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	oaUp.StreamHTTP.Transport = oaUp.HTTP.Transport
	ocUp := oczen.New()
	// kilo：免密免费池。非流式 client 保留总超时兜底；StreamHTTP 不设 Timeout（R35）。
	// New() 内两个 client 已共用同一 Transport，这里无需再赋 Transport。
	kiUp := kilo.New()
	kiUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// MiniMax：推理是 Anthropic Messages 形状且恒流式，长思考常见 ⇒ 非流式 client 留总超时兜底，
	// StreamHTTP 不设 Timeout（R35），两者共用 Transport。
	mimUp := minimax.New()
	mimUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	mimUp.StreamHTTP.Transport = mimUp.HTTP.Transport
	// LobsterAI：对话是 OpenAI 兼容面且恒流式；非流式 client 留总超时兜底，
	// StreamHTTP 不设 Timeout（R35），两者共用 Transport。
	lobUp := lobsterai.New()
	lobUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	lobUp.StreamHTTP.Transport = lobUp.HTTP.Transport
	// ZCode：Anthropic Messages 形态且恒流式；非流式 client 留总超时兜底，
	// StreamHTTP 不设 Timeout（R35），两者共用 Transport。
	zcdUp := zcode.New()
	zcdUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	zcdUp.StreamHTTP.Transport = zcdUp.HTTP.Transport
	// Gemini：自有 v1internal 信封、恒流式；非流式 client 留总超时兜底，
	// StreamHTTP 不设 Timeout（R35），两者共用 Transport。
	gemUp := gemini.New()
	gemUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	gemUp.StreamHTTP.Transport = gemUp.HTTP.Transport
	// CodeArts：对话走华为 snap-access（HMAC 签名）且长思考常见；非流式 client 留总超时兜底，
	// StreamHTTP 不设 Timeout（R35），两者共用 Transport。
	caUp := codearts.New()
	caUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	caUp.StreamHTTP.Transport = caUp.HTTP.Transport
	glmUp := glm.New()
	// ⚠️ 只改 HTTP（非流式）的 Timeout。
	// **不要**给 glmUp.StreamHTTP 设 Timeout —— 流式请求一旦有整体超时，
	// 长回答会在超时点被掐断且无终止帧（实测 2026-09-27 触发 5 次）。
	glmUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second

	// 开发期调试开关：把各渠道出站对话请求的**上游原始响应**落盘（仅 -tags devdump
	// 构建有效，见 internal/devdump）。这里只做一次启动期读取与提示；真正的接线在
	// 下方 wrapDumps（R57 起这是唯一统一改 Transport 的地方，且只在启动时做一次）。
	devdump.Refresh()
	if devdump.Enabled() {
		log.Printf("devdump enabled: 上游原始响应落盘至 %s（仅开发构建；含 token，勿外传）", os.Getenv("WILDWORK_DUMP_DIR"))
	}

	// 备用上游通道（全局单代理，**不参与常规请求**，仅渠道命中地区拒绝时自取）：
	// 只存地址，不碰任何渠道 client，故面板保存后无需热更新接线（见 transport.go 头注释）。
	applyBackupLink(cfg)

	// devdump 落盘接线表：必须覆盖**每一个**注册进 runtimes 的渠道 client。
	// 旧代理接线表就漏过 trCodeUp（两张表都漏）⇒ 同一个上游客户端变量多实例的渠道
	// （TraeCode 与 TraeWork 各一份 Client）最容易漏，由 transport_test.go 静态锁死。
	wrapDumps(map[string][]*http.Client{
		provider.WorkBuddy.String():   {wbUp.HTTP, wbUp.BillingHTTP, wbUp.StreamHTTP},
		provider.WorkBuddyAI.String(): {wbaUp.HTTP, wbaUp.StreamHTTP},
		provider.TraeWork.String():    {trUp.HTTP, trUp.StreamHTTP},
		provider.TraeCode.String():    {trCodeUp.HTTP, trCodeUp.StreamHTTP},
		provider.Qoder.String():       {qdUp.HTTP, qdUp.StreamHTTP},
		provider.QoderCN.String():     {qcnUp.HTTP, qcnUp.StreamHTTP},
		provider.QoderCOM.String():    {qcmUp.HTTP, qcmUp.StreamHTTP},
		provider.QwenWork.String():    {qwUp.HTTP, qwUp.StreamHTTP},
		provider.MonkeyCode.String():  {mcUp.HTTP, mcUp.StreamHTTP},
		provider.Raccoon.String():     {rcUp.HTTP, rcUp.StreamHTTP},
		provider.Loomy.String():       {lmUp.HTTP, lmUp.StreamHTTP},
		provider.Cline.String():       {clUp.HTTP, clUp.StreamHTTP},
		provider.OfficeAce.String():   {oaUp.HTTP, oaUp.StreamHTTP},
		provider.Oczen.String():       {ocUp.HTTP, ocUp.StreamHTTP},
		provider.Kilo.String():        {kiUp.HTTP, kiUp.StreamHTTP},
		provider.Minimax.String():     {mimUp.HTTP, mimUp.StreamHTTP},
		provider.LobsterAI.String():   {lobUp.HTTP, lobUp.StreamHTTP},
		provider.ZCode.String():       {zcdUp.HTTP, zcdUp.StreamHTTP},
		provider.Gemini.String():      {gemUp.HTTP, gemUp.StreamHTTP},
		provider.CodeArts.String():    {caUp.HTTP, caUp.StreamHTTP},
		provider.GLM.String():         {glmUp.HTTP, glmUp.StreamHTTP},
	})
	checkinMinutes, err := config.ParseClockTimes(cfg.Schedule.CheckinTimes)
	if err != nil {
		fatal("解析签到时间失败：%v", err)
	}

	// Qoder 双区签到窗口（对齐上游 qoder2api 的调度机制）：
	// 每日 10:00（UTC+8）开放，活动可能在整点之后才创建，故 10:00–12:00 每分钟重试。
	const (
		qoderCheckinMinute     = 10 * 60 // 10:00
		qoderCheckinRetryUntil = 12 * 60 // 12:00（上游同款兑底截止）
	)

	// 临期阈值（全渠道共用，scheduler/App 两侧同源，默认 24h，下限 24h）
	expiringThreshold := cfg.ExpiringThresholdDur

	// 积分流水记账器：在 scheduler 之前创建（签到后差分记账用）；
	// App 内部不重建，main 装配时统一注入。失败仅降级为无统计。
	lg, lgerr := ledger.New(filepath.Join(stateDir, "ledger"))
	if lgerr != nil {
		log.Printf("ledger init failed（统计功能不可用）: %v", lgerr)
	}

	// 成长中心自动化**只接 workbuddy**：成长接口仅 WorkBuddy 国内版提供，
	// 其它渠道调用会得到 404/不支持（调度器侧有可选接口探测 + 静默跳过兜底）。
	// 开关默认关闭，见 config.schedule 的 growth_* 项。
	wbSch := scheduler.New(scheduler.Config{
		Pool: wbPool, Upstream: wbUp, Name: "workbuddy",
		CheckinMinutes: checkinMinutes, KeepaliveHours: cfg.Schedule.KeepaliveHours,
		ExpiringThreshold: expiringThreshold, Ledger: lg,
		GrowthRedeemStreak:   cfg.Schedule.GrowthRedeemStreak,
		GrowthTravel:         cfg.Schedule.GrowthTravel,
		GrowthTravelLocation: cfg.Schedule.GrowthTravelLocation,
		GrowthTasks:          cfg.Schedule.GrowthTasks,
		GrowthTasksIncludeMP: cfg.Schedule.GrowthTasksIncludeMP,
	})
	// TraeWork：开启限流延迟重试。
	//
	// 依据 2026-10-12 实测：claim 在高峰会返回 9074（"当前参与用户太多"），
	// 单次调用内虽然已有换设备号重试（最多 8 个号 / 约 1 分钟），高峰期仍可能
	// 持续更久。若不安排延迟重试，本时段不会标记完成，只能干等到下一个签到
	// 时刻（默认 21:00）—— 当天就这么丢了（当天 09:00 撞限流，14:26 手动才补领）。
	// TraeWork 是唯一有账号级限流的渠道，故只在此打开。
	trSch := scheduler.New(scheduler.Config{Pool: trPool, Upstream: trUp, Name: "traework", CheckinMinutes: checkinMinutes, KeepaliveHours: cfg.Schedule.KeepaliveHours, ExpiringThreshold: expiringThreshold, Ledger: lg, RetryOnFailure: true})
	// Qoder 无签到活动：调度器只做 token keepalive（每日 refresh 保活）。
	// CheckinMinutes 用显式空切片关闭（传 nil 会被 scheduler.New 补成默认 9:00/21:00）。
	qdSch := scheduler.New(scheduler.Config{Pool: qdPool, Upstream: qdUp, Name: "qoder", CheckinMinutes: []int{}, KeepaliveHours: cfg.Schedule.KeepaliveHours, ExpiringThreshold: expiringThreshold, Ledger: lg})
	// WorkBuddy 国际版：无显式签到（ActivitiesOnly 模式）。
	// 定时仍对每个账号调用 DailyCheckin，其实现为「免费模型对话保活 + 签到探测」；
	// 不记录/上报签到状态，保持对用户透明。
	// Keepalive 关闭（token 有效期 365 天，无需每日刷新），用显式空切片关闭
	// （传 nil 会被 scheduler.New 补成默认 22:00）。
	wbaSch := scheduler.New(scheduler.Config{Pool: wbaPool, Upstream: wbaUp, Name: "workbuddyai",
		CheckinMinutes: checkinMinutes, KeepaliveHours: []int{}, ActivitiesOnly: true, ExpiringThreshold: expiringThreshold, Ledger: lg})
	// 千问办公：无签到活动（每日积分服务端被动发放，无需保活/领取）；
	// CheckinMinutes + KeepaliveHours 均为显式空切片，本渠道没有任何定时任务
	// （token 由 deviceToken/refresh 按需轮换，定时保活反而会与千问办公 App
	// 互踩 —— 见备忘 §7.5 风险 1）。
	// 余额/费率靠 StartCreditAutoRefresh 循环拉取。
	qwSch := scheduler.New(scheduler.Config{Pool: qwPool, Upstream: qwUp, Name: "qwenwork",
		CheckinMinutes: []int{}, KeepaliveHours: []int{}, ExpiringThreshold: expiringThreshold, Ledger: lg})
	// QoderCN：仅 campaigns 活动路径（legacy daily-check-in 已 DISABLED，其 claim 恒返回
	// 409 会造成「假成功」，故不再使用）。签到窗口对齐上游 qoder2api：10:00 开放，
	// 活动可能在整点后才创建，故 10:00–12:00 之间每分钟重试，直到真正领到或超时。
	qcnSch := scheduler.New(scheduler.Config{Pool: qcnPool, Upstream: qcnUp, Name: "qodercn", CheckinMinutes: []int{qoderCheckinMinute}, CheckinRetryUntil: qoderCheckinRetryUntil, KeepaliveHours: cfg.Schedule.KeepaliveHours, ExpiringThreshold: expiringThreshold, Ledger: lg})
	// QoderCOM：仅 campaigns 活动路径（无 daily-check-in）；其余同 QoderCN。
	qcmSch := scheduler.New(scheduler.Config{Pool: qcmPool, Upstream: qcmUp, Name: "qodercom", CheckinMinutes: []int{qoderCheckinMinute}, CheckinRetryUntil: qoderCheckinRetryUntil, KeepaliveHours: cfg.Schedule.KeepaliveHours, ExpiringThreshold: expiringThreshold, Ledger: lg})
	// MonkeyCode：已实现签到（控制台 Cookie 走 POST {console}/api/v1/users/wallet/checkin，
	// 见 internal/monkeycode/checkin.go）。签到时刻用全局配置；**不设 CheckinRetryUntil**
	// ——它是「直接领一次」的接口，没有 Qoder 那种「活动整点后才创建」的延迟，
	// 每个签到时刻单次尝试即可（失败状态会在下一个时刻重试），避免窗口内每分钟空跑。
	// KeepaliveHours 仍为显式空切片：上游无 refresh 端点，保活没有可刷的东西。
	mcSch := scheduler.New(scheduler.Config{Pool: mcPool, Upstream: mcUp, Name: "monkeycode",
		CheckinMinutes: checkinMinutes, KeepaliveHours: []int{}, ExpiringThreshold: expiringThreshold, Ledger: lg})
	// 小浣熊：签到走 setting_info（每日积分惰性落账的触发点，见 internal/raccoon/checkin.go）。
	// access_token 实测仅 ≈2h，而 refresh_token ≈30 天，
	// 故保活比其它渠道更密（每 4 小时一次）以减少「请求先 401 再刷新」的额外往返；
	// 请求路径上的 refreshIfSessionDead 仍会兜底自愈。
	//
	// 签到时间用全局配置（checkinMinutes）：上游按天去重、无活动上线延迟问题，
	// 故**不设 CheckinRetryUntil**（窗口重试是为「活动尚未创建」设计的，本渠道无此语义）。
	rcSch := scheduler.New(scheduler.Config{Pool: rcPool, Upstream: rcUp, Name: "raccoon",
		CheckinMinutes: checkinMinutes, KeepaliveHours: []int{1, 5, 9, 13, 17, 21}, ExpiringThreshold: expiringThreshold, Ledger: lg})
	// Loomy：已实现签到（宠物每日打工奖励，POST /api/v1/pet-work/rewards，
	// 见 internal/loomy/checkin.go）——判据是该账号当日**有效对话轮次**（上游算），
	// 故未在官方客户端用过该账号时结果是「未达档位」（如实可重试状态）。
	// 签到时刻用全局配置、**不设 CheckinRetryUntil**：档位在一天里是渐进的，
	// 窗口内每分钟重试既无意义又违反 §0.1；每个签到时刻单次尝试即可。
	// KeepaliveHours 仍为显式空切片：**上游无 refresh 端点**（session 14 天，到期需重新登录/导入），
	// 保活没有可刷的东西（refreshToken 为空，调度器会跳过并只记一条日志）。
	lmSch := scheduler.New(scheduler.Config{Pool: lmPool, Upstream: lmUp, Name: "loomy",
		CheckinMinutes: checkinMinutes, KeepaliveHours: []int{}, ExpiringThreshold: expiringThreshold, Ledger: lg})
	// Cline：官方 API Key **不过期**、无签到活动、无 refresh token ⇒ 定时任务全部关闭。
	// 必须传显式空切片 []int{}（R39）：传 nil 会被 scheduler.New 的零值兜底补上默认时间，
	// 于是每天空跑一次必然失败的签到，并在面板渲染红色「签到失败」。
	clSch := scheduler.New(scheduler.Config{Pool: clPool, Upstream: clUp, Name: "cline",
		CheckinMinutes: []int{}, KeepaliveHours: []int{}, ExpiringThreshold: expiringThreshold, Ledger: lg})
	// OfficeAce：有每日签到（bonus/claim）但要先有临时凭据（V11 签名），保活用于续期。
	// CheckinMinutes 用全局签到时间；KeepaliveHours 用配置保活时段（临时凭据约 2h 到期）。
	oaSch := scheduler.New(scheduler.Config{Pool: oaPool, Upstream: oaUp, Name: "officeace",
		CheckinMinutes: checkinMinutes, KeepaliveHours: cfg.Schedule.KeepaliveHours,
		ExpiringThreshold: expiringThreshold, Ledger: lg})
	// OpenCodeZen 匿名：无账号、无签到、无 token 可保活（凭证是常量 public）。
	// CheckinMinutes/KeepaliveHours 均为显式空切片 → 调度器不发生任何上游调用。
	// 本项目**停用**该渠道（不登记 Runtime，见下方 map 注释），故调度器建好后不 Run。
	ocSch := scheduler.New(scheduler.Config{Pool: ocPool, Upstream: ocUp, Name: "oczen",
		CheckinMinutes: []int{}, KeepaliveHours: []int{}})
	_ = ocSch // 停用占位：恢复渠道时改为 go ocSch.Run(ctx) 并解除 map 注释
	// Kilo 免费池：无账号、无签到、无 token 可保活 ⇒ 定时任务全关。
	// 必须传**显式空切片**（R39）：传 nil 会被 scheduler.New 当作「未配置」补成
	// 默认 9:00/21:00 签到与 22:00 保活，每天空跑两次必然失败的调用。
	kiSch := scheduler.New(scheduler.Config{Pool: kiPool, Upstream: kiUp, Name: "kilo",
		CheckinMinutes: []int{}, KeepaliveHours: []int{}})
	// MiniMax：有每日签到（7 天面板，claim 幂等判据是响应里的 claim_result），
	// 也需要 token 保活（refresh_token 会轮换，刷新后必须落盘）。
	mimSch := scheduler.New(scheduler.Config{Pool: mimPool, Upstream: mimUp, Name: "minimax",
		CheckinMinutes: checkinMinutes, KeepaliveHours: cfg.Schedule.KeepaliveHours,
		ExpiringThreshold: expiringThreshold, Ledger: lg})
	// LobsterAI：有每日签到（三步：slot → context → check_in，客户端幂等键），
	// 也需要 token 保活（refresh 会轮换，刷新后必须落盘）。
	lobSch := scheduler.New(scheduler.Config{Pool: lobPool, Upstream: lobUp, Name: "lobsterai",
		CheckinMinutes: checkinMinutes, KeepaliveHours: cfg.Schedule.KeepaliveHours,
		ExpiringThreshold: expiringThreshold, Ledger: lg})
	// ZCode：**无签到**（积分领取依赖阿里云验证码 + headful 浏览器，本仓不实现；
	// DailyCheckin 返回明确的「不支持」错误而不是伪造成功），且凭据**不可续期**
	// （JWT 无 exp）⇒ 定时任务全关。必须传显式空切片（R39）：传 nil 会被补成默认
	// 9:00/21:00 签到与 22:00 保活，每天空跑两次必然失败的调用并污染面板。
	zcdSch := scheduler.New(scheduler.Config{Pool: zcdPool, Upstream: zcdUp, Name: "zcode",
		CheckinMinutes: []int{}, KeepaliveHours: []int{}})
	// Gemini：**无签到**（DailyCheckin 返回明确的「不支持」错误），但有 refresh_token 可续期
	// ⇒ 只开保活；CheckinMinutes 用显式空切片关闭（R39：传 nil 会被补成默认 9:00/21:00，
	// 每天空跑两次必然失败的签到并污染面板）。
	gemSch := scheduler.New(scheduler.Config{Pool: gemPool, Upstream: gemUp, Name: "gemini",
		CheckinMinutes: []int{}, KeepaliveHours: cfg.Schedule.KeepaliveHours,
		ExpiringThreshold: expiringThreshold, Ledger: lg})
	// CodeArts：有每日签到（三步，走 HMAC 签名；本包实现了 CheckinReporter/CheckinGranter，
	// 签到结果按结构化状态上报），也需要 token 保活（续期后必须落盘，不变量 20）。
	caSch := scheduler.New(scheduler.Config{Pool: caPool, Upstream: caUp, Name: "codearts",
		CheckinMinutes: checkinMinutes, KeepaliveHours: cfg.Schedule.KeepaliveHours,
		ExpiringThreshold: expiringThreshold, Ledger: lg})
	// 智谱清言：签到与对话联动（当日需先对话才能领打卡进度），故保活与签到
	// 合成一个流程（glm.Client.Keepalive 先发最小对话再签到）。
	// 用 DailyCheckin 通道驱动：CheckinMinutes 用全局签到时间，
	// KeepaliveHours 也用同一时段（保活=签到，不必额外一次）。
	glmSch := scheduler.New(scheduler.Config{Pool: glmPool, Upstream: glmUp, Name: "glm",
		CheckinMinutes: checkinMinutes, KeepaliveHours: cfg.Schedule.KeepaliveHours,
		ExpiringThreshold: expiringThreshold, Ledger: lg})

	runtimes := map[provider.Kind]*server.Runtime{
		provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: wbPool, Upstream: wbUp, StaticModels: server.WorkBuddyStaticModels()},
		provider.WorkBuddyAI: {Kind: provider.WorkBuddyAI, Pool: wbaPool, Upstream: wbaUp, StaticModels: workbuddyai.StaticModels(),
			// 国际版网关实测间歇性 502/503/504，账号本身健康，不计入账号错误
			NoCooldownOnServerError: true},
		provider.TraeWork: {Kind: provider.TraeWork, Pool: trPool, Upstream: trUp, StaticModels: server.TraeWorkStaticModels()},
		// TraeCode 与 TraeWork 共享 trPool（同一账号体系，避免 refresh token 轮换冲突），
		// 仅上游客户端不同（function=solo_agent）。
		provider.TraeCode:   {Kind: provider.TraeCode, Pool: trPool, Upstream: trCodeUp, StaticModels: server.TraeCodeStaticModels()},
		provider.Qoder:      {Kind: provider.Qoder, Pool: qdPool, Upstream: qdUp, StaticModels: qoder.StaticModels()},
		provider.QoderCN:    {Kind: provider.QoderCN, Pool: qcnPool, Upstream: qcnUp, StaticModels: qodercn.StaticModels()},
		provider.QoderCOM:   {Kind: provider.QoderCOM, Pool: qcmPool, Upstream: qcmUp, StaticModels: qodercom.StaticModels()},
		provider.QwenWork:   {Kind: provider.QwenWork, Pool: qwPool, Upstream: qwUp, StaticModels: qwenwork.StaticModels()},
		provider.MonkeyCode: {Kind: provider.MonkeyCode, Pool: mcPool, Upstream: mcUp, StaticModels: monkeycode.StaticModels()},
		provider.Raccoon:    {Kind: provider.Raccoon, Pool: rcPool, Upstream: rcUp, StaticModels: raccoon.StaticModels()},
		provider.Loomy:      {Kind: provider.Loomy, Pool: lmPool, Upstream: lmUp, StaticModels: loomy.StaticModels()},
		provider.GLM:        {Kind: provider.GLM, Pool: glmPool, Upstream: glmUp, StaticModels: glm.StaticModels()},
		provider.Cline:      {Kind: provider.Cline, Pool: clPool, Upstream: clUp, StaticModels: cline.StaticModels()},
		provider.OfficeAce:  {Kind: provider.OfficeAce, Pool: oaPool, Upstream: oaUp, StaticModels: officeace.StaticModels()},
		// 注意：GLM **不**置 SingleAccount。
		//
		// 早期误判：以为「凭据轮换后不可人工恢复」就该按单账号处理。但
		// ① 现在可用 CDP 自动登录随时补账号；② 单账号语义会**跳过所有账号级惩罚**，
		// 导致账号 A 失效/限流时不会切到账号 B —— 多账号完全失效（实测确认）。
		// 故按正常多账号渠道处理：错误分类惩罚 + 池内轮换。
		//
		// 唯一真实约束是「refresh_token 轮换必须落盘」，已由 glm.RefreshToken 保证。
		// oczen：**本项目刻意不登记**（停用）。匿名免费通道的免费档不受零留存政策保护，
		// 故不对面板暴露、不接受 oczen/* 路由。代码与包保留（上游仍在维护），
		// 恢复方式：把下面两行注释解除即可（serverRuntimes 与 appRuntimes 各一处）。
		// provider.Oczen: {Kind: provider.Oczen, Pool: ocPool, Upstream: ocUp, StaticModels: oczen.StaticModels(),
		// 	SingleAccount: true, NoCooldownOnServerError: true},
		// kilo：同为无账号渠道（免密免费池，虚拟账号不可重登），语义与 oczen 一致——
		// 任何账号级惩罚（冷却/计数/禁用）都等于整条渠道下线，故声明 SingleAccount，
		// 由 handler 对所有错误一律原文透传。
		provider.Kilo: {Kind: provider.Kilo, Pool: kiPool, Upstream: kiUp, StaticModels: kilo.StaticModels(),
			SingleAccount: true, NoCooldownOnServerError: true},
		// MiniMax：真多账号渠道 ⇒ 不置 SingleAccount（普通错误分类 + 池内轮换都适用）。
		provider.Minimax: {Kind: provider.Minimax, Pool: mimPool, Upstream: mimUp, StaticModels: minimax.StaticModels()},
		// LobsterAI：真多账号渠道 ⇒ 不置 SingleAccount（错误分类 + 池内轮换都适用）。
		provider.LobsterAI: {Kind: provider.LobsterAI, Pool: lobPool, Upstream: lobUp, StaticModels: lobsterai.StaticModels()},
		// ZCode：真多账号渠道 ⇒ 不置 SingleAccount（错误分类 + 池内轮换都适用）。
		provider.ZCode: {Kind: provider.ZCode, Pool: zcdPool, Upstream: zcdUp, StaticModels: zcode.StaticModels()},
		// Gemini：真多账号渠道 ⇒ 不置 SingleAccount。
		provider.Gemini: {Kind: provider.Gemini, Pool: gemPool, Upstream: gemUp, StaticModels: gemini.StaticModels()},
		// CodeArts：真多账号渠道 ⇒ 不置 SingleAccount。
		provider.CodeArts: {Kind: provider.CodeArts, Pool: caPool, Upstream: caUp, StaticModels: codearts.StaticModels()},
	}
	appRuntimes := map[provider.Kind]*app.Runtime{
		provider.WorkBuddy:   {Kind: provider.WorkBuddy, Pool: wbPool, Upstream: wbUp, Scheduler: wbSch},
		provider.WorkBuddyAI: {Kind: provider.WorkBuddyAI, Pool: wbaPool, Upstream: wbaUp, Scheduler: wbaSch},
		provider.TraeWork:    {Kind: provider.TraeWork, Pool: trPool, Upstream: trUp, Scheduler: trSch},
		provider.TraeCode:    {Kind: provider.TraeCode, Pool: trPool, Upstream: trCodeUp, Alias: true}, // 别名渠道：无独立 Scheduler，账号聚合/签到/刷新均由 TraeWork 负责
		provider.Qoder:       {Kind: provider.Qoder, Pool: qdPool, Upstream: qdUp, Scheduler: qdSch},
		provider.QoderCN:     {Kind: provider.QoderCN, Pool: qcnPool, Upstream: qcnUp, Scheduler: qcnSch},
		provider.QoderCOM:    {Kind: provider.QoderCOM, Pool: qcmPool, Upstream: qcmUp, Scheduler: qcmSch},
		provider.QwenWork:    {Kind: provider.QwenWork, Pool: qwPool, Upstream: qwUp, Scheduler: qwSch},
		provider.MonkeyCode: {Kind: provider.MonkeyCode, Pool: mcPool, Upstream: mcUp, Scheduler: mcSch},
		provider.Raccoon:    {Kind: provider.Raccoon, Pool: rcPool, Upstream: rcUp, Scheduler: rcSch},
		provider.Loomy:      {Kind: provider.Loomy, Pool: lmPool, Upstream: lmUp, Scheduler: lmSch},
		provider.GLM:        {Kind: provider.GLM, Pool: glmPool, Upstream: glmUp, Scheduler: glmSch},
		provider.Cline:      {Kind: provider.Cline, Pool: clPool, Upstream: clUp, Scheduler: clSch},
		provider.OfficeAce:  {Kind: provider.OfficeAce, Pool: oaPool, Upstream: oaUp, Scheduler: oaSch},
		// provider.Oczen:      {Kind: provider.Oczen, Pool: ocPool, Upstream: ocUp, Scheduler: ocSch}, // 停用，见 serverRuntimes 同名说明
		provider.Kilo:       {Kind: provider.Kilo, Pool: kiPool, Upstream: kiUp, Scheduler: kiSch},
		provider.Minimax:    {Kind: provider.Minimax, Pool: mimPool, Upstream: mimUp, Scheduler: mimSch},
		provider.LobsterAI:  {Kind: provider.LobsterAI, Pool: lobPool, Upstream: lobUp, Scheduler: lobSch},
		provider.ZCode:      {Kind: provider.ZCode, Pool: zcdPool, Upstream: zcdUp, Scheduler: zcdSch},
		provider.Gemini:     {Kind: provider.Gemini, Pool: gemPool, Upstream: gemUp, Scheduler: gemSch},
		provider.CodeArts:   {Kind: provider.CodeArts, Pool: caPool, Upstream: caUp, Scheduler: caSch},
	}

	initCustomServerRts, initCustomAppRts, customNames := buildCustomRuntimes(cfg.CustomChannels, stateDir)
	for k, rt := range initCustomAppRts {
		appRuntimes[k] = rt
	}

	appInst, err := app.New(app.Options{
		ConfigPath: cfgPath,
		Config:     cfg,
		Runtimes:   appRuntimes,
		Ledger:     lg,
	})
	if err != nil {
		fatal("初始化失败：%v", err)
	}
	defer appInst.Close()

	// 调度器结果写日志（无 GUI 推送，日志即面板数据源）
	wbSch.SetCheckinObserver(func(r scheduler.CheckinResult) { appInst.NotifyCheckin("workbuddy", r) })
	trSch.SetCheckinObserver(func(r scheduler.CheckinResult) { appInst.NotifyCheckin("traework", r) })
	qdSch.SetCheckinObserver(func(r scheduler.CheckinResult) { appInst.NotifyCheckin("qoder", r) })
	wbSch.SetRefreshObserver(func(uid string, ok bool, msg string) { appInst.NotifyRefresh("workbuddy", uid, ok, msg) })
	trSch.SetRefreshObserver(func(uid string, ok bool, msg string) { appInst.NotifyRefresh("traework", uid, ok, msg) })
	qdSch.SetRefreshObserver(func(uid string, ok bool, msg string) { appInst.NotifyRefresh("qoder", uid, ok, msg) })
	wbaSch.SetCheckinObserver(func(r scheduler.CheckinResult) { appInst.NotifyCheckin("workbuddyai", r) })
	wbaSch.SetRefreshObserver(func(uid string, ok bool, msg string) { appInst.NotifyRefresh("workbuddyai", uid, ok, msg) })
	qwSch.SetRefreshObserver(func(uid string, ok bool, msg string) { appInst.NotifyRefresh("qwenwork", uid, ok, msg) })
	glmSch.SetCheckinObserver(func(r scheduler.CheckinResult) { appInst.NotifyCheckin("glm", r) })
	glmSch.SetRefreshObserver(func(uid string, ok bool, msg string) { appInst.NotifyRefresh("glm", uid, ok, msg) })
	// 小浣熊签到已接入（setting_info 触发每日积分落账，见 internal/raccoon/checkin.go）：
	// 结果必须写日志——面板的「签到成功/失败」标签与 stats 的签到收入都读日志。
	rcSch.SetCheckinObserver(func(r scheduler.CheckinResult) { appInst.NotifyCheckin("raccoon", r) })
	rcSch.SetRefreshObserver(func(uid string, ok bool, msg string) { appInst.NotifyRefresh("raccoon", uid, ok, msg) })
	oaSch.SetCheckinObserver(func(r scheduler.CheckinResult) { appInst.NotifyCheckin("officeace", r) })
	oaSch.SetRefreshObserver(func(uid string, ok bool, msg string) { appInst.NotifyRefresh("officeace", uid, ok, msg) })

	// HTTP handler：OpenAI 端点 + Web UI + 管理 API
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		fatal("embed web: %v", err)
	}
	inner := server.NewHandler(server.Config{
		Runtimes:  runtimes,
		APIKey:    cfg.APIKey,
		ModelRate: appInst.ModelRate,
		Ledger:    appInst.Ledger(),
		// 会话粘性：同一 conversationId 恒定走同一账号 → 上游 prompt 缓存命中，
		// 且上下文里的工具调用 ID / assistant offset 跨轮对得上。
		// 客户端不发会话 id 时自动退化为「只有请求计数粘性」的旧行为。
		SessionEnabled:    cfg.SessionSticky,
		SessionTTL:        cfg.SessionStickyTTLDur,
		SessionGCInterval: cfg.SessionStickyGCDur,
		Stats:             appInst.Stats(), // 转发路径插桩 AddUsage/AddRequestRow（运行统计）
		HardCooldown:      cfg.HardCreditDur,
		// 硬冷却截到每日发奖窗口（方案 B）：qwenwork 每日 00:00 UTC+8 发额度，
		// 账本实测 00:01~00:20 到账。固定 12h 会把 19:07 的 402 锁到次日 07:07，
		// 白锁 7 小时；按窗口截断后 00:30 即解冻，额度一到账就能用。
		HardCooldownFor:  cfg.HardCooldownFor,
		SoftCooldown:     cfg.SoftRateDur,
		ErrThreshold:     cfg.Cooldown.ErrThresh,
		ErrCooldown:      cfg.ErrCooldownDur,
		MaxRotate:        cfg.MaxRotate,
		ModelFallbacks:   cfg.ModelFallback,    // 启动时载入降级链（此前仅面板保存后热更新，重启即丢）
		ReasoningEfforts: cfg.ReasoningEfforts, // 启动时载入思考强度映射（同上）
		WebUI:            sub,
		AttachAPI:        appInst.HandleAPI,
	})
	inner.SetCustomRuntimes(initCustomServerRts)

	// 两层结构：外层兼容层只接管三个新端点，其余（含 /v1/chat/completions、Web UI、
	// 管理 API）原样落到内层 handler，旧客户端的调用栈完全不变。
	// 渠道清单注入 Router，用于校验模型名前缀并给出可读报错。
	var builtinChannels []string
	for k := range runtimes {
		builtinChannels = append(builtinChannels, k.String())
	}

	// chMu/channels 持有「当前生效的完整渠道清单」（内建 + 启用中的自定义），供
	// compat syncer 与 custom syncer 共用（channelList 类型，便于单测锁住）。
	// 为什么必须是可变共享态而不能各自用启动快照：compat syncer 与 custom syncer 分属
	// 不同保存路径，若 compat syncer 闭包捕获启动快照，则「先加自定义渠道、
	// 后单改模型映射并保存」会把清单重置回启动态 ⇒ 新渠道被丢掉，/v1/responses 与
	// /v1/messages 报 unknown channel（R61 同族的「只实现一条通道」缺陷）。
	chList := &channelList{builtin: builtinChannels, custom: customNames}
	currentChannels := chList.snapshot

	compat := gateway.New(gateway.Config{
		Inner:  inner,
		APIKey: cfg.APIKey,
		Router: gateway.Router{
			Default:  cfg.Compat.DefaultChannel,
			Map:      cfg.Compat.ModelMap,
			Channels: gateway.SortChannels(currentChannels()),
		},
		MaxTokensCap: cfg.Compat.MaxTokensCap,
	})
	// 面板保存 compat 时热更新兼容层路由表（不然新映射要重启才生效）
	appInst.SetCompatSyncer(func(defaultChannel string, maxTokensCap int, modelMap map[string]string) {
		compat.SetCompat(defaultChannel, maxTokensCap, modelMap, currentChannels())
	})
	// 面板保存模型降级链时热更新内层 handler（不然新链要重启才生效）
	appInst.SetModelFallbackSyncer(func(rules []config.ModelFallbackRule) {
		inner.SetModelFallbacks(rules)
	})
	// 面板保存思考强度映射时热更新内层 handler（不然新映射要重启才生效）
	appInst.SetReasoningEffortsSyncer(func(efforts map[string]string) {
		inner.SetReasoningEfforts(efforts)
	})
	// 面板保存自定义渠道时热更新内层 handler、app 运行时与兼容层路由表（R66）
	appInst.SetCustomChannelsSyncer(func(customChannels []custom.ChannelConfig) {
		newServerRts, newAppRts, newNames := buildCustomRuntimes(customChannels, stateDir)
		inner.SetCustomRuntimes(newServerRts)
		appInst.UpdateCustomRuntimes(newAppRts)

		// 同步更新共享渠道清单（下一步 compat.SetCompat 用同一份，避免两条通道打架）
		chList.setCustom(newNames)

		compat.SetCompat(cfg.Compat.DefaultChannel, cfg.Compat.MaxTokensCap, cfg.Compat.ModelMap, currentChannels())
		inner.InvalidateModels()
	})
	// 备用上游通道无需热更新接线：面板保存走 app.SetBackupProxy → backuplink.Set（只换指针），
	// 渠道在命中地区拒绝时按需自取（R57），因此不存在旧的「两张接线表不一致」失效形态。
	// 面板保存 oczen key 后热更新渠道凭证；启动时也应用一次配置中的初始 key
	appInst.SetOczenSyncer(ocUp.SetAPIKey)
	ocUp.SetAPIKey(cfg.OczenAPIKey)
	compat.SetAPIKeySource(inner.CurrentAPIKey) // 面板改 API-Key 后，兼容层立即跟随
	mux := http.NewServeMux()
	compat.Routes(mux) // POST /v1/responses · /v1/messages · /v1/messages/count_tokens · /v1/systemone
	mux.Handle("/", inner)
	if compat != nil {
		log.Printf("三接口兼容层已启用：default_channel=%q max_tokens_cap=%d model_map=%d 条",
			cfg.Compat.DefaultChannel, cfg.Compat.MaxTokensCap, len(cfg.Compat.ModelMap))
	}
	// 安全响应头 + 访问日志包在**整个 mux 外层**：兼容层的四个端点由外层
	// 直接注册，只挂在内层 Handler 上会漏掉它们。
	appInst.SetHandler(inner)
	appInst.SetRootHandler(server.SecurityHeaders(mux, log.Printf))

	if err := appInst.StartServer(); err != nil {
		// 监听失败必须终止：只记日志继续跑会让调度器与托盘照常起来，
		// 表现为「面板看着一切正常、其实是别的实例在服务本端口」，
		// 而两个进程共享同一数据目录，会重复签到并互相覆盖刷新出的 token。
		fatal("监听 %s 失败：%v\n\n该端口可能已被占用（例如已有一个 workbuddy-api 实例在运行）。\n"+
			"请先退出已有实例，或改用 config.json 的 listen.port（也可用环境变量 WILDWORK_LISTEN）。",
			cfg.Listen.Addr(), err)
	}

	// 调度器后台运行
	sctx, stop := context.WithCancel(context.Background())
	defer stop()
	go wbSch.Run(sctx)
	go trSch.Run(sctx)
	go qdSch.Run(sctx)
	go wbaSch.Run(sctx)
	go qwSch.Run(sctx)
	go qcnSch.Run(sctx)
	go qcmSch.Run(sctx)
	go mcSch.Run(sctx)
	go rcSch.Run(sctx)
	go lmSch.Run(sctx)
	// ⚠️ 新增渠道时必须在这里补 Run()。
	// 漏了**不会报错**——只表现为「该渠道的自动签到/保活从不运行」，
	// 而手工触发（面板按钮 / RunCheckinNow）仍然可用，极难发现。
	go glmSch.Run(sctx)
	// Cline 的 Run 同样不可漏（R34：漏了不报错，只表现为该渠道定时逻辑从不运行）。
	// 本渠道任务全关，Run 只会阻塞等 ctx/配置变更，但缺了它将来加任务即静默失效。
	go clSch.Run(sctx)
	// OfficeAce 的 Run 不可漏（R34）：本渠道有签到与保活。
	go oaSch.Run(sctx)
	// kilo 的 Run 同样不可漏（R34）：本渠道当前无任何定时任务，Run 只会阻塞等
	// ctx/配置变更；但漏了它，将来给它加任务就会静默不跑。
	go kiSch.Run(sctx)
	// MiniMax 的 Run 不可漏（R34）：本渠道有签到与保活两条定时任务。
	go mimSch.Run(sctx)
	// LobsterAI 的 Run 不可漏（R34）：本渠道有签到与保活两条定时任务。
	go lobSch.Run(sctx)
	// ZCode 的 Run 同样不可漏（R34）：本渠道当前无定时任务，但漏了将来加任务即静默失效。
	go zcdSch.Run(sctx)
	// Gemini 的 Run 不可漏（R34）：本渠道有 token 保活任务。
	go gemSch.Run(sctx)
	// CodeArts 的 Run 不可漏（R34）：本渠道有签到与保活两条定时任务。
	go caSch.Run(sctx)

	// 积分自动刷新覆盖全部渠道：
	// - workbuddyai / qoder 无签到活动，不自动刷就会一直显示旧值或 0；
	// - traework / workbuddy(CN) 虽有签到顺带拉余额，但一天只有两次，
	//   其间 token 若在别处被轮换（401）也无法自愈；统一纳入循环才能
	//   启动即出真实拆分数字，并靠 401 自愈（refreshIfSessionDead）及时恢复。
	// oczen 与 kilo 不在其中：两条都是免密渠道（无账号、无积分可刷，面板显示「不适用」），
	// 其 FetchModelPricing 已在费率刷新路径单独覆盖。
	// glm 纳入：其凭据本体是 refresh_token（accessToken 可为空），
	// 积分靠 member_info 拉取，不刷就永远是 0。
	appInst.StartCreditAutoRefresh(sctx, []provider.Kind{
		// 旧 Qoder 渠道已下线：不自动刷积分/token（避免周期性 token refresh failed 噪音）
		provider.WorkBuddy, provider.WorkBuddyAI, provider.TraeWork, provider.QoderCN, provider.QoderCOM, provider.QwenWork, provider.GLM,
		provider.MonkeyCode,
		provider.Raccoon,
		provider.Loomy,
		// MiniMax 有 credit/details 额度端点：不纳入就永远显示 0（面板看不到真实余额）。
		provider.Minimax,
		// LobsterAI 有 profile-summary 额度端点，同理必须纳入。
		provider.LobsterAI,
		// ZCode 的额度是 **token 口径**（credits_unit="token"）：纳入刷新是为了让面板能显示
		// Token 用量（用户 2026-10-17 的明确要求）；它的数字不进积分小计与积分流水
		// —— 由 app.creditsUnit 分流保证（积分流水跳过 token 渠道，条目一律 InfoOnly）。
		provider.ZCode,
		// Gemini **不**纳入：它的 UserResource 恒 0——真实信息是「配额窗口剩余百分比」，
		// 已按 InfoOnly 呈现在明细里、不参与积分算术，纳入只会刷出一个无意义的 0。
		// CodeArts 有 benefit 额度（按 UTC+8 自然日重置）与积分账户形态，必须纳入。
		provider.CodeArts,
	}, app.CreditRefreshInterval)

	// 启动即刷新「模型列表 + 费率」，之后每 30 分钟。
	// 否则刚启动时费率缓存为空，面板下方全是 unknown（需手动点刷新才正常）。
	appInst.StartPricingAutoRefresh(sctx, app.PricingRefreshInterval)

	// 运行统计喂入器（账号状态差值/粘性路由/今日作废，30s 一轮）：
	// 进程内直调各数据源，面板「运行统计」tab 即时可用。
	appInst.StartStatsFeeders(sctx)

	// 会话粘性的后台 TTL 清理。
	//
	// 为什么必须显式 Start：Router 的 GC 是 goroutine，不启动的话过期绑定永远滞留，
	// 分配时「空闲账号」集合会越来越小，最终退化成全池哈希（空闲号再也用不上）。
	// 关停由 sctx 取消 → StopGC 幂等，进程退出时自然结束。
	inner.StartSessionGC()
	// 与 sctx 一起收口：所有关停路径都经过 stop()（含 defer 与各信号分支），
	// 故 GC goroutine 不会在 handler 已不可用后仍空转。
	go func() {
		<-sctx.Done()
		inner.StopSessionGC()
	}()
	if cfg.SessionSticky {
		log.Printf("会话粘性已开启：同一会话绑定有效期 %s，清理间隔 %s", cfg.SessionStickyTTLDur, cfg.SessionStickyGCDur)
	}

	// 启动提示（非 --autostart）：系统通知
	if !autostart && !noTray {
		platform.Notify("workbuddy-api 已启动",
			fmt.Sprintf("OpenAI 兼容 API 地址：\nhttp://%s:%d\n\n点击右下角托盘图标或菜单打开主界面。", displayHost(cfg), cfg.Listen.Port))
	}

	if noTray {
		// 无头模式：打印信息，阻塞等待信号
		addr := cfg.Listen.Addr()
		fmt.Printf("workbuddy-api headless mode started\n")
		fmt.Printf("  OpenAI API:    http://%s:%d/v1\n", displayHost(cfg), cfg.Listen.Port)
		fmt.Printf("  API-Key:       %s\n", cfg.APIKey)
		fmt.Printf("  Web UI (本机): http://%s:%d/\n", openHost(cfg), cfg.Listen.Port)
		fmt.Printf("  Listen:        %s\n", addr)
		if appInst.PanelAuthEnabled() {
			fmt.Printf("  Panel auth:    admin_password enabled (cookie session, 7d)\n")
		}
		fmt.Printf("  Accounts:      workbuddy=%d traework=%d qoder=%d kilo=%d(匿名) minimax=%d lobsterai=%d zcode=%d gemini=%d codearts=%d\n", len(wbAuths), len(trAuths), len(qdAuths), len(kiAuths), len(mimAuths), len(lobAuths), len(zcdAuths), len(gemAuths), len(caAuths))
		fmt.Printf("\nPress Ctrl+C to exit\n")
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		fmt.Printf("\nShutting down...\n")
		stop()
		appInst.Stop()
		return
	}

	// 系统托盘（阻塞）。无桌面环境（如 SSH/服务会话）下托盘初始化失败时
	// 捕获 panic，提示用户使用 --no-tray 参数启动。
	func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "Tray init failed: %v\n", r)
				fmt.Fprintf(os.Stderr, "Use --no-tray to run in headless mode\n")
				stop()
				appInst.Stop()
				os.Exit(1)
			}
		}()
		systray.Run(trayIconICO, "workbuddy-api — 渠道聚合代理", systray.Actions{
			OpenUI: func() {
				// 本机打开面板：通配监听时浏览器访问不了 0.0.0.0，用局域网 IP 兑底
				_ = platform.OpenURL(fmt.Sprintf("http://%s:%d/", openHost(cfg), cfg.Listen.Port))
			},
			OpenLog: func() {
				if err := appInst.OpenLogFile(); err != nil {
					log.Printf("open log: %v", err)
				}
			},
			Quit: func() {
				if platform.AskYesNo("workbuddy-api", "确定退出 workbuddy-api 吗？") {
					// 先摘托盘图标：NIM_DELETE 由托盘消息循环执行，返回即已回收。
					// 直接 os.Exit 会跳过这一步，Windows 任务栏留下幽灵图标。
					systray.Quit(3 * time.Second)
				}
			},
		})
	}()

	// Run 返回 = 托盘图标已回收，停机清理后进程自然退出。
	stop()
	appInst.Stop()
}

// handleRaccoonCallback 处理 `--raccoon-callback <深链>` 这一**非守护**入口。
//
// 背景：小浣熊登录期间会把 HKCU 的 `office-raccoon` 协议命令行临时改成
//
//	"<workbuddy-api.exe>" --raccoon-callback "%1"
//
// 用户授权完成后 Windows 按该命令行再拉起一份本 exe。该子进程的**唯一职责**
// 是把深链解析后原子落盘（raccoon.SaveCallback），由常驻 daemon 的轮询读取。
//
// 返回 handled=true 表示本次进程是回调子进程（调用方应直接退出）；
// err 非 nil 时仍属 handled（已接管，只是落盘失败）。
//
// state 目录取自 config.json 的 state_file 所在目录；config 读不到时退回
// raccoon 包的默认值（./data），与 config.Default 一致。
func handleRaccoonCallback(args []string) (handled bool, err error) {
	// 只认「第一个参数恰为 CallbackFlag」：避免把深链里的字符串误判成入口。
	if len(args) == 0 || args[0] != raccoon.CallbackFlag {
		return false, nil
	}

	stateDir := ""
	if cfg, cerr := config.Load("config.json"); cerr == nil && cfg != nil {
		stateDir = filepath.Dir(cfg.StateFile)
	}
	// Windows 的 windowsgui 构建**无控制台**，stderr 看不到——把结果追写到
	// data/app.log（与 daemon 同一文件、O_APPEND 个位数写入安全），否则回调失败时
	// 用户与运维都无任何线索（只能看到主进程 5 分钟后「登录超时」）。
	defer func() {
		msg := "raccoon 回调已落盘（等待主进程兑换 token）"
		if err != nil {
			msg = "raccoon 回调处理失败：" + err.Error()
		}
		appendCallbackLog(raccoon.StateDir(stateDir), msg)
	}()

	if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
		return true, errors.New("缺少深链参数")
	}
	if serr := raccoon.SaveCallback(stateDir, args[1]); serr != nil {
		return true, serr
	}
	return true, nil
}

// appendCallbackLog 追写一行到 <stateDir>/app.log（失败静默，日志不能反过来阻断主流程）。
func appendCallbackLog(stateDir, msg string) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(stateDir, "app.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006/01/02 15:04:05.000000"), msg)
}

// workDir 解析数据目录（config.json / auths/ / data/ 的落点）。
//
//   - WILDWORK_HOME 显式指定时优先（测试/自定义布局用）；
//   - 运行在 macOS .app bundle 内（.../Xxx.app/Contents/MacOS）时改用
//     ~/Library/Application Support/WildWork：bundle 内容对普通用户只读，
//     且写到包内会在替换/升级 app 时连同账号一起丢失；
//   - 其余形态（CLI 二进制、Windows 便携包、Linux）沿用 exe 所在目录。
func workDir() string {
	if v := os.Getenv("WILDWORK_HOME"); v != "" {
		if err := os.MkdirAll(v, 0o755); err == nil {
			return v
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	dir := filepath.Dir(exe)
	if strings.Contains(dir, ".app/Contents/MacOS") {
		if home, err := os.UserHomeDir(); err == nil {
			p := filepath.Join(home, "Library", "Application Support", "WildWork")
			if err := os.MkdirAll(p, 0o755); err == nil {
				return p
			}
		}
	}
	return dir
}

// displayHost 展示用途的主机名。
// 通配监听（0.0.0.0/::/空）是用户主动配置的意图，如实显示 0.0.0.0；
// localhost 归一为 127.0.0.1。不做任何「善意」替换——面板顶栏同理（见 app.js renderTopbar）。
// 注意：浏览器/托盘打开面板时用此值，0.0.0.0 在浏览器不可直接访问，
// 故 OpenUI 场景改用 lanIPOrLocalhost() 兑底（见 main.go 调用处）。
func displayHost(cfg *config.Config) string {
	if cfg.Listen.Host == "" || cfg.Listen.Host == "::" {
		return "0.0.0.0"
	}
	if cfg.Listen.Host == "localhost" {
		return "127.0.0.1"
	}
	return cfg.Listen.Host
}

// lanIPOrLocalhost 返回局域网出口 IP，取不到时兑底 127.0.0.1。
// 用于「本机打开面板」场景：监听 0.0.0.0 时浏览器不能访问 0.0.0.0，
// 用局域网 IP（或 127.0.0.1）代替，用户可从本机或局域网其他机器访问。
func lanIPOrLocalhost() string {
	conn, err := net.Dial("udp", "8.8.8.8:53")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || addr.IP == nil || addr.IP.IsLoopback() {
		return "127.0.0.1"
	}
	return addr.IP.String()
}

// openHost 返回适合「本机浏览器打开」的主机：通配监听时用局域网 IP 兑底。
func openHost(cfg *config.Config) string {
	switch cfg.Listen.Host {
	case "", "0.0.0.0", "::":
		return lanIPOrLocalhost()
	case "localhost":
		return "127.0.0.1"
	}
	return cfg.Listen.Host
}

// fatal 记录日志并弹出系统提示后退出。
func fatal(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Printf("%s", msg)
	platform.InfoBox("workbuddy-api", msg)
	os.Exit(1)
}

// migrateStateFiles 旧版 state.json 兼容：旧版 WorkBuddy 状态文件名为 state.json，
// 新版改为 state-workbuddy.json。若旧文件存在且新文件不存在，则重命名。
// TraeWork 状态文件始终是 state-traework.json，无需迁移。
// channelList 持有「当前生效的完整渠道清单」（内建 + 启用中的自定义），供 compat 与
// custom 两条保存通道共用。零值可用：snapshot 返回空表。
// 单测锁定「setCustom 后 snapshot 含新渠道、且不与内建重复」——这正是原始实现的缺陷：
// compat syncer 捕获启动快照，导致后加的自定义渠道在某条保存路径下被丢弃。
type channelList struct {
	mu      sync.Mutex
	builtin []string
	custom  []string
}

// setCustom 替换自定义渠道清单（内建部分不变）。
func (c *channelList) setCustom(custom []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.custom = append([]string(nil), custom...)
}

// snapshot 返回内建 + 自定义的完整渠道清单（拷贝，调用方可安全持有）。
func (c *channelList) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.builtin)+len(c.custom))
	out = append(out, c.builtin...)
	out = append(out, c.custom...)
	return out
}

func migrateStateFiles(oldStateFile, stateDir string) {
	newFp := filepath.Join(stateDir, "state-workbuddy.json")
	if _, err := os.Stat(newFp); err == nil {
		return // 新文件已存在
	}
	if _, err := os.Stat(oldStateFile); err != nil {
		return // 旧文件不存在
	}
	// 读旧文件确认是有效的 state JSON
	raw, err := os.ReadFile(oldStateFile)
	if err != nil {
		return
	}
	var sf struct {
		Accounts map[string]json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &sf); err != nil {
		log.Printf("state.json 解析失败，跳过迁移: %v", err)
		return
	}
	if err := os.WriteFile(newFp, raw, 0o600); err != nil {
		log.Printf("迁移 state.json 失败: %v", err)
		return
	}
	_ = os.Rename(oldStateFile, oldStateFile+".old")
	log.Printf("已迁移 state.json → state-workbuddy.json")
}

// buildCustomRuntimes 将配置中的自定义渠道列表装配为 server 与 app 运行时（R66）。
func buildCustomRuntimes(channels []custom.ChannelConfig, stateDir string) (map[provider.Kind]*server.Runtime, map[provider.Kind]*app.Runtime, []string) {
	sRuntimes := map[provider.Kind]*server.Runtime{}
	aRuntimes := map[provider.Kind]*app.Runtime{}
	var names []string

	for _, cc := range channels {
		k := provider.Kind(cc.Name)
		client := custom.New(cc)
		p := pool.New(filepath.Join(stateDir, "state-custom-"+cc.Name+".json"))
		au := &auth.Auth{
			Kind:      cc.Name,
			UID:       "custom-" + cc.Name,
			Nickname:  cc.Name + " (自定义)",
			ExpiresAt: 4102444800,
		}
		p.Add(au)
		p.SetDisabled(au.UID, false)
		p.ClearPenalty(au.UID)

		sRuntimes[k] = &server.Runtime{
			Kind:                    k,
			Pool:                    p,
			Upstream:                client,
			StaticModels:            client.StaticModels(),
			SingleAccount:           true,
			NoCooldownOnServerError: true,
		}
		aRuntimes[k] = &app.Runtime{
			Kind:     k,
			Pool:     p,
			Upstream: client,
			Custom:   true,
		}
		names = append(names, cc.Name)
		stats.RegisterChannelKind(cc.Name)
	}
	return sRuntimes, aRuntimes, names
}
