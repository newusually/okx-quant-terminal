//go:build !windows

package perf

// cpu_other.go —— 非 Windows 平台的占位实现。
//
// 本项目实际只跑在 Windows 上（Windows 服务 + MySQL + Apache）。
// 留着这个文件是为了 `go vet ./...` / 交叉编译 / 单测不至于因为缺符号而挂。

func processCPUSeconds() float64 { return 0 }

const cpuSupported = false
