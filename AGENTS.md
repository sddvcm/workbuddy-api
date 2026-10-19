# workbuddy-api 开发约定（AGENTS.md）

> 本文件面向 AI Agent 与开发者，记录**工具设计、架构选型、决议项**与开发约定。
> 决议项（第 1 节）记录的是**为什么这么做**，改动前请先读对应条目 ——
> 多数看起来「可以简化」的写法都有明确的踩坑记录。

---

## 0. 项目一句话

多平台（Windows / macOS / Linux）多渠道账号聚合工具：把各家平台的多个账号聚合成
一个 OpenAI 兼容 API，管理界面走**系统托盘 + 系统浏览器 Web UI**。

形态上刻意不用 Wails / WebView2：省内存与运行时依赖，托盘常驻、面板用浏览器打开，
因此面板可以在任意设备上访问。平台能力封装在 `internal/platform`（build tag 拆分）。

## 0.1 新增渠道的完整清单（最易漏，漏了不报错）

接入一个新渠道需要改**五处**，其中第 4、5 处漏了**不会报错**，只是该渠道的
自动签到/保活从不运行或界面无入口 —— 属于最难自查的一类缺陷：

1. `internal/<channel>/` 协议封装，实现 `provider.Upstream`
2. `internal/login_<channel>/` 凭据加载（面板登录 / 导入）
3. `cmd/workbuddy-api/main.go` 装配 `Runtime`、`client`、代理同步项
4. `cmd/workbuddy-api/main.go` 调度器启动处补 `go <sch>.Run(sctx)`
   —— 文件里有醒目注释标注此位置
5. 前端 `cmd/workbuddy-api/web/` 加渠道展示与操作入口

另有渠道前缀白名单（`internal/stats/stats.go`）需同步新增，否则统计漏该渠道。

### 0.2 其他硬性红线

| # | 决议 | 说明 |
|---|------|------|
| R1 | **托盘菜单固定，不做动态内容、不做定时/事件刷新** | 用户在自己客户端操作无法捕捉，动态展示无意义 |
| R2 | ~~托盘提供「刷新积分」菜单~~ **已移除**。刷新积分改为 Web UI 面板操作 | 托盘菜单精简为：打开主界面 / 查看日志 / 退出 |
| R3 | 托盘固定菜单项：**打开主界面 / 查看日志 / 退出** | 双击托盘 = 打开主界面；不再弹"已启动"提示框 |
| R4 | **管理 API 采用 cookie 会话鉴权**（2026-09-24 修订原「不设鉴权」） | 新增 `config.admin_password`（默认缺失 = 不鉴权，向前兼容）；**监听非环回地址时强制要求设置**（启动层 fatal + `SetListen` 拒绝），否则局域网任何设备可无凭据访问面板/退出程序。会话实现见 `internal/app/session.go`：token 随机 + 内存态（只存 SHA-256），HttpOnly + SameSite=Lax cookie，7 天滑动续期，改密码即失效，登录失败 5 次/IP 锁定 5 分钟。`/api/auth/*` 不鉴权，其余 `/api/*` 走守卫；OpenAI 侧 `api_key` 不受影响 |
| R5 | **Web UI 用纯静态 HTML/CSS/JS**（无前端编译链） | `go:embed` 打进单文件；实用 + 大众审美即可 |
| R6 | 托盘库：**保留 energye/systray**（已跨平台 Win/mac/Linux） | 各菜单项使用不同颜色纯 Go 生成图标，无需外部图标文件 |
| R7 | **移除 wails / WebView2 全部依赖** | 省内存与运行时；平台能力封装进 `internal/platform`（build tag 拆分） |
| R8 | daemon 单进程：一个 `http.Server` 同时服务 OpenAI 端点 + 管理 API + 静态 UI | 沿用 server 现有 ServeMux 扩展 |
| R9 | 核心业务（pool/scheduler/upstream/traework/server/login/config/auth/provider）**整体复用**，格式零迁移 | config.json / auths/ / data/state.json 兼容旧版；旧 state.json 自动迁移到 state-workbuddy.json |
| R10 | 新增渠道扩展方式：实现 `provider.Upstream` 接口 + auth 加载器 + 注册 Runtime | 模型前缀 `channel/<model>` 路由；已实现 WorkBuddyCN(国内) + WorkBuddyAI(国际) + TraeWork + TraeCode(与 TraeWork 共账号，function=solo_agent) + QoderCN + QoderCOM(国际) + 千问办公(qwenwork) + MonkeyCode(monkeycode，平台托管模型) + 小浣熊(raccoon) + Loomy(loomy) + 智谱清言(glm) 共十二渠道；旧 Qoder（`qoder/*`，QoderWork）已从界面下线但路由保留 |
| R11 | Windows 产物在 WSL 交叉编译（`GOOS=windows CGO_ENABLED=0`，已验证可行）；macOS 产物走 GitHub Actions macos-latest（cgo 必需） | WSL 无法编 darwin cgo；CI 增加 darwin job |
| R12 | **无桌面 Linux 使用 `--no-tray` 参数** | 无参启动在无 DBus 环境托盘 panic 直接 exit 并提示；`--no-tray` 跳过托盘打印信息阻塞等待 Ctrl+C |
| R13 | **三接口兼容采用两层结构：内层 handler 不动，新增 `internal/gateway` 边缘层**，经 **in-process 调用**（`io.Pipe` + ResponseWriter 形状）复用内层 | 代码量比内联重构多 20%，但改动面小一个数量级（主链路仅 2 处调用点 + 1 个访问器），回归风险低、可脱离 pool 单测。**不得用 HTTP 自环**（`0.0.0.0` 监听不可作目标、鉴权双份、启动竞态） |
| R14 | **`Stream`/`Aggregate` 的 model 由调用方显式传入**，渠道不得用实例字段记忆「上次请求的模型名」 | 旧实现 qoder 用全局 `lastModel`，多账号并发会串号；traework 恒为空串。详见 `docs/三接口兼容改造备忘.md` §3 |
| R15 | **Responses 的 `function_call` 必须是独立 output item**（带 `call_id`），Anthropic 的 tool_use 参数必须走 `input_json_delta` | 参考实现 `tokligence-gateway` 两处写法不合规范（塞进 `message.content`、start 里一次性给完整 input），Codex/Claude Code 会解析失败 |
| R16 | **无账号渠道不建 auth 文件、不进 `reloadAccounts`、不参与禁用/冷却惩罚** | 匿名凭证是常量 `public`；`SyncToDir` 会剔除磁盘上不存在的虚拟账号，故只在装配时注入一次。单账号 + 不可重登 ⇒ 任何账号级惩罚都等于整渠道下线，故 4xx 一律走 `ErrPassthrough`（原文透传、不计错不冷却）。**2026-09-24 修订：429 也不再冷却**——原「429 短冷却是唯一需要的背压」经实测证伪：单账号无号可轮换，冷却后后续请求在挑号阶段被挡成 `503 no_healthy_account`，反而不如透传 429 让客户端按 `Retry-After` 自行退避；同理**传输层错误也不再累计 `errCount`**（默认 3 次网络抖动即冷却唯一账号）。两者由新增的 `server.Runtime.SingleAccount` 统一豁免（结构属性，不硬编码渠道名），启动时另调 `Pool.ClearPenalty` 自愈旧版遗留的冷却。详见 `internal/server/handler.go` 的 `Runtime.SingleAccount` 分支 |
| R17 | **用量/积分双流水分口径统计，不强关联、不折算** | `internal/ledger` 双 JSONL（usage 按渠道×模型 / credit 按账号 earn·spend·expire）；写入仅 append 缓冲句柄（30s AutoFlush），读取仅在 UI 请求 `/api/usage` 时按月分段扫描聚合，常驻内存 ≈0。`Upstream.Stream` 返回末帧 usage（R14 同款显式传参哲学）。首见账号只记一条「存量额度」baseline，不逐条展开。**同 key 重复条目（WorkBuddy 伪键一对多）先聚合求和再差分**，每 key 每次刷新最多一条事件；升级首启将旧错误流水一次性归档为 `old-credit-*.jsonl` 并删快照重建 baseline（issue #38）。**2026-10-08 补丁（issue #67）**：WorkBuddy 签到包落在「明日到期」key 上且提前一天以 r=0 建档，「发放即消耗」场景同 key 差分 delta=0 → 当日 earn 被抵消为 0（面板今日收入恒 0）。两层修复：① 差分补丁——快照增加 `used` 字段，delta=0 且 used 上涨时按 `发放额=usedDelta` 补记 earn（数学依据 remain=total-used；used 恒 0 的渠道零影响）；② 权威入账——新增 `provider.CheckinGranter` 可选接口（对齐 CheckinReporter 模式），WorkBuddy 实现 `DailyCheckinGrant`，调度器以它为唯一签到执行路径并用回执 `credit/today_credit` 直接记 earn（回执缺失/为 0 静默降级回差分）。详见 `docs/用量积分流水记账备忘.md` |
| R18 | **临期阈值可配（默认 24h，下限 24h）** | `config.schedule.expiring_threshold_hours`，normalize 钳下限（日期粒度到期判定低于一天无意义）；scheduler 与 app.creditTotals 同源取 `cfg.ExpiringThresholdDur` |
| R19 | **TraeWork 专用池判据仅 `available_endpoint==1`**（2026-09-26 修订，issue #44 撤回 pid==209） | 演进：09-18 专用池下发 ep=1 → 09-23（f6f41a4）上游不再下发 ep=1，改判 `pid==209` → **09-26 实锤证伪**：pid=209「200 档每日签到」已升级为通用积分。本仓日志铁证：09-23 15:20–16:10 连发 99 次对话期间 remain（208/221 池）恒为 3086，而「不可用」小计 2200→1846（-354=99 次对话消耗量），即**扣费实际发生在被判为不可用的池上**；继续排除会让 pool 按虚低余额选号、面板误标「不可用」。故判据回退为 `ep==1`。pid=208/209/221 均可消耗 |
| R20 | **千问办公推理 body 必须携带 `business` 段**（`{product:"qoder_work",type:"agent",version:"1",feature_switches:{}}`） | 2026-09-24 上游 1.0.4 起网关按 `body.business.{product,type}` 解析模型目录，缺失 → 对话恒 HTTP 200 + envelope 503 `Model catalog unavailable`（模型列表/余额/费率不受影响）。**仅补 `Cosy-Business-*` 静态头不能替代**。已实测四组对照隔离变量：body 缺 business 时「本项目透传 body」与「上游原生重构造 body」均 503，补上后均 200 ⇒ 原生 body 结构、官方 `Encode=1` WASM 组包、机器指纹（machineId/Token）**均非必要条件**，故本项目只补字段、不引入 wasmtime 级依赖。参考 Buddy2api PR #84（v2.1.15） |
| R21 | **千问办公 `expires_in` 单位是秒**，且 `expiresAt` 可被 access token 的 JWT `exp` 校正 | 回归：早期按毫秒处理（`*time.Millisecond`），把 7 天压成 604.8 秒 → 落盘 `expiresAt` 比真实寿命少 ~7 天 → `NeedsRefresh(10min)` 几乎恒为真 → **每次请求都刷 token**，与千问办公 App 高频互踩，直至 refresh token 被作废、账号被禁用。证据链：上游 `expires_in=604800` 按秒算 = access token JWT 的 `iat→exp`（整 7 天，吻合）；按毫秒算 = 文件里的值（吻合）。且同仓 workbuddy/trae/workbuddyai 的 auth 文件 `expiresAt` 与 JWT `exp` 逐秒一致，**仅 qwenwork 偏离 6.99 天**。修复：①refresh 按秒解释，优先取绝对字段 `expires_at`，两字段都缺失时回退 84h（JWT 实测 7 天的一半）；②`LoadQwenWorkDir` 调 `Auth.AdoptJWTExpiry()`，用上游签名的 JWT `exp` 原地校正历史脏值（仅内存、只增不减、非 JWT 不动）。**注意：同族 dt-/drt- 渠道（qoder/qodercn/qodercom）token 为不透明串、无 JWT 可交叉验证，其 `// ms` 标注未被本次改动触及**（无证据不做改动） |
| R22 | **智谱清言（`glm/*`）走网页版私有接口；登录以 CDP 自动捕获为主、手工粘贴为兜底** | 清言无可编程登录接口，凭据是浏览器 Cookie 里的 `chatglm_refresh_token`。自动路径见 R27/R28；手工路径保留 `POST /api/login/glm_token`。**不为它引入 WebView2**（R7 已删除该依赖，为单渠道加回是架构倒退且 Windows 专属）——CDP 走系统已装的 Edge/Chrome，零新依赖。协议要点见 `internal/glm/`（含 CDP 登录编排 `internal/login_glm/`） |
| R23 | **清言「签到」的实质是保活对话；新账号额度需 App 侧登录才发放** | `member_info.score_rule` 原文「免费用户，登录赠送200积分/天」，**但对照实验证明这个「登录」指 App 登录**：新账号加进来后 `left_score=0`，**必须在智谱清言 App 登录一次**才发放（+3000，随后再 +500）。证据：某账号创建后独立监控 **15.5 分钟全程为 0**，App 登录后 **30 秒内**变 300000（见 `internal/glm/`（含 CDP 登录编排 `internal/login_glm/`） §2.16）。**影响**：`pool.Pick()` 按 credits 降序选号，而 `healthy()` 不要求 credits>0 ⇒ **额度为 0 的账号不算被禁用，但永远排最后、实际轮不到**。**代码层面无解**（额度发放是上游行为，Web 端无「领取额度」端点）。故 `DailyCheckinReport` 的实质动作 = 保活对话；`UserResource` 从 `member_info.left_score` 读真实积分（**单位「分」，÷100 得积分**）。旧的 `activity-api` 签到活动已下线，保留调用作尽力而为、**完全静默失败** |
| R23b | **分析纪律：观察性数据只能提假设，定因果必须做对照实验** | R23 的结论我**连续错了四次**（详见 `internal/glm/`（含 CDP 登录编排 `internal/login_glm/`） §4.3）：①「服务端延迟」②「App 登录是原因」③「14 分钟没到账⇒需要 App」④「自己到账，与 App 无关」（**忘了是我自己让用户去登录的**，把实验干预当自然现象）。共同病根：**拿观察当因果 + 样本量 1 就下结论**。最终靠**对照实验**才定性：不干预观察 15.5 分钟全 0 → 引入单一变量（App 登录）→ 30 秒内到账。**可复用判据**：下结论前先问「还有哪些变量在同一窗口内变动」；**自己做过干预的实验必须把干预当变量** |
| R24 | **清言渠道为正常多账号渠道（`SingleAccount` 不设）** | **2026-09-26 修正**：初版误设 `SingleAccount=true`，理由是「凭据轮换后不可人工恢复」。但 ① 现在可用 CDP 自动登录随时补账号，该理由不成立；② `SingleAccount` 会**跳过所有账号级惩罚**（handler 两处短路），导致账号 A 失效/限流时**永远不切账号 B**，多账号形同虚设。故改为正常多账号：错误分类惩罚 + 池内轮换。唯一真实约束「refresh_token 轮换必须落盘」由 `glm.RefreshToken` 保证。**教训：`SingleAccount` 是结构性声明（该渠道只有一个号且不可恢复），不是「觉得渠道脆」的保险丝** |
| R25 | **清言 SSE 是「增量 delta」，但 `part.status=="finish"` 时给的是全文** | **2026-09-26 逐帧实测纠正**：`part.content[].text/.think` 的语义取决于 `part.status`——`init` 帧是**增量片段**（每帧几个字符），`finish` 帧是该段落**完整全文**。即 init 帧拼接 == finish 帧全文。实现：init 帧直接透传为 OpenAI delta 并累加；finish 帧与已发出内容比对，仅在全文更长时**补发差额**（防漏兜底）。**曾因照抄参考实现「假定全量快照」的注释而写错**，实测拼出 `"1, 3, 4, 5, 5"`（正确应为 `"1, 2, 3, 4, 5"`）。回归测试 `TestStreamRealFramesNoDuplication` 用真实抓包帧锁死。**注意参考实现 GLM-Free-API 的流式路径本身也是错的**（`substring` 求差在 delta 语义下丢字），不可照抄 |
| R26 | **清言 `accessToken` 允许为空（凭据本体是 refresh_token）** | 用户手填时通常只给 refresh_token，而 `auth.Parse` 要求 accessToken 非空 → `LoadGLMDir` 用放宽版解析器 `parseAllowMissingAccessToken`，由 `glm.acquireToken` 在首次请求时补齐。且 refresh 会轮换 refresh_token，**必须落盘**（落盘失败显式报错，对应不变量 19/20） |
| R27 | **清言登录走 CDP 自动捕获 Cookie，独立 profile，手工粘贴仅作兜底** | 凭据是浏览器 Cookie 里的 `chatglm_refresh_token`。**不读浏览器 Cookie 数据库**——实测 Edge 运行时对其持独占锁（20 进程），既不能读也不能复制，且值为 DPAPI+AES-GCM 加密。改为：`internal/cdp`（手写最小 WebSocket，零新依赖）拉起**独立 profile** 的 Edge/Chrome + 调试端口 → 用户正常登录 → CDP `Network.getAllCookies` 读回。独立 profile 天然隔离，**多账号逐个添加互不干扰**（无需手动开无痕）。手工粘贴路径保留为兜底。见 `internal/login_glm/auto.go` |
| R28 | **自动登录的完成判据是「凭据验证通过」，不是「Cookie 出现」** | **实测发现**：清言对全新访客会自动下发 `chatglm_refresh_token`（423 字符），该 token 调 `user/refresh` 被拒（`访客账号不可用`）。若以「Cookie 出现」为完成判据，会抓到一个**永远不可用的访客账号**。故实现为：轮询 Cookie → 尝试验证 → 只有验证通过（非访客）才落盘收工；超时且只见到访客凭据时给出明确提示。见 `TestLiveGuestTokenBehavior`（实盘验证） |
| R29 | **多账号轮换的真实语义：粘性路由 + 错误惩罚 + 同请求换号**（2026-10-08 修订，PR #82） | 轮换**不是**「积分低就切」：① **粘性路由**优先复用上次成功的账号，直到冷却/禁用或连续 50 次成功；积分只在**选新号**时作排序键（临期 → 总余额）。② **错误惩罚照旧**（冷却/禁用/计错），但自 PR #82 起**账号级错误改为同请求内自动换号**：`ErrKind.Rotatable()` 族（限流/欠费/登录态死/404/5xx/账号故障/ErrClient）在惩罚后 continue 换下一个候选（`MaxRotate` 默认 3 封顶）；内容类错误（content_blocked/prompt_too_long/passthrough）**仍透传不换号**——换号必然复现；候选全部耗尽时透传**最后一个上游原始响应**（保 6004 重置时间等细节）而非包装 503。只有**传输层错误**照旧不计 Rotatable、直接 continue 换号。**变更动机**：旧「错误下次生效」语义下，客户端重试又被粘性路由钉回同一池，本可避免的失败原样甩给客户端（2026-09-23 生产事故：13 个健康号在场却返回 429）。单账号渠道（`SingleAccount`）完全豁免不变。回归测试 `internal/server/glm_rotation_test.go`（旧断言已按新语义更名改写）、`rotate_on_account_error_test.go` |
| R30 | **清言「伙伴/群聊」任务不自动化** | 接口已探明（`mainchat-api/claw_agent` 完整 CRUD，`claw` = 「伙伴」），但**决定不实现**：① 每天 500 积分（≈5 积分实际价值）vs. 风控风险不对称；② 这些任务的设计意图是引导**真人**使用产品，脚本刷属典型薅羊毛特征；③ 用户手动点两下成本极低。**故本渠道自动化边界 = 保活对话 + 积分读取，不碰创建伙伴/绑定 IM/群聊等写操作** |
| R31 | **每日登录积分走 `member-api/member/daily_login_score`，已接入保活流程** | 初版探测的路径名**全错**（`login_bonus`/`daily` 均 404），真实端点是 **`daily_login_score`**（2026-09-26 从 Web 主包挖出）。主包里它是**页面加载时自动调用**的（`errorMessageShow:false`）⇒ 调用它 ≈ 打开一次网页，特征与网页端一致，**不是跨端伪装**。响应：`status=0` 领取成功；`status=10001 "今日已领取"` = **幂等非错误**。**故无论上游是「自动发放」还是「需主动调用」，调它都安全且有益**。⚠️ **实现陷阱**：`doJSON` 在 `env.Status != 0` 时抛 error，会把 `10001` 误报成「领取失败」→ 新增 `doJSONEnvelope`（只把 HTTP ≥400 与非法 JSON 转 error，业务码交调用方解释）。回归测试 `internal/glm/daily_score_test.go` |
| R32 | **不模拟 App 登录来触发额度发放** | 用户设想「模拟 App 登录动作」以避免手动开 App。**实测否决**：① Web 端**无**任何额度激活接口（8 个候选全 404）；② 换 App 头访问同一接口反而 401（`member_info` Web 头 200 / App 头 401）⇒ App 走**另一套认证**（设备指纹、App 签名等），不是加 UA 就能冒充；③ App 域名 `api.chatglm.cn` 独立存在。**风险不对称**：模拟 App 登录是**跨端伪装**，风控风险比只读 Web 私有接口高一个量级，且触发额度发放正是薅羊毛特征（与 R30 同一逻辑）。**结论：手动开 App 登录一次即可**（一次性成本，零风险，零开发） |
| R33 | **三个 chatglm 变体的模型名带上游代号后缀 `:moe_53f`；旧名保留为别名** | **实测**：6 个模型名逐个测服务端上报的 `parts[].model` —— 三个 chatglm 变体（普通/`zero` 推理/`deep_research` 沉思）**都上报 `moe_53f`**，即**同一底层模型的不同推理等级**（`moe`=MoE 架构、`53` 很可能指 5.3）；search=`ai-search`、ppt/video=`all-tools-glms-glms-v2`。故给三个变体加 `:<model>` 后缀（`glm/chatglm:moe_53f` 等），**search/ppt/video 不改**（其代号是工具标识非模型版本）。**两个必须同步的点**：① `resolveAssistant` 查表前**剥掉 `:` 后缀**（同时保留完整名查表），否则 `chatglm:moe_53f` 只能靠兜底命中；② **旧名保留为别名**，已配置旧名的客户端不断，但 `/v1/models` 只列新名。新增 `UpstreamModel()` 辅助函数。回归测试 `TestResolveAssistantStripsUpstreamSuffix` 断言带/不带后缀解析结果一致 |
| R34 | **新增渠道必须补 go xxxSch.Run(sctx)，否则该渠道的自动签到/保活从不运行** | **2026-09-27 实测发现**：加 glm 渠道时创建了 glmSch、注册进 runtimes、设了观察者，**但漏了 Run()** ⇒ GLM 的自动签到与保活**从未执行**。危害特征：**不报错、不崩溃**，且**手工触发仍可用**（面板按钮 / RunCheckinNow 走的是另一条路径），故极难发现——用户是手工签到后才察觉。**回归测试** cmd/workbuddy-api/scheduler_start_test.go：静态扫描「定义了 xxxSch := scheduler.New(...) 就必须有 go xxxSch.Run(」，并交叉校验 runtimes 里声明的 Scheduler 都已启动。**新增渠道清单应加一项：创建 → 注册 runtimes → 设观察者 → **go Run** → 补测试** |
| R35 | **流式请求必须用独立的 StreamHTTP（不设 Client.Timeout）** | **2026-09-27 生产日志实证**：GLM 对话流走带 Client.Timeout 的 client，触发 context deadline exceeded **5 次**（09/26 22:42、09/27 00:03/00:09/00:12/00:18），**且无终止帧** ⇒ 客户端表现为「回答到一半停住」。根因：Go 的 Client.Timeout **覆盖整个请求生命周期（含读 body）**，对 SSE 意味着「长回答必被掐断」。**修法**（照 traework 既有模式）：Client 加 StreamHTTP *http.Client{Transport: tr}（**共用 Transport 复用连接池、但不设 Timeout**），ChatStream 改用它；main.go 两处 applyProxies 都要把 {glmUp.HTTP, glmUp.StreamHTTP} 一起传（SetTransportProxy 会**新建** Transport，只套一个会让流式漏掉代理）。**非流式 client 仍保留 Timeout**（一问一答需兜底）。回归测试 internal/glm/stream_timeout_test.go：证明流能跑过 HTTP.Timeout，且对照证明非流式仍会超时。**2026-09-29 推广到全部流式渠道**（issue #42）：此前只剩 glm/traework 有 StreamHTTP，qwenwork/qodercn/qodercom/qoder/workbuddy/workbuddyai 的 ChatStream 全用带 Timeout 的 HTTP ⇒ 长流式同样会被掐断。现已给全部流式渠道补 StreamHTTP（共用 Transport、不设 Timeout，流式里 Select{StreamHTTP 非 nil 则用之}），main.go **两处** applyProxies（启动 + 面板热更新）都要把 {HTTP, BillingHTTP?, StreamHTTP} 一起传，否则只套 HTTP 会让流式漏掉代理 |
| R36 | **流被截断不得靠补 `data: [DONE]` 伪装成正常收尾**（issue #42） | **实证**：qwenwork/qodercn/qodercom/qoder 四份 sse.go 的 `sawDone` 是**死变量**（声明后从不赋值）⇒ 上游连接中断/读超时被掐断、或断在半个帧上（末行无换行），出口照样补 `data: [DONE]`，客户端拿到半截 tool_call arguments 报 "tool input was not fully received"，而网关日志**无任何错误**，极难排查。**修法**：parseNestedSSE 增加 `truncated` 出参（读错误/半帧 → true），`streamAsOpenAI` 里截断时改发一帧 OpenAI 规范 error（`code: upstream_truncated`）且**不发** [DONE]——调用方 `err!=nil` 同时保留。通用路径上游/sse.go streamCore 也做了同样的截断→error帧改造。刻意保留的行为：上游「正常 EOF 但漏发 [DONE]」（帧完整、末行有换行）仍兜底补 [DONE]，避免误伤本就补的渠道。回归测试：qwenwork/qodercn/qodercom/qoder 各 `stream_truncation_test.go` + 通用 `TestStreamTruncationNotDisguisedAsDone`，覆盖四种输入（传输层错误/半帧/正常 [DONE]/EOF 漏发） |
| R37 | **TraeWork AuthCode 交换：4xx 属终态，不得换 origin 重试掩盖首因；设备号作用域 = 账号**（issue #50；2026-10-08 二次修订 issue #60/#70） | 旧实现 `ExchangeAuthCode` 对 4xx 也 `continue` 试下一个 origin，把首选 api.trae.cn 的真实错误（403/20401 设备数上限）覆盖成回退 origin 的 400/10101「无效参数」后再打印 ⇒ 表象彻底指向参数写错，且 authCode 一次性、4xx 后重试纯属刷屏。**修法**：4xx（除 429/408）立即返回携带**首个** origin 真实响应的 `*AuthCodeRejectedError`（含 origin/status/body）；`IsDeviceLimitReached` 单表识别 20401（可行动提示「去其它设备登出释放名额，最多 10 台」）；`login_trae.Poll` 把终态错误固化到 `login-state.json` 的 `Err` 并清空 AuthCode，后续轮询短路不再每 2s 重消耗。**设备号持久化**（初版）：官方客户端 `getDeviceId()` 是持久的，本仓每次登录 `randHex(32)` 随机新生成 = 每点一次都是新设备。改为从 `device-id.json`（`login-state.json` 同目录、独立文件）复用，缺失才生成并落盘（不复用 login-state，它登录成功/取消即删）。**二次修订（2026-10-08，#60/#70 实证推翻本机级共享）**：`device-id.json` 让一机多号共享同一设备号——#70 实测 15 号对照：共享设备号的 3 个账号全部撞 4017 common risk control（仅对话入口被拒、积分正常），一机一号的 12 个零风控；#60 实测同一编造设备号被多账号绑定后新账号授权恒 20401 Device limit reached（换设备号立即成功）。**修法**：设备号作用域从「本机」改为「登录会话」——每次 `Start()` 新生成一对 machineId/deviceId，登录成功随 `auths/trae-<uid>.json` 落盘成为该账号的稳定注册设备（满足 #3 的 x-device-id 稳定性，签到/对话头从 auth 文件读、不经过 Start）；`Poll` 成功后顺手删除遗留的 `device-id.json`（老用户自愈）。**#60 建议的「读官方客户端真实设备号」不做**：需移植 storage.json TC 解密且依赖本机装官方客户端，而 #70 数据已证明随机号本身可用，会话级新号即可消解两者。**权衡披露**：同账号重登会消耗一个设备名额（上限 10），但远低于旧随机版的消耗速度。回归测试 `internal/login_trae/login_device_test.go` |
| R38 | **上游按模型限流时不得软化整个账号**（issue #53） | **2026-09-29 最小修复（不做大架构改动）**：WorkBuddyAI 上游模型级 429（6004 / body 含 `switch to the other models` / `usage exceeds frequency limit`）在 `Classify` 里判为 `ErrPassthrough`（请求级、不冷却账号），透传原文让客户端按 Retry-After 自退避或换模型——**不**把该账号其它还能用的模型一起拖进 SoftCooldown。与 R16 单账号渠道同哲学。**不做的部分**：pool 升级到 model 维度状态、`stickyKey(kind)`→`(kind,model)`、state v4（issue 作者提议的两种更大改造），它们动 pool/state/handler 三核心、~370 行，需单独评估（作者已主动提出可拆两个小 PR）。注意此判据仅限 WorkBuddyAI：其它渠道的 429 语义仍是账号级 |
| R39 | **`scheduler.Config` 的时间字段：`nil` = 未配置（落默认），`[]int{}` = 本渠道没有这类定时任务** | **2026-09-23 实证**：`scheduler.New` 原用 `len(...) == 0` 判默认值，把两者混为一谈 ⇒ 传 `nil` 想关掉定时任务的渠道被静默补上默认时间。`main.go` 里命中四处：**qoder** 每天 09:00、21:00 各跑一次必然失败的签到（面板按 `last_checkin_ok` 渲染出红色「签到失败」标签，消息是「qoder 暂无签到活动」/「no refresh token」）；**workbuddyai** 注释写「Keepalive 关闭」但 22:00 照常每日刷 token；**qwenwork 最严重** —— 注释明写「定时保活反而会与千问办公 App 互踩」，而保活批次每天照跑。同一个「无签到活动」概念在 `app.go` 有 `noExplicitCheckin()` 守卫（手动签到、首次签到路径都据此跳过），唯独 `scheduler.New` 的零值兜底把它抹掉。**修法**：两处 `len(...) == 0` 改 `== nil`；四处调用点改传 `[]int{}`；`Run()` 补「两个列表都为空时阻塞等 ctx/配置变更」—— 否则 `nextFireMinutes` 返回零值走 `IsZero` 兜底，变成每天 1440 次空转。回归测试 `internal/scheduler/scheduler_defaults_test.go`（零值落默认 / 显式空关闭 / `CheckinHours` 旧字段兼容 / 无任务时 `Run` 阻塞且响应取消） |
| R40 | **流内业务错误不得伪装成正常收尾；兼容层不得吞 error 帧**（2026-10-03） | **背景**：traework 的 `event:error`（3004 限流/1005 权益）旧实现写成 `delta.content` + `finish_reason:stop` + 补 `[DONE]`，且函数返回 `nil` ⇒ 客户端看到「正常结束、内容莫名其妙」、agent 拿半截回答继续跑，且 handler 无从得知失败 ⇒ **账号不冷却**；更糟的是 `NoteSuccess`/`stickySuccess` 在读流**之前**已执行，粘性路由把请求钉死在中招账号上，重试必然再撞。**修法**：① `solosse` 错误分支改发 OpenAI 规范 error 帧（限流标记 `upstream_rate_limited`）、**不补 `[DONE]`**、把错误返回调用方；② 新增 `provider.StreamErrorClassifier`（`Kind()`），handler 用 `errors.As` 取值后按 kind 冷却账号（软 60s / 硬 12h / 禁用），粘性路由见 `status.Cooling` 自动让位，无需显式 `stickyClear`；③ **兼容层必须识别 error 帧**——`gateway.parseChatSSELine` 此前只认 `choices`，error 帧被静默丢弃，致 `/v1/messages` 发 `end_turn`+`message_stop`、`/v1/responses` 发 `response.completed`+`[DONE]`，把失败伪装成成功（该缺口对内层所有渠道通用，含 R36 的 `upstream_truncated`）。**协议合规要点**：Anthropic `error.type` 是 **9 元判别联合**（`api_error`/`rate_limit_error`…），**不得填内层私有码**——限流映射为 `rate_limit_error`；Responses 用 `response.failed`（不补 `response.completed`/`[DONE]`），`response.error.code` 亦须映射到枚举（`server_error`/`rate_limit_exceeded`），私有码只放 `error` 事件的自由 `code` 字段。**3004 的限流维度是待强化假设**（R23b）：观察仅 1 例且来自 checkin 路径，只足以排除 IP/全局级；因软冷却仅 60s、判据不成立时最坏影响是闲置一分钟，故按账号级处置。回归：`traework/solosse_test.go`、`server/stream_error_penalty_test.go`、`gateway/stream_error_frame_test.go`、`scripts/ui-static-check.mjs` |
| R41 | **小浣熊协议回调必须由 `main.go` 在初始化前拦截（`--raccoon-callback`）**（2026-10-05） | **背景**：小浣熊「浏览器授权登录」的授权码经自定义深链 `office-raccoon://auth/callback` 回传，实现方式是登录期间临时改写 HKCU 的 `office-raccoon` 注册表命令行为 `"<workbuddy-api.exe>" --raccoon-callback "%1"`。`internal/raccoon` 把 `CallbackFlag` 与 `SaveCallback()` 都写好了，**但 `cmd/workbuddy-api/main.go` 从未解析该 flag** ⇒ Windows 唤起回调子进程后，它当作普通启动又拉了一份 daemon：端口被占（`listen … bind: Only one usage…`）、**且它的启动自愈看到「残留的协议改写」立刻 `RestoreProtocol`+`ClearCallback`** ⇒ 授权码从未落盘，常驻进程轮询到 5 分钟超时。**全程不报错不崩溃**，用户只看到「点完授权但账号没加进来」。**修法**：`main.go` 在 `os.Chdir(workDir())` 之后、**任何初始化（config/日志/服务/托盘）之前** 拦截 `handleRaccoonCallback(os.Args[1:])`，命中即「落盘后 `os.Exit`」，绝不启动第二份服务。**同时**：`windowsgui` 构建无控制台，回调结果必须追加写进 `data/app.log`（否则失败时零线索）。**同类教训**：这与 R34（新增渠道漏 `go xxxSch.Run()`）同族 —— **「声明了却没接线」的疏漏在源码层就能判定**，故补静态回归 `cmd/workbuddy-api/raccoon_callback_test.go`（断言 main.go 调用了 handler、位置在服务初始化前、分支含 `os.Exit`），去掉修复即失败。**附带**：`/api/state` 新增 `login_error` —— 此前前端只看 `login_busy`，把**所有渠道**的登录失败/超时都显示成「登录完成」，正是它掩盖了本次问题；现由 `peekLoginError()` 非破坏式回传真实原因（读取即清空会被 `/api/state` 的其它调用方偷走） |
| R42 | **渠道本地模型校验必须取「静态表 ∪ 动态目录」的并集**（2026-10-05） | **背景**：raccoon / loomy 都做本地模型名校验（上游对未知模型名**静默回落到默认模型**并返回 200，不校验会让用户以为在用 A、实际扣 B 的额度），但实现只查**静态表**（`KnownModel(id)`）⇒ 上游目录新增的模型会被本地 400 误拒，而**同一个模型正被 `/v1/models` 正常列出**（后者读的是动态目录 `FetchModels` 结果）——表现为「面板列出却调不动」。**实测（2026-10-05）**：raccoon 账户 `/v1/models` 列出 9 个模型，其中 `sn-sensenova-6-8-flash` 调对话被本地 400 `model_not_found`，但直连上游同一模型 **HTTP 200 正常服务**（`sn-sensenova-6-8-flash-lite` 在静态表里、`sn-sensenova-6-8-flash` 不在，二者仅差一个后缀）。**修法**：`fetchCatalog` / `fetchModels` 成功后把目录里的模型名记进 Client（`liveIDs`，**单调扩大、只增不减**——避免目录瞬时抖动把可用模型判成未知），`ChatStream` 改查 `c.knownModel()`（静态表 ∪ liveIDs）。两渠道同款修复 + 回归测试 `TestKnownModelAcceptsLiveCatalog`（去掉并集即失败）。**顺带**：给两个 Client 加 `Base` 字段（默认常量端点，测试注入 httptest 假上游），避免为写这条测试而把 `LLMBase`/`GatewayBase` 从 const 改成 var |
| R43 | **运行统计（`internal/stats`）与用量流水（`internal/ledger`）是两套并列口径，不合并、不互校**（2026-10-07，PR #71–#73） | **背景**：新增「运行统计」tab 时引入了第二套统计面。分工：`ledger` = 磁盘 JSONL 双流水（`data/ledger/{usage,credit}-*.jsonl`），账号**条目差分**口径，按需扫描聚合（R17）；`stats` = 内存实时 + 按日归档 `data/stats.json`，「今日」口径——消耗走**余额下降差值**、收入走**日志签到事件**（`events.go` 正则解析 `app.log`，因「登录即自动签到」等场景条目晚于入账建立，差值恒 0 漏记）、逐请求 token 走 **usage 帧精确值**（`usage.go`）。**决议**：两者**零共享状态、零互相写入**（stats 仅只读 `ledger.Query(1)` 取当日作废），各自标注口径、互不引用；**数字天然不同是设计而非缺陷**（差值含积分包到期作废，ledger 已拆 spend/expire），**不追求对齐**。**边界**：`ledger` 仍是面板「用量与积分」的数据源与 issue #67 等待修的口径；`stats` 只服务「运行统计」tab。新增统计需求时**先明确归属**（精确拆分→ledger；实时/今日/趋势→stats），**不得**为对齐数字而改动另一侧。**注入纪律**：stats 全部数据输入走**进程内直调**（`allStatuses` / `handler.StickySnapshot` / `ledger.Query`），**不得** HTTP 自环（R13）；`main.go` 须 `go appInst.StartStatsFeeders(sctx)`（漏接线即静默无数据，同 R34 家族） |

---

## 1. 决议项索引（R1–R96）

**全文（含证据链、反例、已知限制）见 [`docs/决议项.md`](docs/决议项.md)** —— 改动相关代码前必读对应条目。

| # | 决议摘要 |
|---|----------|
| R1–R3 | 托盘菜单固定（打开主界面/查看日志/退出），不做动态刷新；「刷新积分」已移到 Web UI |
| R4 | 管理 API 用 cookie 会话鉴权（监听非环回时强制要求设密码） |
| R5–R8 | Web UI 纯静态 embed；托盘用 energye/systray；移除 wails/WebView2；daemon 单进程单 `http.Server` |
| R9 | 核心业务整体复用，config/auth/state 格式零迁移（向后兼容） |
| R10 | 新增渠道 = 实现 `provider.Upstream` + auth 加载器 + 注册 Runtime；模型前缀 `channel/<model>` 路由 |
| R11–R12 | Win 产物 WSL 交叉编译、mac 走 CI（cgo）；无桌面 Linux 用 `--no-tray` |
| R13–R15 | 三接口兼容用 `internal/gateway` 边缘层 + in-process（`io.Pipe`）；`model` 显式传参；Responses/Anthropic 规范形状 |
| R16 | 无账号渠道（oczen）不建 auth、不进 `reloadAccounts`、不参与账号级惩罚 |
| R17 | 用量/积分**双流水分口径**（`ledger`），不强关联不折算 |
| R18 | 临期阈值可配（默认 24h，下限 24h） |
| R19 | TraeWork 专用池判据仅 `available_endpoint==1`（pid==209 已撤回） |
| R20–R21 | 千问办公 body 必带 `business` 段；`expires_in` 单位是**秒**，`expiresAt` 可用 JWT `exp` 校正 |
| R22–R33 | 智谱清言：网页版私有接口 + CDP 自动登录；签到=保活对话；新号额度需 App 登录；SSE 是增量 delta；模型名带 `:moe_53f` 后缀 |
| R23b | **分析纪律：观察性数据只能提假设，定因果必须做对照实验** |
| R24 | 清言是正常多账号渠道（`SingleAccount` 不设）；`SingleAccount` 是结构性声明，不是保险丝 |
| R29 | 轮换语义 = 粘性路由 + 错误惩罚 + **同请求内换号**（账号级错误不留给客户端重试） |
| R30/R32 | 不自动化「伙伴/群聊」任务；不模拟 App 登录（跨端伪装风控风险高） |
| R31 | 每日登录积分走 `daily_login_score`（幂等，`10001 今日已领取` 是正常语义） |
| R34 | **新增渠道必须补 `go xxxSch.Run(sctx)`**，否则自动签到/保活静默不运行 |
| R35 | 流式请求必须用独立的 `StreamHTTP`（**不设** `Client.Timeout`） |
| R36 | 流被截断不得补 `[DONE]` 伪装正常收尾 |
| R37 | TraeWork AuthCode 4xx 属终态（不换 origin 掩盖首因）；设备号作用域 = 账号 |
| R38 | 上游**按模型**限流时不得软化整个账号（→ `ErrSoftRate` 模型级冷却） |
| R39 | `scheduler.Config` 时间字段：`nil`=未配置落默认，`[]int{}`=本渠道无此类定时任务 |
| R40 | 流内业务错误不得伪装正常收尾；兼容层不得吞 error 帧（协议枚举合规） |
| R41 | 小浣熊协议回调须由 `main.go` 初始化前拦截（`--raccoon-callback`） |
| R42 | 本地模型校验取「静态表 ∪ 动态目录」并集 |
| R43 | `stats`（运行统计）与 `ledger`（用量流水）是**两套并列口径**，不合并不互校 |
| R44/R67 | 小浣熊登录走网页版 CDP 捕获（迁站后 cookie 名 `xhx_token`） |
| R45 | 明细与刷新路径同源回写 pool 快照（`creditSnapshot`） |
| R46–R47 | Cline 渠道（`sk_` Key 池 + 免费模型）；429 分两类，共享上限**不得**软化账号 |
| R48 | assistant 补 `content:""` 不再以 tool_calls 为前提（治 4001 中毒历史） |
| R49 | **思考档位只透传上游声明，绝不编造** |
| R50 | 思考链死循环检测（`loopguard`）：中断 SSE + **不罚账号** |
| R51 | OpenRouter 系思考字段归一化到 `reasoning_content` 单字段 |
| R52/R59 | 模型 ID 拼接规则单一来源（`provider.PublicModelID/SplitModelID`）；Cline 直通 `cline-free/*` |
| R53/R53b | usage 缓存计量只归一形状不改语义；oczen 会话头须是官方 `Identifier.descending()` 编码 |
| R54 | 模型降级链：原模型无容量（含整渠道全灭）时触发，不粘滞自动切回，降级必须可感知 |
| R55 | traework 模型元数据取上游下发；展示字段容错承接（`json.RawMessage`）不拖垮整张目录 |
| R56 | tool schema `$ref` **全量内联**消毒（moonshot 系对任何保留的 `$ref` 都不认账） |
| R57 | 代理改为「**全局备用上游通道**」：默认不参与任何请求，仅渠道按需取用（cline/oczen） |
| R58 | 流被截断必须**自证原因**（空闲看门狗 vs 上游断连）；`stream_idle_seconds` 配置项从未存在 |
| R60 | 单号渠道 402 处置 = **模型级**冷却（非账号级、非纯透传） |
| R61 | 模型降级链必须在**启动时**载入（不得只靠面板热更新通道） |
| R62 | oczen 同接备用通道；编排抽到 `backuplink.Replay` 与 cline 共用 |
| R63 | 思考强度映射是「出站注入」，与 R49「只透传声明」互补 |
| R64 | 面板「测试」类探针必须与真实请求**同源**取用备用通道，并如实回传实际出口 |
| R65 | 小浣熊「每日积分」是惰性落账，签到 = 调一次 `setting_info`；判据用 `point_grant_popups` |
| R66 | 自定义 OpenAI 兼容渠道（`custom_channels`）：面板配置即接入，统一流水/统计/路由；**无积分概念**，全面退出积分体系（v2.8.1：RefreshAll 跳过、单账号刷新/明细明确报「不适用」、前端动态判定隐藏无意义按钮） |
| R68 | **启动时 HTTP 监听失败必须终止进程**（不得只记日志继续跑） |
| R69 | oczen 匿名车道走 `/inference/v1`；不发 `x-opencode-request`；`x-opencode-project` 用裸 40 位 hex |
| R70 | 华为云果办 OfficeAce：双层凭据（网关 Basic 不过期 + 临时 AK/SK ~2h 需 refresh 续期） |
| R71 | WorkBuddyAI 对话出站必带会话头族（缺 `X-Conversation-ID` 即 `400 11-128`） |
| R72 | devdump 落盘必须 tee 流式（不得 `io.ReadAll`）：读全量再转发会把 SSE 攒批成「正文突然整段出现」 |
| R73 | 硬冷却（402）必须截到「下一个每日发奖窗口 + 缓冲」：qwenwork 每日 00:00 发额度（实测 00:01~00:20 到账），固定 12h 会把 19:07 的 402 白锁到次日 07:07 |
| R74 | 内建渠道扩到 20 条（Kilo/MiniMax/LobsterAI/ZCode/Gemini/CodeArts）；CodeArts 无上游身份字段 ⇒ 用户指定「账号标识」复用 UID |
| R75 | 429+code 14018（积分耗尽）归 `hard_credit`；候选耗尽时可在**请求内**按降级链重入一次（封顶一次、单账号豁免） |
| R76 | 登录态失效走可自愈的 `SessionDead` 标记（不 `Disable`）；重登后按 auth 文件 mtime 自动清除标记 |
| R77 | 上游声明可重试的 429（14003 瞬时拥塞，retry_policy.retry / actions 含 RETRY）必须请求内重试一次（独立预算，20s 退避）；6004 用量配额无信号，维持冷却到重置点 |
| R79 | **目录/费率接口遇 HTTP ≥400 必须返回类型化 `*provider.Error`**（裸 `fmt.Errorf` 会让 `errors.As` 取不到分类 ⇒ 模型面/费率面的 401 自愈永不生效） |
| R80 | **折线图的 0 值必须画成 `null` 断点**（画成 0 会在底部连出贴轴长线），并过滤「区间总量为 0」的系列；`connectNulls:false` |
| R81 | **自定义渠道独立成一级弹层**（主面板第 18 个按钮）+ **模型一键导入**（拉 `/models` 勾选批量加）；设置弹层里已移除该节 |
| R82 | **自定义渠道一行式列表 + 新增/修改全走弹窗**（渠道/模型/单模型三层）+ **下拉框 `appearance:none` 自绘箭头** |
| R83 | **模型与 provider 同行展示**（chip 是文字按钮，点开编辑）+ **删除改文案按钮**（不用 🗑/×）+ **移除「停用」概念**（`enabled` 字段删除） |
| R84 | **弹窗宽度统一三档**（400/720/1180，禁止第四档）+ **自定义渠道搬回设置弹层**（独立弹层只是绕过「设置太窄」的伪问题）+ 设置加吸顶跳转条 |
| R85 | **自定义模型改独立弹窗**（设置里只留入口按钮）+ **Provider 一行、其下每个模型各一行**，两类行都 `nowrap`（折行用横向滚动替代）；R83「模型与 provider 同行」被推翻，删除改文案/移除停用保留 |
| R86 | **设置弹层宽度回原样（720px）** + **自定义模型入口移到主工具栏「＋ CodeArts」右侧**；删掉 R84 的吸顶跳转条（搬走那块后失去理由） |
| R87 | **自定义模型弹层与设置同宽（720px）** + **去掉「测试」** + **每行按钮组整体右对齐**（信息区 `flex:1` + 按钮组 `margin-left:auto`，不是上下按列对齐） |
| R88 | **自定义模型弹窗深度统一项目原生风格与对齐修复**：Provider 行移除 `<div class="cm-actions">` 彻底修好按钮右对齐 Bug；底栏操作并列右下角；视觉设计全面回归项目原生 CSS 变量与设计令牌（标准 `.hint`、`.btn.small` 与从属树线），危险按钮改柔和描边消除刺眼红斑，与全局面板浑然一体 |
| R89 | **设置弹窗视觉细节规整化**：表单 Label（90px）与子行（98px）严格缩进对齐；签到时间改流式容器化，时间 chips 与「＋ 时间」按钮同高紧凑排列（消除加号孤立掉落缺陷）；开关行垂直居中规整化；滚动条极细化 |
| R90 | **弹窗长篇说明文字角标化（`?` 悬浮说明）**：自定义模型与设置弹窗中原占用大篇幅排版的说明段落收敛为右上/行内 `?` 角标（`.help-mark`），鼠标悬停（hover）展示带透明衔接桥的自适应浮动气泡（`.help-pop`，支持 rich text），消除长文本挤占纵向空间并提升表单通透度 |
| R91 | **修复设置内点「查询模型」导致设置弹窗消失 Bug**：移除 `openModelSearch` 误收起设置的逻辑；`modelSearchOverlay` 设为独立高层（`z-index: 120`），打开时以浮层叠加在设置上方，关闭后自然回到设置原位置，已填表单与滚动状态完全保留；补全标题右上角 `✕` 关闭按钮与 Escape 监听 |
| R92 | **设置面板降级链与思考强度编辑区视觉精细化重构**：① 标题区使用 flex 排序，使 `?` 角标紧随区标题（左侧），「🔍 查询模型」从虚线框升级为实线胶囊操作按钮（右侧）；② 修复全局默认输入框 `.grow` 坍缩截断 Bug，自适应撑满行宽；③ 降级链与思考强度规则录入区封装为独立 `.fb-compose-panel` 浅底卡片，操作区边界清晰；④ 空态提示升格为轻柔虚线卡片，已生效代理采用绿点徽标（`● 已生效`） |
| R93 | **修复降级链与思考强度输入框点击下拉菜单缺失 Bug**：① 修复 style.css 中注释意外闭合导致的 CSS 语法破坏，恢复 `.pk-wrap` 相对定位与 `.pk-menu.open { display: block }` 核心样式，并提级 `z-index: 100` 消除层叠遮挡；② 补充输入框 `"click"` 事件监听，使输入框已获焦点时再次点击亦能即刻唤起候选菜单；③ 设置打开时若费率未就绪自愈触发 `loadFees()` |
| R94 | **模型降级链 = 模型维度的替换机制**，只在「换号已用尽」后出手；三维归因（账号/模型/请求）对应四类处置（request / account-transparent / account-rotate / model），同维替换、跨维禁止；**模型饱和（14003/6004）与积分耗尽（14018）同属「值得请求内换模型」**，且请求内重入**只跨渠道**（同渠道兄弟模型共享同一批账号）；详见 [`docs/模型降级链定义.md`](docs/模型降级链定义.md) |
| R95 | **Qoder 双区签到：活动资格是上游按账号授予的**（实测：同账号在官方客户端里同样看不到入口）⇒ 无资格必须直说「该账号无资格、不是本工具的问题」，不得含糊成「上游未下发」；**且按活动 `startAt/endAt` 有效窗口过滤**（窗口外不参与「可领/已领」判定，缺字段则不过滤）；面板结果分「领到 / 已领 / 还没发 / 失败」四档，**不得一律写「签到成功」**（详见 `docs/决议项.md`） |
| R96 | **账号登录态失效必须可被用户看见与修复：`session_dead` 是「可自愈标记」不是「禁用」，且签到失败必须如实反映（R76 的落地补强）**——① `RecordCheckin` 的 map 查找变量不得叫 `ok`（遮蔽函数参数会让 `last_checkin_ok` 恒真、每次都清掉 `session_dead`、`else if sessionDeadLike` 成死代码）；② `MarkSessionDead` 已标记时**更新原因但保留时间锚**（时间锚是「重登后自动清除」的判据）；③ CodeArts 这类**回调写死回环**的渠道要做异地登录兜底 |
| R97 | **LobsterAI 异地登录：上游 portal 只认 `http://127.0.0.1:<port>/auth/callback`（hostname 严格等值，否则丢弃并改发 `lobsterai://` deep-link）⇒ 改 redirect host 到 LAN IP 无效（已实测并回滚，留护栏测试）**。可行方案：① 面板「粘贴回调地址」（`SubmitCallbackURL`，与自动回调同一结论通道、state 不放宽）；② 全自动 = 固定端口（`config.login.lobsterai_callback_port`）+ 浏览器侧 `ssh -L` 端口转发（本机已装 `ww-lobsterai-tunnel.service`，38055）；③ 局域网模式下不得因错 state 写终态（防 DoS）已随方案 ② 一并回滚，因为监听仍只绑环回 |
| R98 | **用量折线图的横轴必须是「固定整窗刻度」**：`days=1` ⇒ 00:00~23:00 共 24 个小时桶，`days>1` ⇒ 窗口内每天一个（**含无流水的空桶**）。不得按「当前几点」或「只有数据的时段」截断——旧实现 `for h := 0; h <= now.Hour(); h++` 让今日视图在 11 点只画到 11:00，横轴长度随时刻变化、刻度本身也在变，既无法横向比较也看不出「后面还没跑」；积分图更严重（自己按流水聚合，横轴只有有消耗的日子）。补桶是**后端唯一来源**（`ledger.Query`），前端只消费 `by_day`；积分图用 `usageAxisKeys(usageDays)` 对齐同刻度 |
| R99 | **渠道配色必须单一来源**：`:root` 的 `--ch-<类名>` 是唯一真源，`.badge.x` 一律 `var(--ch-x)`（**禁止写死 hex**），徽章统一走 `chBadge()`（底色 = `chColor()`，与折线图 series 同源，物理上无法漂移）；**新增渠道必须同时补 `--ch-<类名>`**，漏加会让 `chColor()` 静默回退 `#6366f1` ⇒ 多个渠道在图上撞成同色（10-17 连加 6 个渠道时就踩了）。自定义渠道无品牌色，按列表序号黄金角均分色相（不用固定色池，色池在渠道数超过池长时必撞）。**已知限制（实测，未修）**：master 走品牌色路线，两两辨识度靠「无完全同色」把关，**未做 CIEDE2000 校验**——实测最小 ΔE2000 = 6.9（loomy vs minimax），共 15 对 <14；`.badge` 文字色恒为白，traecode（4.1）/raccoon（3.79）白字对比度低于 WCAG AA 的 4.5。要收紧就得改色值，需先做视觉评估。**完整规范见 [`docs/渠道配色规范.md`](docs/渠道配色规范.md)**（色值基线表 + 新增渠道必补三处 + 变更流程；`ui-static-check` 逐值比对文档与 `:root`，改色不同步文档即测试失败） |
| R101 | **积分流水差分必须区分「池子缩水」与「用掉更多」**：`remain = total - used` ⇒ `Δremain = Δtotal - Δused`。旧实现只比 remain，「余额下降 + 无到期日」恒记 spend ⇒ QoderCN 一个 100 分包整包到期（带走了包内已用 1 分后剩的 99）被面板显示成「今日消耗 99」（2026-10-19 用户实测，当天真实调用只有 1 次 480 token，上游 quota 实测 `used` 反而从 1 归零）。修法：快照新增 `t`(total)，下降时多一道 `poolShrankExpiry` 判据，三门槛缺一不可——① 该条目无到期日（有到期日的渠道继续走 `expired()`，那一路本来就对）；② **该渠道**被证实会给 total/used 分别赋值（`channelBooksUsage`，持久化为快照 `"@ch/<channel>"` 标记；qwenwork/raccoon/minimax/monkeycode/loomy/glm 六个渠道 `Total=Remain`、`Used` 恒 0，无此门槛会把它们的真实消耗全记成过期。**必须按渠道判、不能按 key/账号判**：只签到不花钱的号 `used` 恒 0，按 key 判会把它的整包到期漏修）；③ `total` 确实下降且上次快照有 total（老快照无 `t` 字段 = 零值 ⇒ 自动退回旧口径，不改写历史归因，符合不变量 5）。详见 [`docs/决议项.md`](docs/决议项.md) |
| R102 | **「今日作废」必须是平台表的一等列，不能只藏在合计行 tooltip 里**：`credit_expired`（往回看：今天已经到期消失的额度）与 `expire_today`（往前看：今晚余额里还有多少会没）是**相反方向**的两个口径，混成一列会把「已经没了」说成「还没发生」。R100 补删掉顶部汇总横条时把唯一可见的「今日作废」一起删了，只剩合计行「今日消耗」的悬浮说明里带一句，等于不可见（2026-10-19 用户实测：「今日作废在哪看？」）。修法：平台表在「今日消耗」右侧加 `st-plat-expd` 列，逐渠道取 `platforms[].credit_expired`、合计行取顶层 `credit_expired`，配色用 `--danger` 与「今日消耗」的 `--warn` 区分。`ui-static-check` 里把 `FAKE_STATS` 的 `expire_today` 从 100 改成 77——两者取同值时断言无法分辨口径是否被接错（变异验证实证：同值时漏改 `credit_expired→expire_today` 不报警） |
| R103 | **「不适用」账号默认收起，但入口必须留在原地且一键可逆**：无积分概念的渠道（oczen/kilo/cline/自定义渠道）在账号页上每号占一整张只有「不适用」三字的卡片、汇总条上还各占一枚同样空洞的 chip，把真正有余额的渠道稀释掉（2026-10-19 用户：「把不适用的默认隐藏掉，但有时候又得看一下」）。修法：① 判据只用后端下发的 `credits_na`（`isNoCreditAcct`），**不在前端另维护渠道名单**——自定义渠道是运行期可增删的，手写名单必漏收起；② 汇总条收起这些 chip，改在**原位**放一枚「不适用 N号 展开/收起」开关 chip（不额外占工具栏一格）；③ 账号卡片在 `renderAccounts` 里就带 `hidden`（首帧不闪），并带 `data-na` 供 `applyAcctFilter` 二次过滤；④ `toggleNoCredit` 必须**重渲染卡片**而不只改 class（可见性只有 `naHiddenClass` 一个真相，两处各写一份必漂移）；⑤ 收起时若正筛选着某个「不适用」渠道必须自动清筛选，否则网格空白且「全部」chip 也随汇总条消失；⑥ 选择持久化 `localStorage["acct-show-na"]`，全部被收起时给 `#acctHiddenTip` 可执行提示。`ui-static-check` 新增场景 1.7（13 条断言），4 处变异（默认展开 / 去 `data-na` / 不重渲染 / 不清筛选）全部确认变红 |
| R104 | **回环回调渠道跨机登录 = 五渠道统一人工粘贴兜底；CDP 浏览器化已回滚**：traework/codearts/lobsterai/qwenwork/gemini 的回调写死回环，dashboard ≠ 网关机时唯一通用解是把浏览器地址栏 URL 粘回面板（`manualCallbackKind` 白名单单一来源，qwenwork/gemini 的 `SubmitCallbackURL` 本轮补齐）。CDP 方案（网关机拉浏览器+捕获导航）已完整实现后实测回滚——只解决「人在网关机前」，对主流的远程 dashboard + 无人值守网关无解。**跨机方案动手前必须先确认「人在哪台机器前」**。详见 docs/全渠道登录授权方式梳理.md §4 |

> ⚠️ **R71 附带的实测纪律**（也是 §0.1 的来源）：上游校验的是**头族齐全**而非单点
> （只补 `X-Conversation-ID` 仍被拒）；且 `X-Conversation-ID` 的**取值形态不被校验**
> （UUIDv4 / 32hex / 裸串都通过）。

---

## 2. 关键不变量（改动前必读）

管理 API（REST，均挂 `/api/*`；除 `/api/auth/*` 外均需有效面板会话——密码为空时不校验）：

```
POST /api/auth/login               # {password} → {session}；下发 HttpOnly cookie
POST /api/auth/logout              # 注销当前会话
GET  /api/auth/state               # 会话探针（不返回 401）：全量状态 + auth_enabled/auth_session/auth_required
GET  /api/state                    # 全量状态（账号/积分/签到/配置）
POST /api/login/start              # {channel} → {auth_url}
POST /api/login/glm_auto           # 智谱清言自动登录：拉起独立 profile 浏览器（见 R27）
GET  /api/login/glm_auto_status    # → {status: idle|pending|success|failed|cancelled, uid?, nickname?, error?}
POST /api/login/glm_auto_cancel    # 取消自动登录并关闭浏览器
POST /api/login/glm_token          # {refresh_token} → {uid} 智谱清言手工兜底（见 R22）
POST /api/login/cancel
POST /api/account/checkin          # {uid}
POST /api/account/checkin_all
POST /api/account/refresh          # {uid}
POST /api/account/refresh_all
POST /api/account/remove           # {uid}
POST /api/account/disable          # {uid,disabled} 停用/启用
POST /api/account/resource_detail  # {uid} → 积分明细
POST /api/account/nickname          # {uid,nickname} 修改显示名
POST /api/config/checkin_times     # {times:["09:00","21:30"]}
POST /api/config/listen            # {host,port,admin_password?}（非环回时必须带密码）
POST /api/config/admin_password    # {password}（空 = 关闭面板鉴权，仅环回监听允许）
POST /api/config/api_key           # {key}
POST /api/config/autostart         # {on:bool}
POST /api/config/compat            # 模型路由（默认渠道 / 封顶 tokens / 映射表）
POST /api/config/expiring_days     # {days} 临期阈值
POST /api/config/proxies           # {proxies:{channel:url}} 单渠道上游代理
GET  /api/fees                     # 渠道费率（本地缓存 + 按需刷新）
POST /api/fees/refresh             # 异步刷新费率
GET  /api/usage                    # 用量/积分流水聚合（R17，ledger 口径）
GET  /api/stats                    # 运行统计快照（R43，stats 口径）
GET  /api/stats/logs?limit=15      # 运行统计的最近请求日志行
GET  /api/logs                     # 最近 300 行日志
POST /api/quit                     # 退出程序
```

## 5. 渠道（已实现 WorkBuddyCN + WorkBuddyAI 国际版 + TraeWork + TraeCode + QoderCN + QoderCOM 国际版 + 千问办公 + MonkeyCode + 小浣熊 + Loomy + 智谱清言；旧 Qoder 已下线）

1. 新建 `internal/<channel>/` 包，实现 `provider.Upstream` 接口
2. `internal/auth` 增加对应 `Load<Channel>Dir()`（文件名前缀 `<channel>-*.json`；
   **glob 边界**：`qoder*.json` 会吞掉 `qodercn-`/`qodercom-` 前缀，LoadQoderDir 必须显式排除）
3. 装配处注册 `server.Runtime{Kind, Pool, Upstream, StaticModels}` + `app.Runtime{..., Scheduler}`
4. 前端渠道选择器加一项；`internal/login_<channel>` 实现登录编排（如需）
   > **无账号渠道跳过第 2、4 步**：不建 auth 文件与加载器，虚拟账号由 `main` 装配时注入 pool，
   > 且 `app.reloadAccounts` 不得纳入（否则 `SyncToDir` 会把它剔除）。

> provider.Kind 即模型名前缀；server 按 `channel/<model>` 前缀路由，无需改接口。
> **QoderCN（`qodercn/*`）**：qoder2api 参数形态（cosyVersion 1.0.10、18 头含 cosy-scene 族、
> session_type=qoder、identity userType 实测回填）；签到仅 campaigns（legacy daily-check-in 已 DISABLED，
> 其 claim 恒返回 409 会造成假成功，故不再使用）；每日 10:00 开放 + 10:00–12:00 窗口内重试（见不变量 23）；
> 动态模型表（无静态兑底，上次成功缓存）。详见 `internal/qodercn/`。
> **QoderCOM（`qodercom/*`）**：国际版（qoder.com/openapi.qoder.sh/api1+api2.qoder.sh 三域分离）；
> 凭据与 CN 区完全隔离（双向 401）；签到仅 campaigns（无 daily-check-in，实测 404）；
> 每日 10:00 开放 + 10:00–12:00 窗口内重试（同 CN，见不变量 23）。详见 `internal/qodercom/`。
> **旧 Qoder（`qoder/*`，QoderWork）已从界面下线**：代码与路由保留，存量账号仍可用；不新增功能，后续可移除。
> WorkBuddyAI 国际版：`DailyCheckin` 实现为「免费模型对话保活 + 签到探测」（对用户透明，无前端界面）；
> token 有效期 365 天，故 KeepaliveHours 设为 nil。详见 `internal/workbuddyai/`。
> **MonkeyCode（`monkeycode/*`，平台托管模型）**：凭据由面板「从本机客户端导入」从官方客户端
> （ohmyagent）的 settings.json 取得，一个账号 = `oma_` api_key + `omas_` signing_secret；
> 按模型声明的 type 分流到 `{base}/messages`（Anthropic 形状）或 `{base}/responses`（Responses 形状），
> 两路都是无状态 SSE；上游无目录/额度/刷新接口 → 模型表静态、额度恒 0、`RefreshToken` 空实现。
> 协议对照参考：`codkeep/MonkeyCodeReverseEngineer`（Apache-2.0）。
> **小浣熊（`raccoon/*`）**：两条添加路径 —— 面板登录按钮走浏览器授权（登录期间临时把
> `office-raccoon://` 协议回调指向本工具以接住授权码，结束立即恢复注册表，见 internal/login_raccoon），
> 弹窗次按钮「从客户端导入」读本机 `.box-agent/config/auth.json`。凭据 access_token ≈2h +
> refresh_token ≈30d，保活每 4 小时一次以减少「请求先 401 再刷新」的往返；无签到端点。
> 上游默认档即最深思考，故不接档位面。
> 接口结构整理：`xxhhlk/raccoon2api`（MIT）。
> **Loomy（`loomy/*`）**：导入型渠道 —— 凭据由面板「从本机客户端导入」读
> `C:\Users\Public\Loomy\<hash>\userData\auth-session.json`。上游**无 refresh 端点**，
> session 约 14 天，到期需重新导入；故 `RefreshToken` 留空、定时任务全关（保活会被
> 按「无 refresh token」跳过，但每天仍空跑一次并记失败）。鉴权失败是 **HTTP 200 +
> 业务码 `100002`**（不是 401），业务码判定必须排在状态码判定之前。
> 接口结构整理：`xxhhlk/loomy2api`（MIT）。
> **智谱清言（`glm/*`）**：网页版私有接口（**非**开放平台 open.bigmodel.cn），凭据是浏览器 Cookie 里的
> `chatglm_refresh_token`；所有私有接口需签名 `X-Sign = md5(ts-nonce-secret)`；模型 = `assistant_id`
> （24 位 hex 智能体 ID）+ `chat_mode`（`zero` 推理 / `deep_research` 沉思 / `ppt` / `video`）。
> **模型清单是「智能体清单」不是「模型版本清单」**——清言客户端无法选模型版本，
> 服务端按账号分配（实测主对话恒为 `moe_53f`）；GLM-5.3 的具体版本控制只能走官方开放平台。
> 静态表**只收录实测可用的 4 个智能体**（ChatGLM / AI搜索 / 清言PPT / 视频助手）；
> AI画图（需 cogview 参数）、AI阅读（需上传文件）、学习搭子（上游 10025 报错）**不收录**——
> 想用未收录的智能体直接填其 24 位 hex ID。
> **积分机制**：`member_info.score_rule` = 「免费用户，登录赠送200积分/天」。
> **新账号激活**：加进来后 `left_score=0`，**必须在智谱清言 App 登录一次**才发放（+3000 随后 +500）——
> 对照实验证实（R23）。**代码层面无解**（Web 端无激活接口，R32 实测），
> 故额度为 0 的账号会永远排最后、实际轮不到。
> **每日积分**：走 `member-api/member/daily_login_score`（**已接入保活流程**，R31）——
> 该接口网页版打开时自己就会调，幂等安全（`status=10001 今日已领取` 是正常语义）。
> 「签到」的实质是**保活对话**。**操作建议**：面板加完账号 → 手机 App 登录同账号一次 → 之后每天自动领。
> SSE 为**增量 delta**（`finish` 帧给全文，见 R25）。登录走 CDP 自动捕获 Cookie（R27/R28），
> 支持多账号（独立 profile 逐个添加）。详见 `internal/glm/`（含 CDP 登录编排 `internal/login_glm/`）。

## 6. 关键不变量（改动前必读）

0. **每次代码变更后必须本地重新构建 `dist/workbuddy-api.exe`**（见 §8）。
   `dist/` 在 `.gitignore` 中，CI 只产出带平台后缀的 `workbuddy-api-<os>-<arch>`，
   **不会**生成 `dist/workbuddy-api.exe`——该文件只能手动构建。
   不重建会导致：本地运行的二进制与源码不一致（例如改了版本号但仍显示旧版本）。
1. `PrepareBody` 四改写勿动：强制 `stream=true`、`tool_choice` 归一化、`developer→system`、tool schema `$ref` 消毒（R56，无 `$ref` 时零改动）
2. 日志/面板/消息框**零 token**：不得输出 access/refresh token（调试用假 token）
3. auth 文件嵌套格式 `{auth:{...},account:{...}}`，`internal/auth.Parse` 与 login.SaveAuth 必须一致
4. `config.listen` 兼容新对象格式 + 旧字符串格式 `":7863"`
5. `data/state-*.json` 只增不减字段，向后兼容；旧 state.json 自动迁移
6. 托盘回调必须 goroutine 化
7. `config.example.json` 与 `config.Default()` 同步
8. 上游 HTTP ≥400 错误直接透传原始响应，不包装
9. 定价缓存持久化到 `data/pricing-cache.json`，启动加载，超 1h 自动刷新
10. 无桌面 Linux 必须 `--no-tray`，不带参数 panic 直接 exit 提示
11. **粘性路由**：`pickWithSticky` 优先复用上次账号，直至连续成功请求达 50 次或遭遇错误冷却。成功时 `stickySuccess` 递增计数，错误时 `stickyClear` 清除粘性记录。不使用 credits 阈值（pool 中余额是 stale 数据）。
12. **`internal/server` 主链路不得被绕过**：`POST /v1/chat/completions` 与 `GET /v1/models` 由内层直接服务，`internal/gateway` 只接管 `/v1/responses`、`/v1/messages`、`/v1/messages/count_tokens`。
13. **兼容层调用内层只能经 `Gateway.call()`**（`io.Pipe`），调用方读完必须 `res.Close()`，否则内层 goroutine 可能阻塞在 Write 上泄漏。
14. **`pipeRW.Flush()` 为空操作是刻意的**：`io.Pipe` 无缓冲，Write 即送达；不要改成缓冲 + 定时 flush。
15. **错误分类 429 必须优先于 hardMarkers**：限流 body 高频带 `quota exceeded`，先判 hardRule 会把限流误归余额耗尽 → 12h 硬冷却。三渠道 `Classify` 均已修复此顺序。
16. **脱敏层仅做文本替换不做语义变更**：`internal/sanitize` 只改模板句、不改用户内容语义；预检不命中时零分配原样通过。将来配置 `features.sanitize_fingerprints` 可一键关闭（逃生门）。
17. **积分「可用/不可用」拆分统计**：`provider.ResourceItem.Usable` 标记条目是否属于本工具可消耗的额度池，`provider.Summarize()` 汇总小计。
    - TraeWork 判据（2026-09-26 修订，R19 / issue #44）是 **`available_endpoint==1` 为不可用**
      （pid==209 判据已于 09-26 撤回，见下）：
      上游已不再下发 ep=1（专用池也标 0），ep 判据仅作历史兑底。**pid==209 判据已于 2026-09-26
      撤回**（issue #44）：pid=209「200 档每日签到」升级为通用积分，实测扣费就发生在这个池上
      （09-23 99 次对话期间 remain 恒 3086、「不可用」小计 -354），继续排除会让 pool 按虚低余额选号、
      面板把真实可用积分误标「不可用」。**不得用 `group_type` 判定**——同名「每日签到」既有
      通用份也有专用份。早期仅用 ep 判定的实测证据见 `internal/upstream/` §2.3。
    - `UserResource` / `UserResourceDetail` 返回的 remain **只能是可消耗余额**，
      否则 pool 会按虚高余额选号。含专用池的总量（`usage_summary.total_amount`）不能作路由依据。
    - 不可消耗额度仅用于面板展示（`pool.Status.UnusableCredits`），不参与 `Pick()` 排序；
      展示的唯一目的是让用户看到的总积分能和官网对上。
18. **到期时间字段因渠道而异，缺失则不显示**：WorkBuddy 系是 `CycleEndTime`（**上游从不下发 `PackageEndTime`**，旧判据恒 miss），
    TraeWork 是 `expire_time`（Unix 秒），Qoder 无此字段。均按 **UTC+8 墙钟**解析（`softRateResetLoc`），
    用 `time.Local` 会在非 UTC+8 机器上算错一天。上游未下发时 `ResourceItem.ExpireAt` 必须为空串，
    前端据此隐藏整列——**不得用零值时间冒充「永不过期」**。
    - **两种写法并存，消费方必须同时支持**：`YYYY-MM-DD`（该日**全天可用**，次日 00:00 起失效；
      全仓主口径，当日有效池走 `provider.ExpireDateToday()`）与 **RFC3339**（精确时刻，如 gemini 的
      5 小时/周配额窗口重置）。判定统一走 `provider.ExpireInstant`（是否已失效）与
      `provider.ExpiringWithin`（是否临期），**不要各自 Parse**——旧实现各处自解析且吞掉错误，
      RFC3339 的条目被静默当成「永不过期」（2026-10-18 核对）。
19. **401 必须自愈，不能只信本地 `expiresAt`**：上游刷新会作废旧 access token（refresh token 同步轮换）。若新 token 未落盘、
    或同一账号在别处被刷新，本地文件里的 token `expiresAt` 仍在未来，但上游已拒绝 → `NeedsRefresh` 恒为假、永不刷新、
    积分恒 0、明细恒空。因此积分/明细/费率路径遇 `ErrSessionDead` 必须「refresh + 落盘 + 重试一次」
    （`app.refreshIfSessionDead`，scheduler 的 checkin 路径同理）。
    - **上游错误必须类型化**：自愈链全靠 `errors.As` 取 `*provider.Error` 判 `Kind`。
      目录/费率接口曾用裸 `fmt.Errorf` ⇒ 模型面与费率面的 401 自愈**永远不生效**（R79）。
      新增任何上游调用时，≥400 一律返回 `&provider.Error{Kind: Classify(...), …}`。
20. **凡是调 `Upstream.RefreshToken` 的地方必须紧跟 `SaveAtomic`**：refresh token 会轮换，不落盘 = 下次启动用旧 refresh token，
    重回上一条的死锁（`RefreshPricing` 曾漏，已补）。
21. **虚拟账号/单账号渠道不得进入任何「能把它弄没」的路径**：`reloadAccounts` 不纳入（`SyncToDir` 会剔除），
    启动时 `SetDisabled(uid,false)` 兜底自愈；`RemoveAccount`/`DisableAccount` 对此类账号硬拒（后端拒 + 前端无入口）。
    其 `Auth.ExpiresAt` 必须为远期值（不得为 0），否则 `NeedsRefresh` 恒真 → 反复 `RefreshToken` + 冷却。
    > 历史上唯一的无账号渠道是 OpenCodeZen（`oczen/*`），已于本项目移除。
    > 该渠道留下的两处机制仍在代码里，将来接入新的匿名通道可直接复用：
    > `Runtime.SingleAccount`（下条）与 `NO_CREDITS` 空集（前端「积分不适用」显示）。
22. **单账号渠道（`Runtime.SingleAccount`）不得施加任何账号级惩罚**：唯一账号且不可重登 ⇒ 任何惩罚
    （冷却/计数/禁用）都等于整条渠道下线。故 `Runtime.SingleAccount=true` 的渠道在 handler 里
    **传输层错误与 `status>=400` 一律原文透传**，不走 `NoteError`/`Cooldown`/`Disable`。
    - **429 也不冷却**：无号可轮换，冷却后后续请求在挑号阶段被挡成 `503 no_healthy_account`，
      不如透传 429 让客户端按 `Retry-After` 退避。
    - **传输层错误不累计 `errCount`**：否则 3 次网络抖动（默认 `ErrThreshold`）即冷却唯一账号。
    - 启动时 `Pool.ClearPenalty(uid)` 自愈遗留冷却（`SetDisabled` 只清禁用，不清冷却）。
    - **严禁**把 401/403 归为 `ErrSessionDead`（会 `pool.Disable` 永久禁用且无法人工恢复）。
    - 该渠道的 `Classify` 仍做语义分类（供日志），但**不再用于决定惩罚**（惩罚判定前已短路）。
23. **Qoder 双区签到只能走 campaigns，且状态必须结构化上报**：
    - **绝不调用 legacy `daily-check-in/claim`**：该端点已全局 DISABLED，却对未领取日恒返回 409，
      会被误判成「今日已领取」而跳过真实领取 → 假成功、零积分（上游 `99ab022` 同款结论，2026-09-21 抓包实测）。
      只有 `GET /sash/api/v1/me/campaigns` → `POST .../campaigns/{id}/claim` 会真实发放 100 Credits。
    - **401 必须返回 `*provider.Error{Kind: ErrSessionDead}`**，不得用裸 `fmt.Errorf`：调度器
      `isSessionDead` 走 `errors.As` 类型断言，裸 error 会让它恒为 false，自愈失效（不变式 19）。
    - **结果状态经 `provider.CheckinReporter` 上报**（`CheckinClaimed`/`Already`/`NoCampaign`/`NoToken`/`Error`）：
      `error` 通道无法区分 `no_campaign` 与 `error`——两者都不是「已签到」，却都需在窗口内重试。
      **`Msg` 必须透出**，不得被调度器覆盖成 `"ok"`，否则面板无法区分「真领到 100」与「活动还没上线」。
    - **签到窗口必须带重试**：每日 10:00（UTC+8）开放，活动可能在整点后才创建，
      故 `CheckinMinutes=[10:00]` + `CheckinRetryUntil=12:00`，窗口内每分钟重试，
      直到全部账号达 `claimed`/`already` 才算当日完成（上游 `ae3d42f` 修的就是漏领一天）。
      完成标记按「日期+时段」而非仅日期——本工具支持一天多时段（09:00/21:00），
      只按日期标记会让早间成功吞掉晚间时段。
24. **缓存字段是「可选上报」，不得把「不提供」显示成『0%』**：上游 usage 里是否回缓存命中字段
    因渠道而异（实测：WorkBuddy 系 `prompt_cache_hit_tokens`、Trae 系 `cache_read_input_tokens`、
    其余 `prompt_tokens_details.cached_tokens`；小浣熊回**空对象**、清言/Loomy 整条流无 usage）。
    判定集中在 `internal/server/cacheusage.go`，必须区分两种情形：
    - 「字段存在但为 0」（如 `prompt_cache_hit_tokens:0`）= 上游明确的「本轮未命中」⇒ 显示 0%；
    - 「字段不存在 / details 为空对象」= 上游不提供该维度 ⇒ 显示「—」，**绝不得显示 0%**。
    落地细则：`UsageEntry.CRC` / `ModelStat.CRCov` / `TokenStats.CRCov` 为零即代表「无样本」；
    利用率 = `CR ÷ CRCov` 的**加总口径**（未上报的请求整条不进分母，不稀释命中率）；
    三个字段一律 `omitempty`，保证旧流水读出来就是「未上报」（不变量 5 的向后兼容落地）。
    **分母不得做 `PT + cacheRead`**：实测 workbuddy 系 `hit+miss==prompt_tokens`、
    trae 系 `total==prompt+completion` ⇒ 命中数是 prompt_tokens 的**子集**（与 Anthropic
    官方口径相反）。副产品：利用率恒 ∈ [0,1]。回归测试 `internal/server/cacheusage_test.go`、
    `internal/ledger/cache_test.go`、`internal/stats/cache_test.go`、`scripts/ui-static-check.mjs`。
    **唯一会重建 usage 的渠道是 monkeycode**（只拼 pt/ct/total）⇒ 搬字段必须显式加，
    否则上游给的缓存数据在网关内部就被丢掉（本项就是它被发现的原因）。

---

## 3. 架构与渠道（摘要）

**架构选型**：

| 主题 | 选型 | 理由 |
|------|------|------|
| GUI 壳 | **无**（删除 wails） | WebView2 内存开销大 + Windows 绑定；托盘 + 浏览器足够 |
| 托盘 | energye/systray v1.0.3 | 已跨平台；菜单固定方案规避其不可删菜单项限制 |
| 管理后端 | 现有 http.Server 扩展 `/api/*` | 单端口、复用鉴权中间件（cookie 会话守卫）、零新服务 |
| Web UI | 纯静态 embed + fetch | 无 Node 构建链，单 exe 双击即用 |
| 登录 | `internal/login` + `login_*` | 纯 HTTP + 本地回调端口（或 CDP 捕获），跨平台 |
| 平台能力 | `internal/platform` + build tag | 浏览器无痕/开机自启/消息框/日志/工作区，接口同名 |
| 构建 | WSL 交叉编译 win；CI macos-latest 编 darwin | 见 R11 |

**托盘菜单**（固定，不做动态内容）：`打开主界面` / `查看日志` / `退出`；
单击与双击 = 打开主界面；各菜单项用不同颜色纯 Go 生成图标。

**平台能力差异表**（`internal/platform`）：

| 能力 | Windows | macOS | Linux |
|------|---------|-------|-------|
| 打开浏览器 | rundll32 url.dll | `open <url>` | xdg-open |
| 系统消息框 | MessageBoxW | osascript display dialog | stderr |
| 开机自启 | 注册表 Run | LaunchAgent plist | 未实现 |
| 打开日志文件 | notepad | open -a TextEdit | xdg-open |
| 确认框 | MessageBoxW YESNO | osascript buttons | 默认否 |
| 无头模式 | --no-tray | --no-tray | --no-tray（推荐） |

**面板区块与 API 全量清单**见 [`docs/接口参考.md`](docs/接口参考.md)。

### 渠道清单（20 条内建 + 自定义）

WorkBuddyCN（`workbuddy/*`）、WorkBuddyAI（`workbuddyai/*`）、TraeWork（`traework/*`）、
TraeCode（`traecode/*`）、QoderCN（`qodercn/*`）、QoderCOM（`qodercom/*`）、
千问办公（`qwenwork/*`）、MonkeyCode（`monkeycode/*`）、小浣熊（`raccoon/*`）、
Loomy（`loomy/*`）、智谱清言（`glm/*`）、OpenCodeZen（`oczen/*`，匿名）、
Kilo（`kilo/*`，免密免费池、无账号）、MiniMax Code（`minimax/*`，设备码授权）、
LobsterAI（`lobsterai/*`，本机回调授权）、ZCode（`zcode/*`，设备授权；积分领取依赖网页验证码，本工具不自动领取）、
Gemini（`gemini/*`，本机回调授权）、CodeArts（`codearts/*`，PKCE 授权）、
Cline（`cline-free/*`）、华为云果办 OfficeAce（`officeace/*`）；
另有用户自定义 OpenAI 兼容渠道（`custom_channels`，R66）。
旧 Qoder（`qoder/*`）已从界面下线，路由保留。

**新增渠道步骤**：① `internal/<channel>/` 实现 `provider.Upstream`；
② `internal/auth` 加 `Load<Channel>Dir()`（注意 glob 边界：`qoder*.json` 会吞 `qodercn-`/`qodercom-`）；
③ 注册 `server.Runtime` + `app.Runtime`；④ 前端渠道选择器加项 + `internal/login_<channel>`；
⑤ **`go xxxSch.Run(sctx)`**（R34，漏了会静默不跑）；⑥ 补回归测试。
> 无账号渠道（oczen / kilo）跳过 ②④，虚拟账号由 `main` 装配时注入。

**各渠道协议要点、凭据模型、已知限制与踩坑**见 [`docs/渠道接入备忘.md`](docs/渠道接入备忘.md)。

---

## 4. 构建与发版

> ⚠️ **本地构建是日常约束**：任何代码变更后都要重新构建 `dist/workbuddy-api.exe`（见 §2 第 0 条）。
> CI 不生成该文件，且 `dist/` 不入版本控制。
> 标准流程：`go build ./... && go vet ./... && go test ./...` 全绿后再构建。

```bash
# Windows（本机直接构建，或 WSL 交叉编译）
bash scripts/build_release.sh          # 发布用构建（含 -trimpath -s -w + 产物自检）

# macOS（需 macOS 真机或 CI，cgo 必需）
GOOS=darwin GOARCH=arm64 go build -o dist/workbuddy-api-darwin ./cmd/workbuddy-api

# Linux 无头
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o dist/workbuddy-api-linux ./cmd/workbuddy-api
```

构建后核对版本号已进二进制（防止拿到旧文件）：

```bash
# Windows bash 下用 python 字节计数（strings 对 Go 二进制的长串不可靠）
python -c "b=open('dist/workbuddy-api.exe','rb').read(); print('new:',b.count(b'2.8.0'),'old:',b.count(b'2.7.3'))"
# 期望：new >= 1 且 old == 0。若旧版本号仍在，说明构建未生效。
```

### 发版（tag 触发）

push `v*` tag → GitHub Actions 构建五平台产物并创建正式 release。
**release note 用仓库根目录的 `RELEASES.md`**（按版本从旧到新排列，新版本说明**append 到文件末尾**）。
CI 从 `RELEASES.md` 截取对应版本段落（`# workbuddy-api vX.Y.Z` 起）作为手写摘要，
而不是 `--generate-notes`；段落缺失时回退自动生成，不阻塞发版。

```bash
# 发版前确认：版本常量已 bump（internal/app/app.go const Version）、
# RELEASES.md 末尾已追加对应版本段落（# workbuddy-api vX.Y.Z）且与 tag 名一致、
# dist/workbuddy-api.exe 已本地重建验证
git tag vX.Y.Z && git push origin vX.Y.Z
```

> **重发同一版本**：`gh release delete <tag> --yes --cleanup-tag`（或 API 删 release + 删 tag）
> → 重打 tag 指向新提交 → 推送。CI 的 `gh release create` 遇到已存在的 tag 会失败，**必须先删干净**。

---

## 5. 开发期上游取证（devdump）

以下备忘被 R16 / 不变量 22 / 不变量 23 等决议引用，但**尚未入库**（`.gitignore` 白名单未反选，仅本地可见）：
`internal/server/handler.go` 的 `Runtime.SingleAccount` 分支、`internal/qwenwork/`、`internal/qodercn/`、
`internal/qodercom/`、`internal/glm/`（含 CDP 登录编排 `internal/login_glm/`）（R22–R38 引用，随 PR #46 新增，
作者未提交入库）。（部分可能已丢失，仅存在于历史会话中）。
如需转为本仓可查，在 `.gitignore` 补 `!docs/<文件名>` 并在上方列表添链接。
**例外**：glm 渠道的协议依据（R22–R38 引用）**不在本地**——作者已将其开源为独立仓库
[glm2api](https://github.com/ttales430/glm2api)（签名算法/认证流程/SSE 语义/积分机制/智能体清单），
引用一律指向该仓库，不要找本地文件。

### 6.1 docs/ 白名单制规则

`.gitignore` 中 `docs/*` 默认忽略全部文档，仅 `!docs/<文件名>` 显式反选的才入库。

- **新增需要入库的文档时，必须同时做两件事**：① 在 `.gitignore` 追加一行 `!docs/<文件名>`；
  ② 在 §6 的表格里加一行链接。
- **验证入库状态**：`git check-ignore -v docs/<文件名>`（无输出 = 已入库；有输出 = 仍被忽略）。
  或 `git ls-files docs/` 查看实际入库清单。
- **`AGENTS.md` / `README.md` / `DEVELOPMENT.md` 中所有 `.md` 链接都必须指向已入库文档**，
  否则他人克隆仓库后会看到死链。提交前用下面这条自查：

```bash
# 只取 markdown 链接目标（]（docs/xxx.md））——不要把反引号里的"未入库文档"也算进来
rg -o '\]\((docs/[^)]+\.md)\)' -r '$1' AGENTS.md README.md DEVELOPMENT.md \
  | sort -u | while read f; do
  git check-ignore -q "$f" && echo "❌ 未入库（死链）: $f" || echo "✅ $f"
done
| R105 | **到期判定必须按「精确时刻 + 自然日」，「到期日字符串比大小」是错的**：七个渠道把上游到期时刻截成 `YYYY-MM-DD`（qwenwork `valid_to`、traework `expire_time`、workbuddy `CycleEndTime`、lobsterai/officeace/zcode 的日期串），面板再拿日期字符串比 `today`/`tomorrow`。后果有两个：① 上游每日类额度的真实形态是**次日 00:00 失效**，截成日期后落到「明日到期」桶，实测 415 分显示「明天到期」而实际只剩 5.7 小时（2026-10-19 用户实测）；② 「7 日内到期」只取**每账号最早到期日那一天**的额度，workbuddy/traework 每账号 20+ 个每日包全被漏掉，真实敞口 4938 分被低估成 1788 分。修法：① 七个渠道的到期值统一改成 **RFC3339 保留时分秒**，`loomy` 上游无到期字段就**留空不伪造**；② 判据收口到 `provider.ExpireBucketOf` / `ExpireWithinDays` / `ExpiringWithinDays`：先 `ExpireInstant` 取精确时刻，减 `expireMidnightGrace`（**5 分钟**，实测上游抖动 0～7 秒）后取**自然日**分桶——**次日 00:00 归今日**；③ `stats` 侧把「每账号最早日+额度」换成**逐条目** `[]ExpireItem`，三个桶逐条目累加；④ 「最近临期」按**剩余时长**升序并显示 `5.7 小时后`（同一天里也要能看出谁先没）；⑤ 临期阈值（设置项 + 调度器）同步改成自然日语义（今天算第 1 天），不用 `now + N×24h`——那个窗口会随设置时的钟点漂移。**伪键必须保持日期粒度**（`provider.ExpireDate`），否则升级瞬间每包产生一对假的 expire+earn 流水。顺带修掉 lobsterai 无时区串按 UTC 解析导致的 8 小时偏差。测试：`TestExpireBucketOf`（19 例边界）、`TestExpireWithinDays`、`TestExpiringWithinDaysSum`、`TestExpireBucketsByInstant`、`TestExpiryRowsHoursLeft`；7 处变异全部确认变红 |
