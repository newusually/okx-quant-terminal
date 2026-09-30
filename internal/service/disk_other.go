//go:build !windows

package service

// disk_other.go —— 非 Windows 平台的占位实现（本项目只跑 Windows，
// 这里只是保证 go vet / go build 在其它平台不会挂）

// FreeDiskMB 非 Windows 平台恒返回 0（= 不做磁盘守卫）
func FreeDiskMB(path string) uint64 { return 0 }
