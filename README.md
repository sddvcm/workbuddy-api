# workbuddy-api

> 多渠道账号聚合工具 —— 把 12 个平台托管模型的多个账号聚合成**一个 OpenAI 兼容 API**，
> 系统托盘常驻，浏览器管理，Windows / macOS / Linux 通吃。

[![Go](https://img.shields.io/badge/go-1.25-blue)](go.mod)
[![License](https://img.shields.io/badge/license-MIT-green)](LICENSE)

---

## 一、它解决什么问题

你有一堆 AI 编程平台的账号（WorkBuddy、TraeWork、Qoder、千问办公……），
每个平台的客户端各自独立、用量各自结算。本项目把它们收拢成**一个 API 端点**：

```
Claude Code / Codex / Cursor / 任意 OpenAI 兼容客户端
                    ↓  一个地址 + 一个 Key
              workbuddy-api  ← 账号池 + 负载均衡 + 自动轮换
                    ↓
      WorkBuddy×N  TraeWork×N  Qoder×N  千问办公×N  GLM×N  …
```

附带解决四件事：**多账号自动轮换**（单个账号额度用尽自动换下一个）、
**积分最大化利用**（临期积分优先烧掉，避免到期作废）、
**自动签到保活**（定时领额度、维持会话）、
**用量与积分对账**（token 与积分双流水，面板可查）。

---

## 二、功能总览

### 2.1 渠道聚合（20 个）

| 渠道前缀 | 平台 | 手动签到 | 添加账号方式 |
|---|---|---|---|
| `workbuddy/` | WorkBuddy 国内版（CodeBuddy） | ✅ | 浏览器 OAuth / 本机客户端导入 |
| `workbuddyai/` | WorkBuddy 国际版 | ❌ 自动领日活奖励 | 浏览器 OAuth / 本机客户端导入 |
| `qodercn/` | QoderCN | ✅ | 浏览器授权 |
| `qodercom/` | QoderCOM 国际版 | ✅ | 浏览器授权 |
| `traework/` | TraeWork | ✅ | 浏览器 OAuth |
| `traecode/` | Trae 代码版（与 TraeWork 共账号） | — | 同 TraeWork |
| `qwenwork/` | 千问办公 | ❌ 无签到活动 | 浏览器授权 |
| `glm/` | 智谱清言 | ✅ | CDP 自动捕获 / 手工粘贴 |
| `monkeycode/` | MonkeyCode（平台托管模型） | ✅ | 仅本机客户端导入 |
| `raccoon/` | 小浣熊 | ✅ | 浏览器授权 **或** 客户端导入 |
| `loomy/` | 讯飞 Loomy | ✅（需当日有效对话轮次） | 仅本机客户端导入 |
| `officeace/` | OfficeAce（华为云果办） | ✅ | 本机桌面端导入 **或** OAuth |
| `minimax/` | MiniMax Code 中国版 | ✅ | OAuth 设备码登录 |
| `lobsterai/` | LobsterAI（有道龙虾） | ✅ | 本机回调 OAuth 登录 |
| `codearts/` | CodeArts（华为云） | ✅ | PKCE OAuth |
| `cline/` | Cline 免费池 | ❌ Key 不过期，无需签到 | 粘贴官方 `sk_` API Key |
| `kilo/` | Kilo AI 免费池 | ❌ 免密、无账号 | 无需添加（自动注入匿名账号） |
| `gemini/` | Gemini（Google Code Assist） | ❌ 配额为窗口百分比 | OAuth |
| `zcode/` | ZCode（智谱） | ❌ 积分领取未实现（依赖验证码） | 设备授权登录 |
| `qoder/` | QoderWork | — | 路由保留，界面已下线 |

**导入型渠道**（`monkeycode`、`raccoon`、`loomy`、`officeace`）的凭据由本项目**不产生**，
从已登录的官方客户端读取。这类凭据通常有有效期（如 Loomy 约 14 天），
到期需重新导入。小浣熊两个路径都支持，弹窗里会同时给出两个按钮。

浏览器授权登录的做法是：登录期间临时把该渠道的自定义协议回调指向本程序，
以接住授权码并完成 token 兑换，结束后立即恢复注册表。

模型名前缀路由：客户端可直接请求 `qodercn/glm-5.2`，也可配置默认渠道后
直接写裸模型名。未匹配任何渠道的模型名按「默认渠道」处理。

### 2.2 三协议兼容

| 端点 | 协议 | 说明 |
|---|---|---|
| `POST /v1/chat/completions` | OpenAI Chat | 主端点，流式 / 非流式 |
| `POST /v1/responses` | OpenAI Responses | 兼容 Codex CLI 的事件流格式 |
| `POST /v1/messages` | Anthropic Messages | 兼容 Claude Code，含 tool_use 增量 |
| `POST /v1/messages/count_tokens` | Anthropic | token 计数 |
| `GET /v1/models` | OpenAI | 模型目录 |

三协议由**边缘兼容层**（`internal/gateway`）转成内部统一的调用，
内层 handler 不动。`reasoning_content` 在 Anthropic 协议下正确映射为
`thinking` / `thinking_delta`，多轮回填不丢。

### 2.3 账号池调度

- **三档挑号策略**（`config.strategy`）：`expire` 临期优先（默认）、
  `credits` 积分优先、`roundrobin` 负载均衡
- **会话粘性**：同一 `conversationId` 恒定走同一账号 —— 命中上游 prompt 缓存，
  多轮对话里工具调用 ID 与 assistant offset 也才对得上
- **两级冷却**：账号级（余额耗尽 / 限流 / 连续报错）与**模型级**（429 按模型限额，
  同号其它模型不陪葬）；短时间内第二个模型撞墙才升级为整号冷却
- **错误分类**：区分「请求问题」与「账号问题」—— 内容拦截 / 上下文超限**不罚号**；
  候选耗尽时透传最后一次上游错误原文，而非包装成笼统的 503

### 2.4 成长任务体系

仅 WorkBuddy 国内版提供。可在面板一键推进成长任务、领取奖励：

- **成长任务**：22 个动作覆盖 15 个常规任务 + 7 个小程序口径任务（小程序口径默认不跑），
  进度达标自动回读领奖 —— 上游字段名写错时仍会返回 200 只是不入账，故以**回读**判定成功
- **连登兑换与抽奖**：连登 7/14/28 天档位自动兑换、抽奖次数自动抽完
- **猫猫旅行**：自动派出 + 到站领取
- **活跃上报**：点亮连登、解锁领养前置

> ⚠️ 兑换与抽奖会**真实改变账号状态**（抽奖次数用掉不可撤销），
> 故默认关闭，需在面板显式开启。

### 2.5 一键领取额外补偿

仅 WorkBuddy 国内版提供。上游发放的活动补偿（如「DeepSeek 模型服务异常额外补偿 500 积分」）
是**按账号**发放的，而个人中心一次只能登录一个账号 —— 账号多的用户逐个登录领取，
很容易错过截止时间。面板「一键领取额外补偿」对全部账号批量处理：

- **先查状态再领取**：`active` / `claimed` 两个字段决定是否发写请求，
  已领取、活动未生效的账号不会被无谓打扰（重复点击也安全）
- **逐账号结果**：每个账号单独报告「本次已领取 / 早已领取 / 活动未生效 / 失败」与截止时间
- **到账即刷新**：领取完成后自动刷新积分，面板余额立即反映

> 端点与响应形状来自 2026-10-19 对个人中心前端的实测（`/billing/meter/compensation-status`
> 与 `/billing/meter/claim-compensation`），非猜测。

### 2.6 自动签到与保活

- 每日定时签到领额度，时间点可在面板配置（部分渠道带窗口重试，避开上游活动延迟）
- Token 保活调度，冷却状态机避免反复打扰上游
- 积分后台自动刷新，401 掉线自动自愈

### 2.7 用量与积分双流水

`internal/ledger` 两套**独立口径**的流水，互不折算：

- **token 流水**：按渠道 × 模型聚合，面板出折线图与模型用量榜
- **积分流水**：按账号记 `earn` / `spend` / `expire`，可对账

写入仅 append 缓冲（30s 自动 flush），读取时才按月分段扫描聚合，
**常驻内存接近 0**。首次见到的账号只记一条「存量额度」基线，不逐条展开。

### 2.8 运行统计

面板第三个标签页：今日签到收入 / 花费 / 账号数里程碑、平台概览、
使用中接棒（粘性路由实时视图）、最近临期与预计作废、模型消耗、
请求日志（TTFB / 吞吐 / 状态码）、异常账号、运行日志。

### 2.9 安全

| 项 | 说明 |
|---|---|
| **首次启动自动生成随机 API Key** | `crypto/rand` + base64url，**打印在日志里**。没有写死的默认 Key |
| **非环回监听强制管理密码** | 对外监听 + 空密码 = **拒绝启动**，防止无凭据暴露面板 |
| **管理面板 cookie 会话** | token 只存 SHA-256，HttpOnly + SameSite=Lax，改密码即失效，登录失败 5 次/IP 锁 5 分钟 |
| **API Key 常量时间比较** | 先哈希再比，避免时序侧信道 |
| **Windows 凭据 DPAPI 加密** | 账号文件里的 token 用本机 DPAPI 加密 |
| **请求体指纹脱敏** | 清除客户端注入的模板句，降低上游内容误拦截 |
| **安全响应头** | `X-Frame-Options: DENY`、`nosniff`、`Referrer-Policy: no-referrer` 等；HTML 附 CSP（防点击劫持等） |
| **单实例锁** | `CreateMutexW`（`Local\` 命名空间），重复启动立即退出并提示 |
| **无自动导入凭据** | **不扫描**用户目录、不静默复制任何登录态 —— 账号只能面板登录或手工放置 |

### 2.10 部署形态

- **Windows / macOS**：双击或命令行启动，系统托盘常驻，托盘菜单
  「打开主界面 / 查看日志 / 退出」
- **Linux 无桌面**：加 `--no-tray` 无头运行
- **Docker / fnOS NAS**：`docker/` 下有完整 compose、离线镜像 tar、
  fnOS 专用 compose 与构建脚本。二进制内嵌 tzdata，容器内时间无需额外配置

---

## 三、快速开始

### 3.1 Windows

1. 从 [Releases](https://github.com/sddvcm/workbuddy-api/releases) 下载
   `workbuddy-api-windows-amd64.exe`，放到**任意目录**（程序会自建数据目录）
2. 双击启动。**首次启动会自动生成一个随机 API Key 并打印在日志里** ——
   记下来，或之后在面板里查看
3. 托盘图标出现后，浏览器打开面板添加账号

程序目录结构（首次启动后自建）：

```
workbuddy-api.exe
config.json      # 配置（首次启动生成）
auths/           # 各账号凭据（Windows 下 DPAPI 加密）
data/            # 状态、流水、日志
```

> ⚠️ Windows SmartScreen 提示「未知发布者」是未做代码签名的正常现象，
> 自行判断是否信任。程序为单文件绿色版，删除该目录即可完全卸载。

### 3.2 Docker

```bash
cd docker
cp .env.example .env
# 编辑 .env：生成两个强随机值填入 ★必改 项
head -c 32 /dev/urandom | base64 | tr -d '/+='   # 用于 API_KEY
head -c 18 /dev/urandom | base64 | tr -d '/+='   # 用于 ADMIN_PASSWORD
docker compose up -d
```

镜像离线构建（目标机无网络时）：`docker/build-image.sh` 产出 tar，
再 `docker/load-image.sh` 导入。

详见 [`docker/README.md`](docker/README.md)。

### 3.3 接入 AI 客户端

客户端填三个参数：**Base URL** `http://<你的地址>:7863/v1`、
**API Key**（启动日志里那个）、**模型名**（`GET /v1/models` 可查）。

Claude Code / Cursor 等支持自定义 OpenAI 端点的工具直接填即可；
支持 Anthropic 协议的客户端用 `/v1/messages`，注意 Base URL 不要带 `/v1`
（由客户端自己拼）。

<details>
<summary>常见客户端的模型名映射示例</summary>

若客户端只认特定模型名，可在面板「设置 → 兼容层」配置映射：

```json
{
  "claude-*":  "qodercn/glm-5.2",
  "gpt-5*":    "workbuddy/deepseek-v4-pro",
  "*":         "workbuddy"
}
```

`default_channel` 指定不带前缀的模型名走哪个渠道。

</details>

---

## 四、面板操作

| 分区 | 能做什么 |
|---|---|
| **账号** | 添加（按渠道登录/导入）、签到、刷新积分、一键领取额外补偿、停用、删除、改昵称、积分明细、成长任务 |
| **模型** | 模型目录与费率、复制模型名、导出配置 |
| **用量** | token 与积分双流水、折线图、模型用量榜 |
| **统计** | 里程碑、平台概览、临期预警、请求日志、运行日志 |
| **设置** | 监听地址、API Key、管理密码、签到时间、挑号策略、兼容层映射、单渠道代理、成长任务开关 |

**面板鉴权**：监听 `127.0.0.1` 时免登录；监听非环回地址时强制要求管理密码。

---

## 五、配置

完整示例见 [`config.example.json`](config.example.json)，容器部署见
[`docker/.env.example`](docker/.env.example)。

几个值得注意的项：

| 配置 | 默认 | 说明 |
|---|---|---|
| `api_key` | 首次启动随机生成 | **空 = 接口不鉴权** |
| `admin_password` | 空 | 空仅允许在环回监听下启动 |
| `strategy` | `expire` | 挑号策略，非法值启动即报错而非静默回落 |
| `max_rotate` | `3` | 单请求最多轮换几个账号（上限 20） |
| `session_sticky` | `true` | 会话粘性，客户端不发会话 id 时自动退化为请求计数粘性 |
| `schedule.growth_tasks` | `false` | 成长任务一键完成（仅 WorkBuddy 国内版） |
| `schedule.growth_tasks_include_mp` | `false` | 执行时是否包含小程序口径任务 |
| `schedule.growth_redeem_streak` | `false` | 连登兑换 + 抽奖 |
| `schedule.growth_travel` | `false` | 猫猫旅行自动派出 + 到站领取 |
| `proxies` | 空 | 单渠道上游代理，`{"渠道名": "socks5://host:port"}` |

> ⚠️ 后四项**开启会真实改变账号状态**（抽奖次数用掉不可撤销），默认全部关闭。

环境变量可覆盖部分配置（`WILDWORK_*` 前缀），完整清单见 `docker/.env.example`。

---

## 六、项目整合来源

workbuddy-api 整合了三个开源项目的能力。三者同源，都出自早期的
`xxx2api` 系列，但各自解决不同问题、互不包含：

| 来源项目 | 定位 | 本项目从中整合的内容 |
|---|---|---|
| [sddvcm/workbuddy-wild](https://github.com/sddvcm/workbuddy-wild) | 同作者的旧项目，Wails 桌面形态 | **账号调度实战经验**：签到遇限流时轮换设备号重试、从本机客户端读取真实设备号、账号挑号策略（临期/积分/轮询）、Windows 凭据 DPAPI 加密、单实例锁、离线镜像构建 |
| [linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel) | 专注 WorkBuddy 成长任务体系 | **成长任务全链路**：成长中心协议层（任务列表/接受/领奖）、连登兑换与抽奖、猫猫旅行、活跃上报、一键完成任务编排 |
| [rockswang/wild-work](https://github.com/rockswang/wild-work) | 多渠道聚合 + 纯 Go Web UI | **项目基线**：多渠道 provider 抽象与路由、三协议兼容层、Web 管理面板、用量与积分双流水、流式空闲监控、请求体脱敏、运行统计面板、Docker 部署 |

### 各自贡献的具体位置

| 来源 | 主要代码 |
|---|---|
| workbuddy-wild | `internal/traework/client.go`（换号重试）、`internal/login_trae/device.go`（设备号读取）、`internal/pool/strategy.go`（挑号策略）、`internal/auth/secure_windows.go`（DPAPI）、`internal/platform/`（单实例锁） |
| workbuddy2api-panel | `internal/upstream/growth.go`、`growth_tasks.go`、`desktop.go`、`mp_report.go`、`growth_auto.go`、`internal/scheduler/growth.go` |
| wild-work | `internal/provider/`、`internal/gateway/`（三协议）、`internal/ledger/`、`internal/idle/`、`internal/sanitize/`、`internal/stats/`、`cmd/workbuddy-api/web/`（面板） |

**三者的边界很清楚**：workbuddy-wild 只有 2 个渠道、workbuddy2api-panel 只有
WorkBuddy 单渠道、wild-work 没有成长任务体系 —— 任何一方都不具备本项目的
完整形态。本项目做的事情是把它们拼起来并解决随之而来的问题，例如：

- wild-work 的 TraeWork 模型目录不按 function 过滤，「列表里列得出、一调用就
  4001」，需要双池合并并登记模型归属才能消除；
- workbuddy-wild 的 TraeWork 签到换号重试在 wild-work 里没有，缺失会导致
  多账号共用设备号时抢不到当天额度；
- panel 的任务领奖端点只在 `WebBase` 域提供，而 wild-work 只有 chat 与 billing
  两个域，不补上则领奖恒返回 400。

### 上游同步记录：wild-work v2.6.2 → v2.9.3（2026-10-19，v0.2.0）

基线追平 `rockswang/wild-work` 的 **v2.9.3**（143 个提交、390 个文件改动、
约 +7.9 万行）。合并采用「树归一化 + 三方合并」：把上游 `cmd/wild-work/` →
`cmd/workbuddy-api/`、模块名 `wild-work` → `workbuddy-api` 后再合，避免整仓重命名噪声。

**本次吸收的上游能力**

| 能力 | 说明 |
|---|---|
| 新渠道 | Cline、Kilo（免密免费池）、OfficeAce、Minimax、LobsterAI、ZCode、Gemini、CodeArts |
| backuplink | 决议 R57：单渠道多上游地址的备用通道 |
| 模型降级链 | `ModelFallback`：命中额度/限流时按规则透明换模型，客户端无感 |
| 思考强度注入 | `ReasoningEfforts`：按模型注入 reasoning_effort，含 `*` 通配回退 |
| 会话失效判定 | `SessionDead` + `MarkSessionDead`，配合会话粘性 |
| 单账号渠道 | `SingleAccount`：无号可轮换时不冷却，原样透传上游错误 |
| 风控与保护 | loopguard、WAF IP 级 fail-fast、模型级冷却 |
| 凭据保护 | `internal/officeace` 的 DPAPI 读取（Windows 专属） |

**本地保留、上游没有的部分**（合并时逐处保留）

成长任务体系（`internal/upstream/growth*.go`、`desktop.go`、`mp_report.go`、
`report.go`）、账号挑号策略（`internal/pool/strategy.go`、`max_rotate`）、
会话粘性、模型列表的费率后缀（`internal/provider/modelsuffix.go`）、
`internal/auth` 的 DPAPI 加密落盘、`internal/platform` 单实例锁。

**刻意的分歧**

- `provider.Oczen`：代码与常量保留（跟随上游，便于后续继续合并），但**不登记 Runtime**
  —— 模型列表、路由、费率面板都不出现该渠道。本仓不再使用 Oczen 匿名通道。
- `cmd/workbuddy-api/proxy.go`（本地每渠道代理）已被上游 backuplink 取代，删除。

> 再次说明：**本项目不产出、不包含、不分发任何协议逆向研究成果。**
> 三者都是集成与聚合，各渠道的接口参数、签名算法、风控指纹等底层知识全部来自
> 下表各上游项目的公开成果；所有逆向相关的功劳与风险归于下述各项目作者。

---

## 七、致谢：各渠道协议来源

本项目的渠道协议实现参考了以下开源项目（各渠道能够落地的前提）：

| 项目 | 贡献 |
|---|---|
| [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) | WorkBuddy 国内版接口 |
| [Sliverkiss/qoderwork2api](https://github.com/Sliverkiss/qoderwork2api) | QoderWork 接口 |
| [wicm84266964/Buddy2api](https://github.com/wicm84266964/Buddy2api) | 千问办公渠道 |
| [wpy030414/xrl-router-plugin-qwenwork](https://github.com/wpy030414/xrl-router-plugin-qwenwork) | 千问办公渠道 |
| [iceloon/dsh-workbuddyai-connect](https://github.com/iceloon/dsh-workbuddyai-connect) | WorkBuddy 国际版接口 |
| [corrinehu/dsh-workbuddy-connect](https://github.com/corrinehu/dsh-workbuddy-connect) | WorkBuddy 渠道架构（MIT） |
| [Zhengyuuuui/qoder2api](https://github.com/Zhengyuuuui/qoder2api) | Qoder 渠道 |
| [icebears111/qoderwork2api](https://github.com/icebears111/qoderwork2api) | Qoder 渠道 |
| [zhangdailin/Orchids-2api](https://github.com/zhangdailin/Orchids-2api) | Qoder 渠道 |
| [ttales430/glm2api](https://github.com/ttales430/glm2api) | 智谱清言接口 |
| [codkeep/MonkeyCodeReverseEngineer](https://github.com/codkeep/MonkeyCodeReverseEngineer) | MonkeyCode 协议对照（Apache-2.0） |
| [xxhhlk/raccoon2api](https://github.com/xxhhlk/raccoon2api) | 小浣熊渠道（MIT） |
| [xxhhlk/loomy2api](https://github.com/xxhhlk/loomy2api) | 讯飞 Loomy 渠道（MIT） |

> 若上述项目作者认为本项目的引用方式不当，请提 issue 联系，会立即调整或移除。

---

## 八、风险提示与免责声明

**风险提示**

- 本项目聚合的是**第三方平台的托管模型账号**，这些平台的免费额度来自推广活动。
  大量账号同时调用可能触发上游风控，请自行评估使用强度与后果
- 自动化签到、任务上报等行为可能与各平台服务条款冲突，**使用者需自行承担全部责任**
- 凭据文件（`auths/`）等同账号密码，**请勿分享或提交到任何公开仓库**
  （Windows 下虽有 DPAPI 加密，但换机即不可解，仍应视为敏感文件）

**免责声明**

本项目按「现状提供」发布，不作任何明示或默示担保。使用本项目造成的
账号封禁、额度损失、数据丢失等一切后果由使用者自行承担。

---

## 九、参与贡献

欢迎报 Issue、提 PR。完整指南见 **[CONTRIBUTING.md](CONTRIBUTING.md)**，要点：

- **提 PR 前本地跑三道关**：`go build ./...`、`go test ./...`、
  `node scripts/ui-static-check.mjs`。CI（`.github/workflows/ci.yml`）对每个 PR 自动跑同样几项
- **新增渠道必须走完整链路**：`provider` 常量 → `stats` 登记 → `pool` → `gateway` 路由 →
  `channelOrder` → `feeChannelOrder` → 前端渠道元信息。漏一处不报错，只会静默少一个 tab
- **提交说明写「为什么」**，涉及取舍把备选方案和放弃原因写进去——这个仓库的历史提交是这么写的
- **禁止提交任何凭据**（`auths/`、token、cookie、api_key）
- 本仓库与 `rockswang/wild-work` 保持三方合并关系，改动落在上游同期也改过的文件上时，
  请在提交说明注明，能省下双方半小时

Issue 模板已按「bug / 功能建议」分好类；不确定是不是 bug 的请走
[Discussions](https://github.com/sddvcm/workbuddy-api/discussions)，不要占 issue。

---

## License

MIT — 仅供个人学习与交流使用，请遵守各上游平台服务条款。
