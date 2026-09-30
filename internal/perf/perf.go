package perf

// perf.go —— 进程内性能计量：CPU 占用、热点计数、耗时统计
//
// ---------------------------------------------------------------------------
// 为什么需要它
// ---------------------------------------------------------------------------
// 用户反馈「CPU 占用太高」。但在这台机器上：
//   · taskmgr 看不到历史曲线，人工盯只能看个瞬时值
//   · wmic / PowerShell / typeperf 全在程序黑名单里，脚本读不到进程 CPU
//   · MySQL 只能告诉你 SQL 慢，告诉不了你「是哪个循环在烧 CPU」
//
// 所以改成进程自测：Go 自己读 GetProcessTimes 拿 CPU 秒数，
// 再配一套「热区打点」，把 CPU 精确摊到具体组件上。
//
// 有了它，「降低 10 倍」才有可验证的基线 —— 优化前 12%、优化后 1.2%，
// 这两个数字都必须是同一个仪表测出来的。
//
// ---------------------------------------------------------------------------
// 三层用法
// ---------------------------------------------------------------------------
//  1. 计数器   perf.Count("kline.upsert.rows", n)      —— 某件事发生了多少次
//  2. 计时器   defer perf.Track("signal.recalc")()      —— 某段代码花了多少时间
//  3. 采样器   perf.Start(ctx, 30*time.Second, logFn)   —— 定期把上面两类 + CPU 打日志
//
// 全部是原子操作，热路径上零分配、无锁竞争。

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// 计数器
// ---------------------------------------------------------------------------

type counter struct {
	val  atomic.Int64
	help string
}

var (
	cntMu      sync.RWMutex
	counters   = map[string]*counter{}
	countersAt = map[string]int64{} // 上次采样时的值，用来算增量
)

// Register 声明一个计数器（带说明，日志里会打出来）。
// 不声明也能用，只是日志里没有中文说明。
func Register(name, help string) {
	cntMu.Lock()
	if c, ok := counters[name]; ok {
		c.help = help
	} else {
		counters[name] = &counter{help: help}
	}
	cntMu.Unlock()
}

// Count 计数 +n。热路径专用：一次 RWMutex 读锁 + 一次原子加。
func Count(name string, n int64) {
	if n == 0 {
		return
	}
	cntMu.RLock()
	c := counters[name]
	cntMu.RUnlock()
	if c == nil {
		cntMu.Lock()
		if c = counters[name]; c == nil {
			c = &counter{}
			counters[name] = c
		}
		cntMu.Unlock()
	}
	c.val.Add(n)
}

// Count1 计数 +1 的简写（最常用）
func Count1(name string) { Count(name, 1) }

// ---------------------------------------------------------------------------
// 计时器
// ---------------------------------------------------------------------------

type timerStat struct {
	calls  atomic.Int64
	total  atomic.Int64 // 累计纳秒
	max    atomic.Int64 // 单次最大纳秒
	help   string
}

var (
	tmrMu    sync.RWMutex
	timers   = map[string]*timerStat{}
	timesAt  = map[string]int64{}
)

// RegisterTimer 声明一个计时器
func RegisterTimer(name, help string) {
	tmrMu.Lock()
	if t, ok := timers[name]; ok {
		t.help = help
	} else {
		timers[name] = &timerStat{help: help}
	}
	tmrMu.Unlock()
}

func timerOf(name string) *timerStat {
	tmrMu.RLock()
	t := timers[name]
	tmrMu.RUnlock()
	if t != nil {
		return t
	}
	tmrMu.Lock()
	defer tmrMu.Unlock()
	if t = timers[name]; t == nil {
		t = &timerStat{}
		timers[name] = t
	}
	return t
}

// Track 开始计时，返回一个「结束计时」的函数。
//
// 用法（注意是 defer 两次调用的形式）：
//
//	defer perf.Track("signal.recalc")()
//
// 写成 defer perf.Track(...) 会立刻执行结束函数，等于没计时 —— 这是 Go 的
// 经典坑，所以这里刻意让 Track 返回值必须是「函数」而不是直接计入。
func Track(name string) func() {
	start := time.Now()
	t := timerOf(name)
	return func() {
		d := time.Since(start).Nanoseconds()
		t.calls.Add(1)
		t.total.Add(d)
		for {
			old := t.max.Load()
			if d <= old || t.max.CompareAndSwap(old, d) {
				break
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 采样器状态
// ---------------------------------------------------------------------------

var (
	sampleMu     sync.RWMutex
	lastCPUAt    time.Time
	lastCPUSec   float64
	instCPUPct   float64   // 最近一次采样区间的平均占用（= 占单核百分比）
	peakCPUPct   float64   // 启动至今峰值
	cpuHist      []float64 // 最近若干次采样，用于算均值
	cpuHistMax   = 20
	startedAt    = time.Now()
)

// SampleOnce 立刻采一次 CPU 并更新滑动窗口。
//
// 返回「占单核百分比」：2 核机器上跑满 == 200%。
// 之所以用「占单核」而不是「占整机」，是因为进程 CPU 本身就是按核累加的
// （GetProcessTimes 在多核上跑满 2 核就是 2 秒/秒），除以核数会把
// 「一个核跑满」显示成 50%，看日志的人会误判。
func SampleOnce() float64 {
	if !cpuSupported {
		return 0
	}
	now := time.Now()
	sec := processCPUSeconds()

	sampleMu.Lock()
	defer sampleMu.Unlock()
	if lastCPUAt.IsZero() {
		lastCPUAt, lastCPUSec = now, sec
		return 0
	}
	wall := now.Sub(lastCPUAt).Seconds()
	if wall <= 0 {
		return instCPUPct
	}
	pct := (sec - lastCPUSec) / wall * 100
	lastCPUAt, lastCPUSec = now, sec
	instCPUPct = pct
	if pct > peakCPUPct {
		peakCPUPct = pct
	}
	cpuHist = append(cpuHist, pct)
	if len(cpuHist) > cpuHistMax {
		cpuHist = cpuHist[len(cpuHist)-cpuHistMax:]
	}
	return pct
}

// CPUPercent 最近一次采样的 CPU 占用（占单核百分比，2 核跑满 = 200）
func CPUPercent() float64 {
	sampleMu.RLock()
	defer sampleMu.RUnlock()
	return instCPUPct
}

// CPUAvgPercent 最近 cpuHistMax 次采样的平均值（比瞬时值稳得多）
func CPUAvgPercent() float64 {
	sampleMu.RLock()
	defer sampleMu.RUnlock()
	if len(cpuHist) == 0 {
		return 0
	}
	var s float64
	for _, v := range cpuHist {
		s += v
	}
	return s / float64(len(cpuHist))
}

// CPUTotalSeconds 进程启动至今累计消耗的 CPU 秒
func CPUTotalSeconds() float64 {
	if !cpuSupported {
		return 0
	}
	return processCPUSeconds()
}

// ---------------------------------------------------------------------------
// Snapshot / 日志
// ---------------------------------------------------------------------------

// Snapshot 当前性能快照（给 /api/perf 和日志用）
func Snapshot() map[string]any {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	sampleMu.RLock()
	peak, avg := peakCPUPct, 0.0
	if len(cpuHist) > 0 {
		var s float64
		for _, v := range cpuHist {
			s += v
		}
		avg = s / float64(len(cpuHist))
	}
	cur := instCPUPct
	sampleMu.RUnlock()

	out := map[string]any{
		"cpuSupported":  cpuSupported,
		"cpuPercent":    round2(cur),      // 最近一次，占单核
		"cpuAvgPercent": round2(avg),
		"cpuPeakPercent": round2(peak),
		"cpuTotalSec":   round2(CPUTotalSeconds()),
		"numCPU":        runtime.NumCPU(),
		"goroutines":    runtime.NumGoroutine(),
		"heapMB":        round2(float64(ms.HeapAlloc) / 1048576),
		"sysMB":         round2(float64(ms.Sys) / 1048576),
		"numGC":         ms.NumGC,
		"uptimeSec":     int(time.Since(startedAt).Seconds()),
	}

	// ---- 计数器（附上自上次快照以来的增量）----
	cntMu.Lock()
	cs := make([]map[string]any, 0, len(counters))
	for name, c := range counters {
		v := c.val.Load()
		delta := v - countersAt[name]
		countersAt[name] = v
		cs = append(cs, map[string]any{
			"name": name, "help": c.help, "total": v, "delta": delta,
		})
	}
	cntMu.Unlock()
	sort.Slice(cs, func(i, j int) bool {
		return cs[i]["total"].(int64) > cs[j]["total"].(int64)
	})
	out["counters"] = cs

	// ---- 计时器 ----
	tmrMu.Lock()
	ts := make([]map[string]any, 0, len(timers))
	for name, t := range timers {
		calls := t.calls.Load()
		total := t.total.Load()
		avgMs := 0.0
		if calls > 0 {
			avgMs = float64(total) / float64(calls) / 1e6
		}
		ts = append(ts, map[string]any{
			"name": name, "help": t.help, "calls": calls,
			"totalMs":     round2(float64(total) / 1e6),
			"avgMs":       round2(avgMs),
			"maxMs":       round2(float64(t.max.Load()) / 1e6),
			"deltaCalls":  calls - timesAt[name],
		})
		timesAt[name] = calls
	}
	tmrMu.Unlock()
	sort.Slice(ts, func(i, j int) bool {
		return ts[i]["totalMs"].(float64) > ts[j]["totalMs"].(float64)
	})
	out["timers"] = ts
	return out
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// Start 启动后台采样器：每秒采一次 CPU，每 interval 打一行汇总日志。
//
// 每秒采一次是为了让「瞬时值」有参考意义（间隔越长越像平均值）；
// 打日志的周期由调用方定（默认 30 秒，日志不至于被刷爆）。
func Start(ctx context.Context, interval time.Duration, logFn func(string, ...any)) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if logFn == nil {
		logFn = func(string, ...any) {}
	}

	// 每秒一次 CPU 采样
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				SampleOnce()
			}
		}
	}()

	// 每 interval 打一行摘要
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		// 先采一次建立基线（不然第一次增量会是「启动至今」的巨大值）
		time.Sleep(2 * time.Second)
		SampleOnce()
		_ = Snapshot() // 清掉计数器基线
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				logFn("[PERF] %s", HumanLine(interval))
			}
		}
	}()
}

// HumanLine 生成一行人类可读的汇总（日志用），delta 是本次统计窗口
func HumanLine(window time.Duration) string {
	snap := Snapshot()
	var sb strings.Builder
	fmt.Fprintf(&sb, "CPU 本次 %.1f%% / 均值 %.1f%% / 峰值 %.1f%%（占单核，共 %d 核）· G %d · 堆 %.0fMB · 累计 CPU %.0fs",
		snap["cpuPercent"], snap["cpuAvgPercent"], snap["cpuPeakPercent"],
		snap["numCPU"], snap["goroutines"], snap["heapMB"], snap["cpuTotalSec"])

	// 只打「本窗口内真的跑过」的计数器，按次数排序
	type kv struct {
		name  string
		delta int64
	}
	cs := snap["counters"].([]map[string]any)
	arr := make([]kv, 0, len(cs))
	for _, c := range cs {
		if d := c["delta"].(int64); d > 0 {
			arr = append(arr, kv{c["name"].(string), d})
		}
	}
	if len(arr) > 0 {
		sort.Slice(arr, func(i, j int) bool { return arr[i].delta > arr[j].delta })
		parts := make([]string, 0, len(arr))
		for i, x := range arr {
			if i >= 8 {
				parts = append(parts, fmt.Sprintf("…共 %d 项", len(arr)))
				break
			}
			parts = append(parts, fmt.Sprintf("%s=%d", x.name, x.delta))
		}
		sb.WriteString(" · 计数 " + strings.Join(parts, " "))
	}

	// 只打「本窗口内调用过」的计时器，按本窗口总耗时排序
	type tv struct {
		name  string
		total float64
	}
	tm := snap["timers"].([]map[string]any)
	ar2 := make([]tv, 0, len(tm))
	for _, t := range tm {
		d := t["deltaCalls"].(int64)
		if d <= 0 {
			continue
		}
		// 本窗口总耗时 = 平均单次 × 本窗口调用次数
		ar2 = append(ar2, tv{t["name"].(string), t["avgMs"].(float64) * float64(d)})
	}
	if len(ar2) > 0 {
		sort.Slice(ar2, func(i, j int) bool { return ar2[i].total > ar2[j].total })
		parts := make([]string, 0, len(ar2))
		for i, x := range ar2 {
			if i >= 6 {
				parts = append(parts, fmt.Sprintf("…共 %d 项", len(ar2)))
				break
			}
			parts = append(parts, fmt.Sprintf("%s=%.0fms", x.name, x.total))
		}
		sb.WriteString(" · 耗时 " + strings.Join(parts, " "))
	}
	return sb.String()
}
