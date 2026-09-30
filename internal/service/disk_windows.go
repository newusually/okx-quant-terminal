//go:build windows

package service

// disk_windows.go —— 查磁盘剩余空间（纯 syscall，不引第三方依赖、不用 CGO）

import (
	"syscall"
	"unsafe"
)

var (
	kernel32            = syscall.NewLazyDLL("kernel32.dll")
	procGetDiskFreeSpace = kernel32.NewProc("GetDiskFreeSpaceExW")
)

// FreeDiskMB 返回 path 所在卷的剩余空间（MB）。出错返回 0。
func FreeDiskMB(path string) uint64 {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0
	}
	var freeToCaller, total, totalFree uint64
	r, _, _ := procGetDiskFreeSpace.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&freeToCaller)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if r == 0 {
		return 0
	}
	return freeToCaller / (1024 * 1024)
}
