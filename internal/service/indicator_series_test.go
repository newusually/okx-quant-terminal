package service

import (
	"math"
	"math/rand"
	"testing"
)

// sameF 比较两个 float64，把「都是 NaN」视为相等（否则 NaN != NaN 会假报错）
func sameF(a, b float64) bool {
	if math.IsNaN(a) && math.IsNaN(b) {
		return true
	}
	return a == b
}

// sameSignal 逐字段比特比对两条信号
func sameSignal(t *testing.T, tag string, got, want *Signal) {
	t.Helper()
	if got == nil || want == nil {
		if got != want {
			t.Fatalf("%s: 一个 nil 一个不是（got=%v want=%v）", tag, got, want)
		}
		return
	}
	if got.Ts != want.Ts {
		t.Fatalf("%s: Ts %d != %d", tag, got.Ts, want.Ts)
	}
	for _, f := range []struct {
		name string
		g, w float64
	}{
		{"Close", got.Close, want.Close},
		{"Open", got.Open, want.Open},
		{"High", got.High, want.High},
		{"Low", got.Low, want.Low},
		{"Vol", got.Vol, want.Vol},
		{"Pot", got.Pot, want.Pot},
		{"Fri", got.Fri, want.Fri},
		{"Kin", got.Kin, want.Kin},
		{"Rsi", got.Rsi, want.Rsi},
		{"BollLo", got.BollLo, want.BollLo},
		{"BollUp", got.BollUp, want.BollUp},
		{"MacdH", got.MacdH, want.MacdH},
	} {
		if !sameF(f.g, f.w) {
			t.Fatalf("%s: %s 不一致 got=%v want=%v", tag, f.name, f.g, f.w)
		}
	}
	if got.Td != want.Td {
		t.Fatalf("%s: Td %d != %d", tag, got.Td, want.Td)
	}
	if got.Mask != want.Mask {
		t.Fatalf("%s: Mask %d != %d", tag, got.Mask, want.Mask)
	}
	if got.Score != want.Score {
		t.Fatalf("%s: Score %d != %d", tag, got.Score, want.Score)
	}
	if got.HitList != want.HitList {
		t.Fatalf("%s: HitList %q != %q", tag, got.HitList, want.HitList)
	}
	if got.Ready != want.Ready {
		t.Fatalf("%s: Ready %v != %v", tag, got.Ready, want.Ready)
	}
}

// TestComputeSignalEquivalence ★ 本改造最重要的测试 ★
//
// 池化 + 批量化的实现，必须与改造前的 O(n²) 原实现在**每一根、每个字段**上一致。
// 只要有一处不一致，就说明买卖点会漂移，必须回滚。
func TestComputeSignalEquivalence(t *testing.T) {
	for _, n := range []int{1, 2, 5, 17, 120, 300, 401, 1000, 1500} {
		c := genBenchCandles(n)
		// 覆盖全部下标，外加边界外的非法下标
		idxList := []int{-1, n - 1, n, 0}
		for i := 0; i < n; i++ {
			idxList = append(idxList, i)
		}
		for _, idx := range idxList {
			got := ComputeSignal("EQ-USDT-SWAP", "15m", c, idx)
			want := computeSignalReference("EQ-USDT-SWAP", "15m", c, idx)
			sameSignal(t, "n="+itoa(n)+" idx="+itoa(idx), got, want)
		}
	}
}

// TestComputeSignalEquivalenceRealish 用更接近真实的价格形态再验一遍：
// 趋势段 + 急跌段 + 横盘段（会触发不同的分支组合）
func TestComputeSignalEquivalenceRealish(t *testing.T) {
	const n = 800
	c := make([]Candle, n)
	p := 2.5
	r := rand.New(rand.NewSource(7))
	for i := 0; i < n; i++ {
		switch {
		case i < 200: // 上涨
			p *= 1.002
		case i < 320: // 急跌
			p *= 0.994
		case i < 500: // 横盘
			p *= 1 + (r.Float64()-0.5)*0.001
		case i < 600: // 再急跌
			p *= 0.990
		default: // 反弹
			p *= 1.003
		}
		hi := p * (1 + r.Float64()*0.004)
		lo := p * (1 - r.Float64()*0.004)
		vol := 1000 + r.Float64()*800
		if i >= 318 && i < 322 {
			vol *= 5 // 制造一根放量
		}
		c[i] = Candle{Ts: int64(i) * 900_000, O: p, H: hi, L: lo, C: p * 0.9998, V: vol, Confirm: true}
	}
	for idx := 0; idx < n; idx++ {
		got := ComputeSignal("EQ2-USDT-SWAP", "15m", c, idx)
		want := computeSignalReference("EQ2-USDT-SWAP", "15m", c, idx)
		sameSignal(t, "idx="+itoa(idx), got, want)
	}
}

// TestComputeSeriesMatchesComputeSignal 序列版与单点版必须给出相同结果
func TestComputeSeriesMatchesComputeSignal(t *testing.T) {
	for _, n := range []int{50, 401, 1500} {
		c := genBenchCandles(n)
		ss := ComputeSeries("SER-USDT-SWAP", "15m", c)
		if ss.Len() != n {
			t.Fatalf("Len()=%d 期望 %d", ss.Len(), n)
		}
		for i := 0; i < n; i++ {
			sameSignal(t, "series n="+itoa(n)+" i="+itoa(i),
				ss.At(i), computeSignalReference("SER-USDT-SWAP", "15m", c, i))
		}
		ss.Release()
	}
}

// TestScratchPoolNoStaleData ★ 池化最危险的坑 ★
//
// 缓冲是复用的，长数组会换给短数组用。如果某个指标函数没有把全部 n 个位置写满，
// 就会读到上一轮留下的旧值 —— 这种 bug 只在「先长后短」时出现，
// 单次调用永远测不出来。这里特意用「长 → 短 → 长」交替，逼它暴露。
func TestScratchPoolNoStaleData(t *testing.T) {
	long := genBenchCandles(1500)
	short := genBenchCandles(37)
	tiny := genBenchCandles(3)
	mid := genBenchCandles(401)

	// 先把池子里的缓冲喂大
	for i := 0; i < 3; i++ {
		ComputeSignal("WARM", "15m", long, 1499)
	}
	// 再用短序列，逐个下标对齐参照实现
	for _, cs := range [][]Candle{short, tiny, mid, short, tiny} {
		for i := 0; i < len(cs); i++ {
			got := ComputeSignal("STALE", "15m", cs, i)
			want := computeSignalReference("STALE", "15m", cs, i)
			sameSignal(t, "len="+itoa(len(cs))+" i="+itoa(i), got, want)
		}
	}
	// 交互着来一遍
	for r := 0; r < 5; r++ {
		for _, cs := range [][]Candle{long, tiny, mid, short, long} {
			idx := len(cs) - 1
			got := ComputeSignal("MIX", "15m", cs, idx)
			want := computeSignalReference("MIX", "15m", cs, idx)
			sameSignal(t, "mix len="+itoa(len(cs)), got, want)
		}
	}
}

// TestComputeSeriesRelease 释放后不得 panic，且 At 返回 nil；Release 可重复调用
func TestComputeSeriesRelease(t *testing.T) {
	c := genBenchCandles(300)
	ss := ComputeSeries("REL", "15m", c)
	if ss.At(299) == nil {
		t.Fatal("Release 前 At 不该是 nil")
	}
	ss.Release()
	if ss.At(299) != nil {
		t.Fatal("Release 后 At 应返回 nil")
	}
	ss.Release() // 二次释放不能崩
	var nilSS *SignalSeries
	if nilSS.At(0) != nil || nilSS.Len() != 0 {
		t.Fatal("nil 接收者应安全返回")
	}
	nilSS.Release()
}

// TestComputeSignalEmptyAndBad 空输入 / 非法下标的行为必须与改造前一致
func TestComputeSignalEmptyAndBad(t *testing.T) {
	got := ComputeSignal("E", "15m", nil, 0)
	if got == nil || got.Ready || got.Mask != 0 || got.Score != 0 {
		t.Fatalf("空输入应返回未就绪的空信号，实得 %+v", got)
	}
	if got.InstID != "E" || got.Bar != "15m" {
		t.Fatalf("InstID/Bar 应被填上，实得 %q/%q", got.InstID, got.Bar)
	}
	c := genBenchCandles(10)
	for _, idx := range []int{-5, -1, 10, 999} {
		got := ComputeSignal("E", "15m", c, idx)
		want := computeSignalReference("E", "15m", c, idx)
		sameSignal(t, "bad idx="+itoa(idx), got, want)
	}
}

// TestPoolActuallyReuses 池化必须真的在复用（否则等于白改）
func TestPoolActuallyReuses(t *testing.T) {
	c := genBenchCandles(1500)
	// 稳态：反复调用应不再产生新的大分配
	f := func() {
		for i := 0; i < 50; i++ {
			_ = ComputeSignal("P", "15m", c, 1499)
		}
	}
	avg1 := testing.AllocsPerRun(3, f)
	avg2 := testing.AllocsPerRun(3, f)
	if avg2 > avg1*1.5+20 {
		t.Fatalf("稳态分配数没收敛：首轮 %.1f 次，后续 %.1f 次（池化可能失效）", avg1, avg2)
	}
	t.Logf("每 50 次 ComputeSignal 的分配次数：首轮 %.1f，稳态 %.1f", avg1, avg2)
}

// BenchmarkComputeSeriesTail 序列版取尾部一根（生产路径的用法）
func BenchmarkComputeSeriesTail(b *testing.B) {
	const n = 1500
	c := genBenchCandles(n)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ss := ComputeSeries("B", "15m", c)
		if sig := ss.At(n - 1); sig != nil {
			benchSinkI = sig.Score
		}
		ss.Release()
	}
}

// BenchmarkComputeSeriesAll 序列版逐根取（改造前这里是 O(n²)）
func BenchmarkComputeSeriesAll(b *testing.B) {
	const n = 1500
	c := genBenchCandles(n)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ss := ComputeSeries("B", "15m", c)
		total := 0
		for idx := 100; idx < n; idx++ {
			if sig := ss.At(idx); sig != nil {
				total += sig.Score
			}
		}
		benchSinkI = total
		ss.Release()
	}
}

// itoa 用 announce.go 里已有的那个（strconv.Itoa 的薄封装），不重复定义。
