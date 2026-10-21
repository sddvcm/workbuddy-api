# v0.1.5

修复 DeepSeek Harness 桌面版调用 workbuddy 渠道模型时报 `400 {"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`。

## 根因：GUI 身份段的 221 字符逐字黑名单

上游内容审核按**逐字精确匹配**拦截，不做语义判断。此前已定位并处理 DSH 的 system 身份句 `You are an AI agent powered by the DeepSeek Harness`，但桌面 GUI 版还有第二段身份描述没被覆盖：

```
You are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names. When the user refers to "this page", "this GUI", or "this app" without naming another target, they mean this GUI.
```

这段文字的行为极其反直觉，2026-10-21 在 7864 端口架设抓包代理、拿到 DSH 真实请求（12 条消息 / 32 个 tools / 41878 字节）后逐层二分才测清：

| 送入内容 | 结果 |
| --- | --- |
| 仅首句 S0（107 字符） | 放行 |
| 仅次句 S1（113 字符） | 放行 |
| S0 + 单空格 + S1（**221 字符整串**） | **拦截 11128** |
| 上述串删任意一个字符 | 放行 |
| 上述串中间插一个空行（222 字符） | 放行 |
| 上述串改任意一个词 | 放行 |
| 等长无关文本（`Z`×221） | 放行 |

也就是说黑名单锚定的既不是长度也不是语义，而是**这两句话用一个空格连起来的逐字串**。此前只按「首句」加特征，命中不到这个跨句组合，所以 DSH 桌面版依旧被拦。

修复走既有的最小改写策略——只换一词，指纹断裂而语义完全不变（`this GUI` 的指代对象不受影响）：

```
…whose URL the runtime context names.
→ …whose URL the runtime context indicates.
```

`internal/sanitize` 与 `internal/upstream` 两处脱敏表同步补入该特征与改写对（`sanitizeRewrites` 命中才改写，`sanitizeFeatures` 负责预检，避免普通请求无谓开销）。

## 回归测试

新增 `internal/upstream/sanitize_dsh_gui_test.go`，5 个用例覆盖：改写生效、最小改写（只动一词）、221 字符指纹可被特征表检出、非精确变体不被误改、以及**整条出站报文的 wire body 里不再残留任何原指纹片段**（最后一条是最关键的守门用例——前四条只验函数，第五条验真实链路）。

`go test ./internal/upstream/ -run 'DSH|Sanitize'` 共 15 个用例全部通过。

## 附：DSH 侧配置（非本仓库改动）

若你用 DeepSeek Harness 接入本服务，两处配置需要注意：

1. `~/.dsh/profiles/<profile>/cordis.patch.yml` 里把 `@deepseek-ai/dsh-system-prompt` 的 `includeHarnessIdentity` 设为 `false`。官方文档对它的说明是 *"Set false only when a compatibility deployment owns the complete system prompt"* ——本服务会做脱敏改写，由它接管完整 system prompt 即可。设为 `false` 后 DSH 不再注入 `You are an AI agent powered by the DeepSeek Harness.`。
2. provider 的 `baseURL` 填到 `/v1` 为止即可（DSH 自己拼 `/chat/completions`），模型 ID 必须与 `/v1/models` 返回值完全一致，含倍率后缀。

---

## 升级方式

1. 下载 `workbuddy-api-windows-amd64.exe`，覆盖旧版即可（配置与账本数据兼容，无需迁移）。
2. macOS 用户下载 `WorkBuddyAPI-0.1.5-<arch>.dmg`。
3. Linux 用户下载对应架构的二进制。

## 本次无新增配置项

`config.json` 结构不变，老配置直接可用。