package service

import (
	"math"
	"testing"
)

// 六期口径（2026-10-02）：
//
//	「Score >= 3 且 RisePct < -0.7（严格小于）」—— 触发那根必须真跌超 0.7%，
//	两个条件必须落在**同一根已收盘 K 线**上。
//
// min_bar_rise_pct 从六期起是**带符号门槛**：> 0 必须真涨、< 0 必须真跌、0 = 关闭。
// 两个方向的边界都要钉死 —— SignalQualified 是买入扫描 / 加仓判定 / 信号入库
// **三处共用的唯一判据**，一旦这里漂移，三处会一起漂，而且不报错。
func TestSignalQualified(t *testing.T) {
	mk := func(score int, ready bool, rise float64) *Signal {
		return &Signal{Score: score, Ready: ready, RisePct: rise}
	}

	const (
		th = 4    // 二十二期阈值（configs/okx_strategy.json 的 score_threshold，= 共振 > 3）
		mr = -1.0 // 二十二期涨跌幅下限（entry.min_bar_rise_pct，必须真跌超 1%）
		md = 2.0  // 二十二期跌幅上限（entry.max_bar_drop_pct，跌幅须 < 2%）
	)

	cases := []struct {
		name      string
		sig       *Signal
		threshold int
		minRise   float64
		maxDrop   float64
		want      bool
	}{
		{"A 二十二期口径：score 4 且跌 1.5%（区间内）→ 通过", mk(4, true, -1.5), th, mr, md, true},
		{"B score 3 不满足「>= 4」→ 拒绝", mk(3, true, -1.5), th, mr, md, false},
		{"C score 满分但只跌 0.9% → 拒绝（下限）", mk(8, true, -0.9), th, mr, md, false},
		{"D 跌幅恰好 -1.0（下限是严格小于）→ 拒绝", mk(5, true, -1.0), th, mr, md, false},
		{"E 跌幅 -1.0001 → 通过", mk(5, true, -1.0001), th, mr, md, true},
		{"F 跌幅恰好 -2.0（上限是严格小于）→ 拒绝", mk(5, true, -2.0), th, mr, md, false},
		{"G 跌幅 -2.5（崩盘式大跌）→ 拒绝（上限）", mk(5, true, -2.5), th, mr, md, false},
		{"H 跌幅 -1.9999 → 通过（贴着上限）", mk(5, true, -1.9999), th, mr, md, true},
		{"I 上涨的 K 线（+2%）在「必须真跌」门槛下 → 拒绝", mk(8, true, 2.0), th, mr, md, false},
		{"J 平盘（0%）在「必须真跌」门槛下 → 拒绝", mk(8, true, 0), th, mr, md, false},
		{"K minRise=0 = 关闭下限 → 只受上限约束（-1.5 过）", mk(5, true, -1.5), th, 0, md, true},
		{"L maxDrop=0 = 关闭上限 → -5% 深跌也过（老行为）", mk(5, true, -5.0), th, mr, 0, true},
		{"M 两个门槛都关 → 只看 score", mk(5, true, 3.0), th, 0, 0, true},
		{"N 暖机不足（Ready=false）→ 拒绝", mk(8, false, -5.0), th, mr, md, false},
		{"O nil 信号 → 拒绝", nil, th, mr, md, false},
		{"P 阈值 0（配置坏掉）→ 拒绝，而不是放宽", mk(8, true, -5.0), 0, mr, md, false},
		{"Q RisePct 是 NaN → 拒绝（保守）", mk(8, true, math.NaN()), th, mr, md, false},
		{"R 正门槛（旧语义「必须真涨」）仍可用：+2% 过", mk(5, true, 2.0), th, 0.5, 0, true},
		{"S 正门槛下恰好压线 0.5 → 拒绝（严格大于）", mk(5, true, 0.5), th, 0.5, 0, false},
	}

	for _, c := range cases {
		if got := SignalQualified(c.sig, c.threshold, c.minRise, c.maxDrop); got != c.want {
			t.Errorf("%s：得到 %v，期望 %v", c.name, got, c.want)
		}
	}
}

// 阈值语义必须是**非严格**的 `Score >= threshold` —— 五期口径原话就是「Score >= 3」。
//
// 这条把「>=」钉死：若哪天有人把它改成严格大于（`Score > threshold`），
// 门槛会被悄悄收紧一档（score 刚好等于阈值的那批全部消失），这里立刻红。
// （对照：三期为了表达「> 3」是把**配置值**写成 4，而不是去改判定符号；
//   五期口径直接是「>= 3」，配置值 3 即字面语义。）
func TestThresholdIsNonStrictGreaterOrEqual(t *testing.T) {
	for th := 1; th <= 8; th++ {
		for s := 0; s <= 8; s++ {
			got := SignalQualified(&Signal{Score: s, Ready: true, RisePct: 99}, th, 1.0, 0)
			if want := s >= th; got != want {
				t.Errorf("threshold=%d, score=%d：应为 >= 语义（%v），实际 %v", th, s, want, got)
			}
		}
	}
	// 反向确认：阈值 3 **包含** 3（不是「>3」）
	if !SignalQualified(&Signal{Score: 3, Ready: true, RisePct: 99}, 3, 1.0, 0) {
		t.Errorf("threshold=3 应当放行 score=3（>=3）；若这里失败说明判定被改成了严格大于")
	}
}

// ComputeSignal 必须把「这根 K 线自己的涨跌幅」填进 Signal.RisePct。
//
// 为什么要端到端测：RisePct 在 signalAt（池化路径）和 computeSignalReference
// （对照实现）里各算一次。两处若有一处漏填，那条路径上的所有信号都会
// RisePct=0 → 被「必须涨过 min_bar_rise_pct（当前 0.5%）」全部拒掉，
// 而且不报错、日志也看不出。
func TestComputeSignalFillsRisePct(t *testing.T) {
	n := 400
	cands := make([]Candle, n)
	for i := 0; i < n; i++ {
		px := 100 + float64(i)*0.1
		cands[i] = Candle{Ts: int64(i) * 60000, O: px, H: px * 1.01, L: px * 0.99,
			C: px * 1.002, V: 1000, Confirm: true}
	}
	// 最后一根：开 100 → 收 102，正好 +2%
	cands[n-1].O = 100
	cands[n-1].C = 102

	got := ComputeSignal("TEST-USDT-SWAP", "1m", cands, n-1)
	if math.Abs(got.RisePct-2.0) > 1e-9 {
		t.Fatalf("RisePct 应为 2.0（(102-100)/100×100），实际 %.6f", got.RisePct)
	}
	if !SignalQualified(got, 0+1, 0.5, 0) {
		t.Logf("提示：该根 score=%d（合成序列，仅用于验证 RisePct 通路）", got.Score)
	}

	// 开路价为脏数据（0）时必须记 0 —— 这样它过不了任何正门槛，偏保守。
	bad := append([]Candle(nil), cands...)
	bad[n-1].O = 0
	if r := ComputeSignal("TEST-USDT-SWAP", "1m", bad, n-1).RisePct; r != 0 {
		t.Fatalf("开盘价为 0 时 RisePct 必须记 0，实际 %.6f", r)
	}
}

// 对照实现（computeSignalReference）与新实现必须给出同一个 RisePct。
//
// 这条是「两个看着差不多的函数差一个字段就是静默故障」的守门测试：
// 一期 closedWindow 读错的 Candle.Confirm 就是这么漏掉的。
func TestRisePctMatchesBetweenImplementations(t *testing.T) {
	n := 320
	cands := make([]Candle, n)
	for i := 0; i < n; i++ {
		px := 50 + float64(i)*0.05
		cands[i] = Candle{Ts: int64(i) * 300000, O: px, H: px * 1.02, L: px * 0.98,
			C: px * 1.004, V: 500, Confirm: true}
	}
	cands[n-1].O = 60
	cands[n-1].C = 61.5 // +2.5%

	newImpl := ComputeSignal("A-USDT-SWAP", "5m", cands, n-1)
	refImpl := computeSignalReference("A-USDT-SWAP", "5m", cands, n-1)

	if math.Abs(newImpl.RisePct-refImpl.RisePct) > 1e-12 {
		t.Fatalf("两条实现算出的 RisePct 不一致：新 %.10f vs 对照 %.10f",
			newImpl.RisePct, refImpl.RisePct)
	}
	if math.Abs(newImpl.RisePct-2.5) > 1e-9 {
		t.Fatalf("RisePct 应为 2.5，实际 %.10f", newImpl.RisePct)
	}
}
