//go:build !windows

package service

// recycle_other.go —— 非 Windows 平台的空实现。
//
// 本项目只在 Windows 上跑（Apache + Windows 服务 + VC++ 运行库都是 Win 专属），
// 这个文件存在的意义只是让 `go vet ./...` 在别的平台上不至于编译失败。

// RecycleResult 一次回收站清理的结果
type RecycleResult struct {
	Roots   []string `json:"roots"`
	Items   int64    `json:"items"`
	Bytes   int64    `json:"bytes"`
	Dirs    int      `json:"dirs"`
	Skipped int      `json:"skipped"`
	DryRun  bool     `json:"dryRun"`
	Err     string   `json:"err,omitempty"`
}

// RecycleBinRoot 回收站根目录（非 Windows 无意义）
const RecycleBinRoot = ""

// PurgeRecycleBin 非 Windows 平台直接返回空结果。
func PurgeRecycleBin(dryRun bool) *RecycleResult {
	return &RecycleResult{DryRun: dryRun, Err: "回收站清理仅支持 Windows"}
}
