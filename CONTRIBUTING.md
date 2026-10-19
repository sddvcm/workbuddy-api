# 参与贡献

欢迎报 Issue、提 PR。这个仓库同时合入了三个上游来源的代码（见 README「六、项目整合来源」），
动手前建议先读那一章——很多文件上有「本地能力 vs 上游实现」的取舍痕迹，
在已被取代的实现上做工，合并时会和上游一起炸。

## 一、提 PR 前的本地验证（三道，缺一不可）

```bash
go build ./...                      # 编译
go test ./...                       # 单测（含大量回归）
node scripts/ui-static-check.mjs    # 前端静态检查
```

CI（`.github/workflows/ci.yml`）对每个 PR 自动跑同样几项，任一亮红不会进入人工评审。
本地 Go 版本用 `go.mod` 指定的 1.25；前端检查只需要 Node 18+，无需 `npm install`。

## 二、分支与提交

- 分支名：`feat/xxx`、`fix/xxx`、`docs/xxx`、`refactor/xxx`
- 提交信息：`类型(范围): 一句话中文摘要`
  类型取 `feat` / `fix` / `docs` / `refactor` / `test` / `chore` / `merge`
- 提交说明写**为什么**，不写**改了什么**——diff 本身已经说明了发生了什么。
  涉及取舍时把备选方案和放弃原因写进去。这个仓库的历史提交是这么写的，
  后来人靠它避免重复讨论同一件事。

## 三、代码约定

- 注释用中文，与现有代码一致；解释意图、坑和「为什么不能那么写」，
  不要逐行翻译代码
- **新增渠道必须走完整链路**，漏一处不会报错，只会静默少一个 tab：
  `provider` 常量 → `stats` 登记 → `pool` → `gateway` 路由 → `channelOrder`
  → `feeChannelOrder` → 前端渠道元信息
- 任何「先探测再写」的上游请求，沿用 `internal/upstream/checkin_activity.go`
  的范式：活动未生效 / 已领取的状态**不要发写请求**（幂等，也少打扰上游）
- 面板新增批量操作时，结果面板复用 `renderTaskPanel` / `renderCompPanel`
  那一套（`.task-panel` 样式已在 style.css），不要另起炉灶
- **禁止提交任何凭据**：`auths/`、token、cookie、api_key。
  凭据文件永远不该进版本库，加密也不行（Windows 的 DPAPI 换机即不可解，
  且提交历史里的秘密删不掉）

## 四、不接收的改动

- 协议逆向的细节实现、抓包原始结果、目标平台私有接口文档
  （本项目不产出、不包含、不分发任何协议逆向研究成果）
- 与「同步上游 wild-work」无关的大范围重命名 / 目录搬移——合并面会失控，
  要动先开 issue 讨论
- 把 `.workbuddy/` 等本地工具目录提交进来（仓库级 `.gitignore` 已排除）

## 五、上游同步请注意

本仓库与 `rockswang/wild-work` 保持三方合并关系（基线 v2.9.3）。
若你的改动落在上游同期也改过的文件上，合并时大概率冲突——
提交说明里注明「该文件上游 v2.9.x 亦有改动」能省下双方半小时。

## 六、三种参与方式

| 方式 | 适合 |
|---|---|
| 报 Issue | 复现步骤清晰的 bug、明确的功能建议 |
| Fork + PR | 任何代码改动（推荐，见 `.github/PULL_REQUEST_TEMPLATE.md`） |
| 申请协作者 | 需要长期直推分支的维护者（Settings → Collaborators → Add people） |

 bug 报告请附版本号（面板「关于」或 `git describe --tags`）与涉及渠道；
日志、截图请自行抹掉 token / cookie / api_key 再贴。
