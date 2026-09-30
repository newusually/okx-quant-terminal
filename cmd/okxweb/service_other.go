//go:build !windows

package main

// service_other.go —— 非 Windows 平台的占位实现
//
// 服务化只对 Windows 有意义（本项目的部署目标就是 Windows Server + Windows 10）。
// 放这个文件是为了让 `go build ./...` 在别的平台上不会因为缺符号而失败。

import (
	"context"
	"fmt"
	"runtime"
)

func runningAsService() bool { return false }

func runAsService(run func(ctx context.Context) error) error { return run(context.Background()) }

func installService() error {
	return fmt.Errorf("Windows 服务只支持 Windows（当前 %s）", runtime.GOOS)
}

func uninstallService() error {
	return fmt.Errorf("Windows 服务只支持 Windows（当前 %s）", runtime.GOOS)
}
