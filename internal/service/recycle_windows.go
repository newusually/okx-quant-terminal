//go:build windows

package service

// recycle_windows.go —— 回收站清理（Windows）
//
// ---------------------------------------------------------------------------
// 为什么不用 SHEmptyRecycleBin
// ---------------------------------------------------------------------------
// 直觉写法是调 shell32 的 `SHEmptyRecycleBinW`，但它在这里**根本不好用**：
//
//  1. 回收站是**按用户**隔离的（`C:\$Recycle.Bin\<SID>`），而
//     `SHEmptyRecycleBin` 清的是「调用进程当前用户」的那一份。
//  2. `okxweb.exe` 作为 Windows 服务跑在 **Session 0 / LocalSystem**，
//     它根本没有交互式 shell —— SHEmptyRecycleBin 要么返回 E_UNEXPECTED，
//     要么「成功」地清空了 SYSTEM 自己那份空回收站，而用户的垃圾一个没动。
//
// 所以这里直接对 `C:\$Recycle.Bin` 做目录级清理：服务有管理员权限，
// 能删掉所有用户的回收站内容，行为可控、结果可核对。
//
// ---------------------------------------------------------------------------
// 安全边界（刻意不做的事）
// ---------------------------------------------------------------------------
//   · 只处理 `C:\$Recycle.Bin` 下的**一级 SID 目录**（形如 S-1-5-21-…），
//     不跟着任何符号链接 / 联接点走（避免被重定向到别处去删东西）。
//   · 只删文件；文件删完后再尝试删空目录，非空目录留着不硬来。
//   · 遇到权限拒绝（别的用户的回收站）就跳过并计数，绝不中断整轮。
//   · dryRun=true 时只统计，一个字节都不删。

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"finally-main/internal/logx"
)

// RecycleResult 一次回收站清理的结果
type RecycleResult struct {
	Roots   []string `json:"roots"`   // 处理了哪几个 SID 目录
	Items   int64    `json:"items"`   // 文件数
	Bytes   int64    `json:"bytes"`   // 体积
	Dirs    int      `json:"dirs"`    // 顺带删掉的空目录数
	Skipped int      `json:"skipped"` // 权限不足 / 占用而跳过的条数
	DryRun  bool     `json:"dryRun"`
	Err     string   `json:"err,omitempty"`
}

// RecycleBinRoot 回收站根目录
const RecycleBinRoot = `C:\$Recycle.Bin`

// isSIDDir 判断是不是形如 S-1-5-21-…-500 的 SID 目录。
// 回收站根下除了 SID 目录还有一些系统文件，只认 SID 目录能把误删范围压到最小。
func isSIDDir(name string) bool {
	if len(name) < 5 {
		return false
	}
	if !strings.HasPrefix(strings.ToUpper(name), "S-1-") {
		return false
	}
	for _, c := range name[4:] {
		if (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// PurgeRecycleBin 清空回收站。
//
//	dryRun=true  → 只统计条目数/体积，不删
//	dryRun=false → 真删
//
// 无论哪种模式都返回结果，调用方负责写日志。
func PurgeRecycleBin(dryRun bool) *RecycleResult {
	res := &RecycleResult{DryRun: dryRun}

	ents, err := os.ReadDir(RecycleBinRoot)
	if err != nil {
		// 回收站根目录不存在（被组策略关掉）不是错误，直接返回空结果
		if os.IsNotExist(err) {
			return res
		}
		res.Err = err.Error()
		return res
	}

	for _, e := range ents {
		if !e.IsDir() || !isSIDDir(e.Name()) {
			continue
		}
		sidDir := filepath.Join(RecycleBinRoot, e.Name())
		res.Roots = append(res.Roots, sidDir)

		// 第一遍：删文件
		_ = filepath.WalkDir(sidDir, func(p string, de fs.DirEntry, werr error) error {
			if werr != nil || de == nil {
				res.Skipped++
				return nil // 别的用户的目录读不进去 → 跳过，不中断
			}
			if de.IsDir() {
				return nil
			}
			info, ierr := de.Info()
			if ierr != nil {
				res.Skipped++
				return nil
			}
			res.Items++
			res.Bytes += info.Size()
			if dryRun {
				return nil
			}
			if rerr := os.Remove(p); rerr != nil {
				res.Skipped++
			}
			return nil
		})

		if dryRun {
			continue
		}

		// 第二遍：自下而上删空目录（只删空的，非空的留着）
		var dirs []string
		_ = filepath.WalkDir(sidDir, func(p string, de fs.DirEntry, werr error) error {
			if werr == nil && de != nil && de.IsDir() && p != sidDir {
				dirs = append(dirs, p)
			}
			return nil
		})
		for i := len(dirs) - 1; i >= 0; i-- {
			if os.Remove(dirs[i]) == nil {
				res.Dirs++
			}
		}
	}

	if !dryRun && res.Items > 0 {
		logx.Logf("INFO", "[RECYCLE] 回收站已清理：%d 个文件 / %.1f MB（%d 个目录，跳过 %d 项）",
			res.Items, float64(res.Bytes)/1048576, res.Dirs, res.Skipped)
	}
	return res
}
