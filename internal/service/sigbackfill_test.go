package service

// sigbackfill_test.go —— 验证「窗口截断」与「全段计算」的信号结果等价。
//
// BackfillSignalsFor 为了把 1m 全量回算从小时级压到秒级，给 ComputeSignal
// 只喂最近 sigWindow 根。8 因子最长的窗口是 sma200，理论上 700 根窗口
// 与全段完全等价 —— 这个测试用随机 K 线逐根对比 mask，保证口径没漂。

import (
	"math/rand"
	"testing"
)

func genRandomCandles(n int, seed int64) []Candle {
	r := rand.New(rand.NewSource(seed))
	out := make([]Candle, n)
	px := 100.0
	for i := 0; i < n; i++ {
		// 带一点趋势和波动聚集，比纯白噪声更接近真实行情
		px *= 1 + (r.Float64()-0.5)*0.02
		o := px
		c := px * (1 + (r.Float64()-0.5)*0.01)
		h := o * (1 + r.Float64()*0.01)
		l := o * (1 - r.Float64()*0.01)
		if c > h {
			h = c
		}
		if c < l {
			l = c
		}
		out[i] = Candle{Ts: int64(1700000000000 + i*60000), O: o, H: h, L: l, C: c, V: r.Float64() * 1000}
		px = c
	}
	return out
}

func TestSigBackfillWindowEquivalence(t *testing.T) {
	const n = 5000 // 模拟 1m 3.5 天
	candles := genRandomCandles(n, 42)

	diff := 0
	compared := 0
	for i := sigWarmup; i < n; i++ {
		// 全段计算（旧口径）
		full := ComputeSignal("TEST-USDT-SWAP", "1m", candles, i)
		// 窗口截断（新口径，与 BackfillSignalsFor 完全一致）
		lo := i + 1 - sigWindow
		if lo < 0 {
			lo = 0
		}
		win := candles[lo : i+1]
		wind := ComputeSignal("TEST-USDT-SWAP", "1m", win, len(win)-1)

		if full == nil || wind == nil {
			t.Fatalf("i=%d 返回 nil", i)
		}
		// Ready 之前的位置（指标 NaN）两口径都算不出信号，跳过对比
		if !full.Ready && !wind.Ready {
			continue
		}
		compared++
		if full.Mask != wind.Mask || full.Score != wind.Score {
			diff++
			if diff <= 3 {
				t.Logf("i=%d 不一致: full mask=%d score=%d / window mask=%d score=%d",
					i, full.Mask, full.Score, wind.Mask, wind.Score)
			}
		}
	}
	if compared == 0 {
		t.Fatal("没有可对比的点（全是 NaN？）")
	}
	if diff > 0 {
		t.Fatalf("%d/%d 个点 mask 不一致（窗口截断不等价）", diff, compared)
	}
	t.Logf("等价性通过：%d 个点全一致（%d 根随机K线）", compared, n)
}
