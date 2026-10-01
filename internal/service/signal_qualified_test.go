package service

import (
	"math"
	"testing"
)

// 五期口径（2026-10-02）：
//
//	「Score >= 3 且 RisePct > 0.5（严格大于）」—— 两个条件必须落在**同一根已收盘 K 线**上。
//
// SignalQualified 是买入扫描 / 加仓判定 / 信号入库**三处共用的唯一判据**，
// 所以它的边界必须钉死 —— 一旦这里漂移，三处会一起漂，而且不报错。
func TestSignalQualified(t *testing.T) {
	mk := func(score int, ready bool, rise float64) *Signal {
		return &Signal{Score: score, Ready: ready, RisePct: rise}
	}

	const (
		th = 3   // 五期默认阈值（configs/okx_strategy.json 的 score_threshold）
		mr = 0.5 // 五期默认涨幅门槛（entry.min_bar_rise_pct）
	)

	cases := []struct {
		name      string
		sig       *Signal
		threshold int
		minRise   float64
		want      bool
	}{
		{"A 用户口径：score 3 且涨 2% → 通过", mk(3, true, 2.0), th, mr, true},
		{"B score 2 不满足「>= 3」→ 拒绝", mk(2, true, 2.0), th, mr, false},
		{"C score 满分但只涨 0.4% → 拒绝", mk(8, true, 0.4), th, mr, false},
		{"D 涨幅恰好 0.5（口径是严格大于）→ 拒绝", mk(5, true, 0.5), th, mr, false},
		{"E 涨幅 0.5001 → 通过", mk(5, true, 0.5001), th, mr, true},
		{"F 分数刚好 3 但涨幅压线 0.5 → 拒绝（两个条件缺一不可）", mk(3, true, 0.5), th, mr, false},
		{"G minRise=0 = 关闭涨幅条件 → 通过", mk(5, true, -3.0), th, 0, true},
		{"H 暖机不足（Ready=false）→ 拒绝", mk(8, false, 5.0), th, mr, false},
		{"I nil 信号 → 拒绝", nil, th, mr, false},
		{"J 阈值 0（配置坏掉）→ 拒绝，而不是放宽", mk(8, true, 5.0), 0, mr, false},
		{"K RisePct 是 NaN → 拒绝（保守）", mk(8, true, math.NaN()), th, mr, false},
		{"L 下跌的 K 线 → 拒绝", mk(8, true, -2.0), th, mr, false},
		{"M minRise 为负 = 按未填处理，不启用", mk(5, true, 0.0), th, -1.0, true},
	}

	for _, c := range cases {
		if got := SignalQualified(c.sig, c.threshold, c.minRise); got != c.want {
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
			got := SignalQualified(&Signal{Score: s, Ready: true, RisePct: 99}, th, 1.0)
			if want := s >= th; got != want {
				t.Errorf("threshold=%d, score=%d：应为 >= 语义（%v），实际 %v", th, s, want, got)
			}
		}
	}
	// 反向确认：阈值 3 **包含** 3（不是「>3」）
	if !SignalQualified(&Signal{Score: 3, Ready: true, RisePct: 99}, 3, 1.0) {
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
	if !SignalQualified(got, 0+1, 0.5) {
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
