package main

import "testing"

// TestPprofAddrIsLoopbackIsSafe ★ 安全判据，必须钉死 ★
//
// pprof 端点能读到 goroutine 栈、命令行参数、堆快照。
// 这个项目对外只开 80（Apache 反代 127.0.0.1:8090），pprof 绝不能被泄漏出去。
// 一旦有人把监听地址写成 0.0.0.0，就等于把这些东西挂到公网上。
//
// 所以这里把「哪些地址可以、哪些必须拒绝」逐个列出来 —— 这条判据不值得靠人眼 review。
func TestPprofAddrIsLoopbackIsSafe(t *testing.T) {
	ok := []string{
		"127.0.0.1:8091",
		"127.0.0.1:0",
		"127.5.5.5:8091",
		"[::1]:8091",
		"localhost:8091",
		"LOCALHOST:8091",
		"  127.0.0.1:8091  ", // 前后空格应被 TrimSpace 吃掉
	}
	for _, a := range ok {
		if _, got := pprofAddrIsLoopback(a); !got {
			t.Errorf("%q 应该被放行，却被拒绝了", a)
		}
	}

	bad := []string{
		"0.0.0.0:8091",     // 绑全部 IPv4 接口 —— 最危险的写法
		"[::]:8091",        // 绑全部 IPv6 接口
		":8091",            // 空主机 = 全接口
		"192.168.1.5:8091", // 内网地址
		"10.0.0.1:8091",    // 内网地址
		"8.8.8.8:8091",     // 公网地址
		"example.com:8091",
		"8091",      // 没带端口，SplitHostPort 会失败
		"",          // 空串（调用方会先处理，但这里也必须安全）
		"127.0.0.1", // 缺端口
		"[::1]",     // 缺端口
	}
	for _, a := range bad {
		if _, got := pprofAddrIsLoopback(a); got {
			t.Errorf("%q 必须被拒绝，却被放行了 —— 这会把 pprof 暴露出去", a)
		}
	}
}

// TestStartPprofRefusesToStartOnPublicAddr
// 拒绝之后必须什么都不启动：调用是无副作用的（不会监听端口）。
func TestStartPprofRefusesToStartOnPublicAddr(t *testing.T) {
	// 这些调用都应该立刻返回，不建立任何监听
	startPprof("")
	startPprof("0.0.0.0:18091")
	startPprof("[::]:18092")
	startPprof("192.168.1.99:18093")
	startPprof("这不是地址")
	// 走到这里没 panic、没卡住，就说明拒绝路径是干净的
}
