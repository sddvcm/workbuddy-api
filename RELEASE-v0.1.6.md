# v0.1.6

修复重复启动时留下的僵尸进程：进程看起来完全正常，但没有 HTTP 服务，面板打不开。

## 症状

日志出现端口占用警告后，**进程仍在运行、日志继续刷、托盘图标正常出现**，但面板打不开：

```
listen 127.0.0.1:7863 failed: bind: Only one usage of each socket address (protocol/network
address/port) is normally permitted.（面板中将提示）
```

最容易误判的地方：双击托盘图标打开面板时浏览器访问 `127.0.0.1:7863`，
请求实际打到了**先启动的那个实例**。如果那个是升级前的旧版本，用户会以为「面板坏了」，
实际上眼前这个进程的窗口根本不属于它。

## 根因

`cmd/workbuddy-api/main.go` 里 `StartServer()` 的失败分支只 `log.Printf` 打印警告就继续往下走：

- 调度器照常启动 → 日志继续输出「状态快照为空，跳过本轮记账」；
- 托盘图标照常出现 → systray 与 listener 无关；
- 但 `net.Listen` 从未成功 → 这个进程**没有 HTTP 服务**，是个只有托盘的空壳。

## 修复

启动路径绑端口失败即 `appInst.Stop()` + `os.Exit(1)`，不再留下「看着正常实则没有服务」的进程。
判定为端口占用时额外给出可操作提示（托盘菜单「退出」或 `taskkill /IM workbuddy-api.exe /F`）
并弹系统通知——否则静默退出，用户看不到任何提示。

面板热切换监听地址的路径不受影响：那条走 `SetListen` → `serveLocked`，失败时由 `apiError` 回 400。

### 端口占用的判定：两个真实的坑

新加的 `isAddrInUse` 判定端口占用，实现上踩了两个坑，都已写进代码注释：

**一、不能用文本匹配。** `strings.Contains(err.Error(), "only one usage")` 里的措辞
来自操作系统英文原文，Windows 中文版与其他平台措辞不同，会静默漏判。

**二、`errors.Is(err, syscall.EADDRINUSE)` 在 Windows 上恒为 `false`**（本机实测）。
真实错误链是 `*net.OpError → *os.SyscallError → syscall.Errno`，最内层 Errno 实测为 **10048**
（`WSAEADDRINUSE`）；而 `syscall.EADDRINUSE` 常量值是 **536870914**（`ERROR_ALREADY_EXISTS`）——
两个毫无关系的码。`errors.Is` 对 10048 也匹配不上。

最终实现是逐层 `Unwrap` 剥到 `syscall.Errno` 再比数值，Unix（`EADDRINUSE`=98）与
Windows（`WSAEADDRINUSE`=10048）两套码都认。

## 回归测试

新增 `cmd/workbuddy-api/startup_listen_test.go`：13 个构造用例 + 1 个真实 socket 占端口的守门用例。

守门用例是关键：最初实现用的就是 `syscall.EADDRINUSE`，13 个构造用例全部通过，
真机一跑立刻失败。这类「判据数值写错」的 bug 构造用例挡不住，必须有真机用例。

---

## 关于「已有实例在运行」提示

项目自 v0.1.0 起就有进程级单实例锁（`platform.AcquireSingleInstance`，
CreateMutex + `Local\` 命名空间），且调用位置在 HTTP 服务之前 —— 同版本的两个实例
会在绑端口前就被拦下并提示「程序已在运行」。

**如果你仍然看到端口占用警告**，说明占着端口的是不走这个锁的进程：多半是早于 v0.1.0
的残留版本，或另一个锁名不同的程序。处置办法：

```
taskkill /IM workbuddy-api.exe /F
```

然后重新启动。**注意关窗口不等于退进程** —— 托盘是常驻设计（见 R1/R3），
请走托盘菜单的「退出」。

## 升级方式

1. 下载 `workbuddy-api-windows-amd64.exe`，覆盖旧版即可（配置与账本数据兼容，无需迁移）。
2. macOS 用户下载 `WorkBuddyAPI-0.1.6-<arch>.dmg`。
3. Linux 用户下载对应架构的二进制。

## 本次无新增配置项

`config.json` 结构不变，老配置直接可用。