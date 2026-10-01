package service

import (
	"math"
	"math/rand"
	"testing"
)

// genBenchCandles 造一段确定性的随机游走 K 线，用来给指标流水线做基准。
// 用固定种子，保证每次跑出来的数字可比。
func genBenchCandles(n int) []Candle {
	out := make([]Candle, n)
	p := 100.0
	r := rand.New(rand.NewSource(20261001))
	for i := range out {
		p *= 1 + (r.Float64()-0.5)*0.006
		hi := p * (1 + r.Float64()*0.003)
		lo := p * (1 - r.Float64()*0.003)
		out[i] = Candle{
			Ts:      int64(i) * 900_000,
			O:       p,
			H:       hi,
			L:       lo,
			C:       p * (0.999 + r.Float64()*0.002),
			V:       1000 + r.Float64()*500,
			Confirm: true,
		}
	}
	return out
}

var benchSinkF float64
var benchSinkI int

// BenchmarkIndicatorPipeline 单独量「指标流水线」本身的成本：
// 一次算完全部指标（TR/ATR/RSI/SMA/EMA/BOLL/MACD/TD9）。
// 这是唯一有可能从 C++ 拿到收益的那部分代码。
func BenchmarkIndicatorPipeline(b *testing.B) {
	for _, n := range []int{1000, 1500, 3000} {
		c := genBenchCandles(n)
		h := make([]float64, n)
		l := make([]float64, n)
		cl := make([]float64, n)
		for i := range c {
			h[i], l[i], cl[i] = c[i].H, c[i].L, c[i].C
		}
		b.Run(benchName(n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				tr := trueRange(h, l, cl)
				atr := atrPercent(h, l, cl, 14)
				rsi := rsiWilder(cl, 14)
				s := sma(cl, 7)
				m := sma(cl, 25)
				g := sma(cl, 99)
				e := ema(cl, 30)
				lo, up, mid := boll(cl, 20, 2)
				mh := macdHist(cl, 12, 26, 9)
				td := tdSetup(cl)
				benchSinkF = tr[n-1] + atr[n-1] + rsi[n-1] + s[n-1] + m[n-1] + g[n-1] + e[n-1] + lo[n-1] + up[n-1] + mid[n-1] + mh[n-1]
				benchSinkI = td[n-1]
			}
		})
	}
}

// BenchmarkComputeSignalAll 量「整段 K 线逐根判信号」的成本。
// 真实场景：回填/扫描时对每个合约的整段 K 线跑一遍。
func BenchmarkComputeSignalAll(b *testing.B) {
	for _, n := range []int{1000, 1500, 3000} {
		c := genBenchCandles(n)
		b.Run(benchName(n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				hits := 0
				for idx := 100; idx < n; idx++ {
					if sig := ComputeSignal("BENCH-USDT-SWAP", "15m", c, idx); sig != nil {
						hits++
					}
				}
				benchSinkI = hits
			}
		})
	}
}

// BenchmarkComputeSignalTail 量「只判最后一根」的成本。
// 真实场景：每一轮 tick 对每个合约只看最新那根 K 线。
func BenchmarkComputeSignalTail(b *testing.B) {
	const n = 1500
	c := genBenchCandles(n)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if sig := ComputeSignal("BENCH-USDT-SWAP", "15m", c, n-1); sig != nil {
			benchSinkI = sig.Score
		}
	}
}

// BenchmarkUniverseFullScan 量「全市场扫一遍」的成本：
// 479 个合约 × 1500 根 15m K 线，每根都判信号。
// 这是本项目 CPU 侧最重的活，也是「C++ 能不能救」这个问题的上限所在。
func BenchmarkUniverseFullScan(b *testing.B) {
	const insts = 479
	const bars = 1500
	all := make([][]Candle, insts)
	for i := range all {
		all[i] = genBenchCandles(bars)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		total := 0
		for _, c := range all {
			for idx := 100; idx < bars; idx++ {
				if sig := ComputeSignal("BENCH-USDT-SWAP", "15m", c, idx); sig != nil {
					total++
				}
			}
		}
		benchSinkI = total
	}
}

// BenchmarkUniverseTailScan 量「全市场只看最新一根」的成本 —— 这才是每轮 tick 的真活。
func BenchmarkUniverseTailScan(b *testing.B) {
	const insts = 479
	const bars = 1500
	all := make([][]Candle, insts)
	for i := range all {
		all[i] = genBenchCandles(bars)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		total := 0
		for _, c := range all {
			if sig := ComputeSignal("BENCH-USDT-SWAP", "15m", c, bars-1); sig != nil {
				total += sig.Score
			}
		}
		benchSinkI = total
	}
}

// TestBenchMathSanity 防止编译器把基准里的计算优化掉，也顺便验证指标不是 NaN。
func TestBenchMathSanity(t *testing.T) {
	c := genBenchCandles(1500)
	h := make([]float64, len(c))
	l := make([]float64, len(c))
	cl := make([]float64, len(c))
	for i := range c {
		h[i], l[i], cl[i] = c[i].H, c[i].L, c[i].C
	}
	rsi := rsiWilder(cl, 14)
	if math.IsNaN(rsi[len(rsi)-1]) {
		t.Fatal("rsi 末值为 NaN")
	}
	if sig := ComputeSignal("BENCH-USDT-SWAP", "15m", c, len(c)-1); sig != nil && sig.Score < 0 {
		t.Fatalf("score 为负：%d", sig.Score)
	}
}

func benchName(n int) string {
	switch n {
	case 1000:
		return "bars1000"
	case 1500:
		return "bars1500"
	case 3000:
		return "bars3000"
	}
	return "bars"
}
