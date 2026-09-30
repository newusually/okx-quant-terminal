//go:build windows

package perf

// cpu_windows.go —— 读当前进程的 CPU 时间（纯标准库，不引入 x/sys 依赖）
//
// 为什么不用 wmic / PowerShell / typeperf：
//   这台机器上它们要么在程序黑名单里，要么沙箱直接拒绝。
//   自测反而最可靠 —— 进程自己知道烧了多少 CPU，而且能长期挂进监控。
//
// GetProcessTimes 返回的是 FILETIME（100 纳秒为单位）：
//   kernel 时间 = 内核态（系统调用、IO 等待被算进内核态的部分）
//   user 时间   = 用户态（纯计算）
// 两者相加就是「这个进程一共吃了多少 CPU 秒」。

import (
	"syscall"
	"unsafe"
)

var (
	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	procGetProcessTimes   = kernel32.NewProc("GetProcessTimes")
	procGetCurrentProcess = kernel32.NewProc("GetCurrentProcess")
)

// filetime 对应 Win32 FILETIME（两个 32 位拼成 64 位）
type filetime struct {
	low  uint32
	high uint32
}

// seconds 转成秒（FILETIME 单位是 100ns）
func (f filetime) seconds() float64 {
	v := uint64(f.high)<<32 | uint64(f.low)
	return float64(v) / 1e7
}

// processCPUSeconds 当前进程累计消耗的 CPU 秒数（user + kernel）。
//
// 出错时返回 0 —— 采样失败不该影响业务。
func processCPUSeconds() float64 {
	h, _, _ := procGetCurrentProcess.Call()
	if h == 0 {
		return 0
	}
	var creation, exit, kern, user filetime
	r, _, _ := procGetProcessTimes.Call(
		h,
		uintptr(unsafe.Pointer(&creation)),
		uintptr(unsafe.Pointer(&exit)),
		uintptr(unsafe.Pointer(&kern)),
		uintptr(unsafe.Pointer(&user)),
	)
	if r == 0 {
		return 0
	}
	return kern.seconds() + user.seconds()
}

// cpuSupported 本平台能否自测 CPU（false 时采样器只报 goroutine / 内存）
const cpuSupported = true
