package handler

import (
	"math"
	"testing"
)

// TestSmaSeriesAligned 校验均线序列与输入下标一一对齐。
//
// 回归场景：ETH 15m 现价 ~2688，但线上接口曾经返回 ma25=1361 / ma99=975，
// 原因是 kline 表里混进了价格 ~740 的合成数据 —— 公式没错，数据脏了。
// 这个测试用「干净序列」钉住公式本身的行为。
func TestSmaSeriesAligned(t *testing.T) {
	x := make([]float64, 120)
	for i := range x {
		x[i] = 2600 + float64(i) // 2600 → 2719 线性上升
	}
	m25 := smaSeries(x, 25)
	if len(m25) != len(x) {
		t.Fatalf("长度应为 %d，实际 %d", len(x), len(m25))
	}
	// 前 24 个必须有值缺失（NaN）
	for i := 0; i < 24; i++ {
		if !math.IsNaN(m25[i]) {
			t.Fatalf("下标 %d 应当是 NaN（数据不足 25 根）", i)
		}
	}
	// 最后一个 = 最近 25 根的均值
	want := 0.0
	for i := len(x) - 25; i < len(x); i++ {
		want += x[i]
	}
	want /= 25
	if math.Abs(m25[len(x)-1]-want) > 1e-9 {
		t.Fatalf("MA25 末值 = %v，期望 %v", m25[len(x)-1], want)
	}
	// 量级上必须贴着价格，不能像脏数据那样掉到一半
	if m25[len(x)-1] < 2600 {
		t.Fatalf("MA25 末值 %v 明显偏离价格区间，序列/下标对不上", m25[len(x)-1])
	}
}

// TestBollSeriesAroundPrice 布林带上下轨必须夹着中轨，且都在价格附近
func TestBollSeriesAroundPrice(t *testing.T) {
	x := make([]float64, 60)
	for i := range x {
		// 2688 上下 ±5 波动
		x[i] = 2688 + 5*math.Sin(float64(i)/3)
	}
	up, mid, lo := bollSeries(x, 20, 2.0)
	i := len(x) - 1
	if math.IsNaN(mid[i]) || math.IsNaN(up[i]) || math.IsNaN(lo[i]) {
		t.Fatal("末值不应为 NaN")
	}
	if !(up[i] > mid[i] && mid[i] > lo[i]) {
		t.Fatalf("上下轨顺序不对：up=%v mid=%v lo=%v", up[i], mid[i], lo[i])
	}
	if lo[i] <= 0 {
		t.Fatalf("下轨为负（%v）—— 说明喂进去的价格序列被污染了", lo[i])
	}
	if math.Abs(mid[i]-2688) > 20 {
		t.Fatalf("中轨 %v 明显偏离 2688", mid[i])
	}
}

// TestSmaSeriesShortInput 数据不足时返回全 NaN，不能 panic
func TestSmaSeriesShortInput(t *testing.T) {
	out := smaSeries([]float64{1, 2, 3}, 25)
	if len(out) != 3 {
		t.Fatalf("长度应为 3，实际 %d", len(out))
	}
	for i, v := range out {
		if !math.IsNaN(v) {
			t.Fatalf("下标 %d 应为 NaN", i)
		}
	}
}
