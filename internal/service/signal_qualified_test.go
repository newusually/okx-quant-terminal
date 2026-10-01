package service

import (
	"math"
	"testing"
)

// 三期口径（2026-10-01）：
//
//	「score >3 + 额外条件：有信号的那个 K 线必须大于 1% 涨幅才行，
//	  就买入和加仓」
//
// SignalQualified 是买入扫描 / 加仓判定 / 信号入库**三处共用的唯一判据**，
// 所以它的边界必须钉死 —— 一旦这里漂移，三处会一起漂，而且不报错。
func TestSignalQualified(t *testing.T) {
	mk := func(score int, ready bool, rise float64) *Signal {
		return &Signal{Score: score, Ready: ready, RisePct: rise}
	}

	cases := []struct {
		name      string
		sig       *Signal
		threshold int
		minRise   float64
		want      bool
	}{
		{"A 用户口径：score 4 且涨 2% → 通过", mk(4, true, 2.0), 4, 1.0, true},
		{"B score 3 不满足「>3」→ 拒绝", mk(3, true, 2.0), 4, 1.0, false},
		{"C score 满分但只涨 0.5% → 拒绝", mk(8, true, 0.5), 4, 1.0, false},
		{"D 涨幅恰好 1.0（口径是严格大于）→ 拒绝", mk(5, true, 1.0), 4, 1.0, false},
		{"E 涨幅 1.0001 → 通过", mk(5, true, 1.0001), 4, 1.0, true},
		{"F minRise=0 = 关闭涨幅条件 → 通过", mk(5, true, -3.0), 4, 0, true},
		{"G 暖机不足（Ready=false）→ 拒绝", mk(8, false, 5.0), 4, 1.0, false},
		{"H nil 信号 → 拒绝", nil, 4, 1.0, false},
		{"I 阈值 0（配置坏掉）→ 拒绝，而不是放宽", mk(8, true, 5.0), 0, 1.0, false},
		{"J RisePct 是 NaN → 拒绝（保守）", mk(8, true, math.NaN()), 4, 1.0, false},
		{"K 下跌的 K 线 → 拒绝", mk(8, true, -2.0), 4, 1.0, false},
		{"L minRise 为负 = 按未填处理，不启用", mk(5, true, 0.0), 4, -1.0, true},
	}

	for _, c := range cases {
		if got := SignalQualified(c.sig, c.threshold, c.minRise); got != c.want {
			t.Errorf("%s：得到 %v，期望 %v", c.name, got, c.want)
		}
	}
}

// 「score_threshold = 4」必须严格等价于用户说的「score > 3」。
//
// 这是本次最容易出错的一步：判定处是 `Score >= threshold`，
// 用户说的是 `Score > 3`。若哪天有人把阈值改成 3（「3 以上嘛」），
// 语义就变成了 score >= 3，门槛被悄悄放宽一档 —— 这个测试会立刻红。
func TestThresholdFourIsExactlyScoreGreaterThanThree(t *testing.T) {
	for s := 0; s <= 8; s++ {
		got := SignalQualified(&Signal{Score: s, Ready: true, RisePct: 99}, 4, 1.0)
		want := s > 3
		if got != want {
			t.Errorf("score=%d：threshold=4 应等价于 score>3（%v），实际 %v", s, want, got)
		}
	}
	// 反向确认：阈值 3 就**不是**「>3」，而是「>=3」（含 3）
	if !SignalQualified(&Signal{Score: 3, Ready: true, RisePct: 99}, 3, 1.0) {
		t.Errorf("threshold=3 应当放行 score=3（>=3）；若这里失败说明判定被改成了严格大于")
	}
}

// ComputeSignal 必须把「这根 K 线自己的涨跌幅」填进 Signal.RisePct。
//
// 为什么要端到端测：RisePct 在 signalAt（池化路径）和 computeSignalReference
// （对照实现）里各算一次。两处若有一处漏填，那条路径上的所有信号都会
// RisePct=0 → 被「必须涨过 1%」全部拒掉，而且不报错、日志也看不出。
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
	if !SignalQualified(got, 0+1, 1.0) {
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
