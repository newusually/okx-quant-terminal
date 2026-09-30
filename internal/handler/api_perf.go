package handler

// api_perf.go —— 进程性能自检接口
//
// /api/perf 返回本进程（不是 MySQL）的：
//   · CPU 占用（瞬时 / 均值 / 峰值 / 累计秒数）
//   · goroutine 数、堆内存、GC 次数
//   · 各热点的调用次数（计数器）
//   · 各热点的累计耗时 / 单次均值 / 单次峰值（计时器）
//
// 存在的意义：这台机器上 wmic / PowerShell 都被黑名单拦了，读不到进程 CPU；
// 而「CPU 占用太高」这种问题必须先能量化，才能谈「降低多少倍」。
// 有了这个接口，优化前后用同一个仪表测，数字才可信。

import (
	"net/http"

	"finally-main/internal/perf"
)

func (s *Server) handlePerf(w http.ResponseWriter, r *http.Request) (any, error) {
	snap := perf.Snapshot()
	snap["ok"] = true
	// 顺手带上「这块机器上有几个核」，CPU 百分比是按「占单核」口径的，
	// 不看核数容易误读（2 核跑满 = 200%）。
	snap["note"] = "cpuPercent 是占单核百分比：N 核跑满显示 N×100"
	return snap, nil
}
