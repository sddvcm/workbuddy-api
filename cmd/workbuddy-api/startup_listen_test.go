package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
)

// TestIsAddrInUse 端口占用判定。
//
// 回归背景（2026-10-22 用户报障）：第二个实例 net.Listen 失败后程序继续运行，
// 变成有托盘、无 HTTP 服务的空壳，用户症状为「进程活着但页面打不开」。
// 修正在启动路径 Fatal 退出，而提示语是否准确取决于这里的判定 ——
// 若误判（非占用错误也报「端口被占用」）会把用户引向错误方向。
//
// ⚠️ 用例里的 Errno 数值是**实测得来**，不是照抄 syscall.EADDRINUSE
// （Windows 上它是 536870914，而端口占用的真实码是 10048，见 main.go 注释）。
func TestIsAddrInUse(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		// Unix 码：net 包在 Linux/macOS 上直接返回 EADDRINUSE。
		{"Unix EADDRINUSE 裸 Errno",
			&net.OpError{Op: "listen", Err: syscall.EADDRINUSE}, true},
		{"Unix EADDRNOTAVAIL",
			&net.OpError{Op: "listen", Err: syscall.EADDRNOTAVAIL}, true},
		// Windows 真实码：实测 net.Listen 占端口时最内层 Errno == 10048。
		{"Windows WSAEADDRINUSE(10048) 裸 Errno",
			&net.OpError{Op: "listen", Err: syscall.Errno(10048)}, true},
		{"Windows WSAEADDRNOTAVAIL(10049)",
			&net.OpError{Op: "listen", Err: syscall.Errno(10049)}, true},
		// 真实错误链形状：*net.OpError → *os.SyscallError → Errno。
		{"Windows 真实链 OpError→SyscallError→Errno(10048)", &net.OpError{
			Op:  "listen",
			Net: "tcp",
			Err: &os.SyscallError{Syscall: "bind", Err: syscall.Errno(10048)},
		}, true},
		// 反例价值在于**测试它不是一个有效判据**：Windows 上真实端口占用码是 10048，
		// 而 syscall.EADDRINUSE 是 536870914（ERROR_ALREADY_EXISTS）——
		// 后者虽在 Windows 上永不出现于 net.Listen，但仍必须认（Unix 平台就是它），
		// 故此处期望 true。真正的回归保障在 TestIsAddrInUseRealOccupied。
		{"Windows 上 syscall.EADDRINUSE(536870914) 数值无关但仍认（Unix 语义）",
			&net.OpError{Op: "listen", Err: syscall.EADDRINUSE}, true},
		// 权限类错误要与端口占用区分，否则用户会去查端口而查不到原因。
		{"Unix 权限不足 EACCES",
			&net.OpError{Op: "listen", Err: syscall.EACCES}, false},
		{"Windows WSAEACCES(10013)",
			&net.OpError{Op: "listen", Err: syscall.Errno(10013)}, false},
		{"非 OpError 的普通错误", errors.New("boom"), false},
		// errors.As 需穿透外层包装。
		{"双重包装仍能识别",
			fmt.Errorf("启动失败: %w", &net.OpError{
				Op:  "listen",
				Err: &os.SyscallError{Syscall: "bind", Err: syscall.Errno(10048)},
			}), true},
		// 剥不动（有环节不实现 Unwrap）时宁可漏判也不误报。
		{"无法剥到 Errno 时返回 false",
			&net.OpError{Op: "listen", Err: errors.New("no errno")}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isAddrInUse(c.err); got != c.want {
				t.Fatalf("isAddrInUse(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// TestIsAddrInUseRealOccupied 用真实 socket 占用后 listen，验证真实错误能被判为占用。
// 这是守门用例：上面的构造用例挡不住「判据数值写错」——
// 最初的实现用的就是 syscall.EADDRINUSE，全部构造用例都过，真机一跑就失败。
func TestIsAddrInUseRealOccupied(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("无法监听本地端口，跳过：%v", err)
	}
	defer ln.Close()

	_, err2 := net.Listen("tcp", ln.Addr().String())
	if err2 == nil {
		t.Skip("本机允许同端口重复绑定（平台特性），跳过")
	}
	if !isAddrInUse(err2) {
		t.Fatalf("真实占用错误未被识别为端口占用：%v", err2)
	}
}
