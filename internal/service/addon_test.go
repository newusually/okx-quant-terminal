package service

// addon_test.go —— 加仓规则的实测验证
//
// ★ 2026-10-01 二期口径变更 ★
//   旧口径：15m 先跌 0.5% 后转涨 → 补 1/3
//   新口径：**与买入条件完全一致** —— 8 因子共振（score ≥ cfg.ThresholdFor）
//           且次数不限（max_times = 0）
//
// 所以这个文件整体重写，覆盖的场景：
//   A. 8/8 全中 + 晚于开仓那根            → 必须加仓，金额 = 原保证金 1/3
//   B. 只有 7/8（差一个因子）              → 不加（这正是「条件与买入一致」的意义）
//   C. 指标暖机不足（Ready=false）         → 不加
//   D. 信号就是开仓那一根（同一根）        → 不加（否则开仓瞬间重复下单）
//   E. 间隔未到 min_gap_bars               → 不加
//   F. max_times = 0（不限）+ 已加过 99 次 → **仍然加**（二期核心变更）
//   G. max_times = 3 + 已加 3 次           → 不加（写了正数才限制）
//   H. 开关关闭                            → 不加
//   I. 金额口径：预算 = 原保证金 × 1/3
//   J. 合并后的加权均价 / 张数 / 保证金
//   K. 「auto」周期解析：用仓位自己的周期
//
// 另外校验：加仓阈值与买入阈值是**同一个函数**（cfg.ThresholdFor），
// 否则「加仓条件与买入一致」就只是句话。

import (
	"testing"

	"finally-main/internal/conf"
	"finally-main/internal/repo"
)

const barMs = 15 * 60 * 1000

// mkAddonCfg 一套最小可用的配置（用内置兜底值，再按需覆盖）
func mkAddonCfg() *conf.Config {
	return conf.DefaultConfig()
}

// mkIns 造一个「每张名义 ≈ 1.6U」的合约：0.1U×20x=2U 名义刚好买 1 张
func mkIns() Instrument {
	return Instrument{
		InstID: "TEST-USDT-SWAP",
		CtVal:  1, CtMult: 1, LotSz: 1, MinSz: 1, LotSzDec: 0,
	}
}

// mkSignal 造一个「已收盘那根」的共振结果
//
// score 直接决定能不能过阈值；hitList 只用于日志/原因文本。
func mkSignal(score int, ts int64) *Signal {
	return &Signal{
		InstID: "TEST-USDT-SWAP", Bar: "15m", Ts: ts,
		Close: 1.5950, Mask: (1 << uint(score)) - 1, Score: score,
		HitList: "势能,摩擦,动能,RSI,布林,MACD,TD9,放量", Ready: true,
		// ★ 2026-10-02 三期：加仓判定与买入共用 SignalQualified，
		//   默认还要求「这根 K 线必须真涨 > min_bar_rise_pct（1%）」。
		//   这里造一根涨 2% 的 K 线，让「分数够 → 加仓」这条主路径仍然成立；
		//   RisePct 不够的负例由 TestAddon_RejectedWhenBarDidNotRise 单独覆盖。
		RisePct: 2.0,
	}
}

// basePos 一个 15m 开的仓：0.1U 保证金、20x、开仓价 1.6、开仓时间 barMs*10
func basePos(entryPx float64) repo.OpenPos {
	return repo.OpenPos{
		ID: 1, InstID: "TEST-USDT-SWAP", Sz: 1, EntryPx: entryPx,
		Margin: 0.1, Leverage: 20, OpenTs: barMs * 10, Bar: "15m",
	}
}

// ---------------------------------------------------------------------------
// A. 满共振 → 加仓
// ---------------------------------------------------------------------------

func TestAddon_FiresOnFullResonance(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = true
	cfg.ScoreThreshold = 8
	cfg.Entry.MarginUSDT = 0.1
	cfg.Entry.Leverage = 20
	cfg.Entry.MaxMarginUSDT = 0.5

	ins := mkIns()
	p := basePos(1.6000)
	sig := mkSignal(8, barMs*11) // 比开仓那根（barMs*10）晚一根

	d := decideAddon(cfg, p, 1.5950, sig, barMs, ins)
	if !d.Add {
		t.Fatalf("8/8 共振 + 晚于开仓那根 → 应当加仓，实际不加")
	}
	if d.Margin <= 0 || d.Sz <= 0 {
		t.Fatalf("加仓保证金/张数应 > 0，实际 margin=%.6f sz=%v", d.Margin, d.Sz)
	}
	if d.Margin > cfg.Entry.MaxMarginUSDT+1e-9 {
		t.Fatalf("加仓保证金 %.4f 超过上限 %.4f", d.Margin, cfg.Entry.MaxMarginUSDT)
	}
	if d.Count != p.AddonCount+1 {
		t.Fatalf("加仓次数应为 %d，实际 %d", p.AddonCount+1, d.Count)
	}
	if d.Ts != sig.Ts {
		t.Fatalf("加仓时间应用信号那根 K 线的时间 %d，实际 %d", sig.Ts, d.Ts)
	}
	wantAvg := (p.Sz*p.EntryPx + d.Sz*1.5950) / (p.Sz + d.Sz)
	if !almostEq(d.NewAvgPx, wantAvg, 1e-9) {
		t.Fatalf("合并均价算错：got %.8f want %.8f", d.NewAvgPx, wantAvg)
	}
	if d.NewMargin <= p.Margin {
		t.Fatalf("合并保证金应增加")
	}
	t.Logf("✓ 场景A 8/8 共振 → 加仓 %s 张 保证金=%.4fU 新均价=%.6f（%s）",
		fmtSz(d.Sz, 0), d.Margin, d.NewAvgPx, d.Reason)
}

// TestAddon_SkipWhenScoreBelowThreshold 7/8 → 不加。
//
// 这是二期最重要的一条：用户口径「加仓条件也是和买入条件一样」，
// 买入是 8 个全中，加仓就必须也是 8 个全中，差一个都不行。
func TestAddon_SkipWhenScoreBelowThreshold(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = true
	cfg.ScoreThreshold = 8
	cfg.Entry.MarginUSDT = 0.1
	cfg.Entry.MaxMarginUSDT = 0.5

	p := basePos(1.6000)
	if d := decideAddon(cfg, p, 1.5950, mkSignal(7, barMs*11), barMs, mkIns()); d.Add {
		t.Fatalf("7/8 < 阈值 8，不该加仓")
	}
	// 阈值降到 7 之后，同样的信号就该加 —— 证明「阈值真的是同一个入口」
	cfg.ScoreThreshold = 7
	if d := decideAddon(cfg, p, 1.5950, mkSignal(7, barMs*11), barMs, mkIns()); !d.Add {
		t.Fatalf("阈值 7 时 7/8 应当加仓（说明加仓读的确实是 cfg.ThresholdFor）")
	}
	t.Log("✓ 场景B 7/8 不加；阈值降到 7 后加 —— 加仓与买入共用同一个阈值入口")
}

// TestAddon_SkipWhenNotReady 暖机不足 → 不加。
func TestAddon_SkipWhenNotReady(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = true
	cfg.ScoreThreshold = 8
	cfg.Entry.MarginUSDT = 0.1
	cfg.Entry.MaxMarginUSDT = 0.5

	p := basePos(1.6000)
	sig := mkSignal(8, barMs*11)
	sig.Ready = false
	if d := decideAddon(cfg, p, 1.5950, sig, barMs, mkIns()); d.Add {
		t.Fatalf("指标暖机不足（Ready=false）时不该加仓")
	}
	t.Log("✓ 场景C Ready=false → 不加")
}

// TestAddon_SkipOnSameBarAsEntry 信号就是开仓那一根 → 不加。
//
// 开仓时 trade.open_ts 写的就是触发开仓那根 K 线的时间戳，
// 所以「sig.Ts <= p.OpenTs」正好挡住「同一根再补一次」。
func TestAddon_SkipOnSameBarAsEntry(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = true
	cfg.ScoreThreshold = 8
	cfg.Entry.MarginUSDT = 0.1
	cfg.Entry.MaxMarginUSDT = 0.5

	p := basePos(1.6000) // OpenTs = barMs*10
	if d := decideAddon(cfg, p, 1.5950, mkSignal(8, p.OpenTs), barMs, mkIns()); d.Add {
		t.Fatalf("信号与开仓同一根 K 线时不该加仓（会变成重复下单）")
	}
	t.Log("✓ 场景D 同一根 K 线 → 不加")
}

// TestAddon_SkipWhenGapNotReached 距上次加仓不足 min_gap_bars → 不加。
func TestAddon_SkipWhenGapNotReached(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = true
	cfg.Addon.MinGapBars = 2
	cfg.ScoreThreshold = 8
	cfg.Entry.MarginUSDT = 0.1
	cfg.Entry.MaxMarginUSDT = 0.5

	p := basePos(1.6000)
	p.LastAddonTs = barMs * 11
	if d := decideAddon(cfg, p, 1.5950, mkSignal(8, barMs*12), barMs, mkIns()); d.Add {
		t.Fatalf("距上次加仓只隔 1 根（要求 2 根）时不该加仓")
	}
	p.LastAddonTs = barMs * 10
	if d := decideAddon(cfg, p, 1.5950, mkSignal(8, barMs*12), barMs, mkIns()); !d.Add {
		t.Fatalf("隔 2 根了，应当加仓")
	}
	t.Log("✓ 场景E 间隔 1 根不加 / 2 根加")
}

// ---------------------------------------------------------------------------
// F/G. 次数：0 = 不限（二期核心变更）
// ---------------------------------------------------------------------------

// TestAddon_MaxTimesZeroIsUnlimited max_times=0 时加过 99 次仍然继续加。
//
// ★ 这是「count 类配置必须三处同改」的守门测试 ★
// 一期在两个计数器（max_concurrent_positions / daily_max_entries）上踩过坑：
// 归一化把 0 反压回默认值，配置里写 0 完全没用。加仓次数是同一类字段。
func TestAddon_MaxTimesZeroIsUnlimited(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = true
	cfg.Addon.MaxTimes = 0 // 不限
	cfg.ScoreThreshold = 8
	cfg.Entry.MarginUSDT = 0.1
	cfg.Entry.MaxMarginUSDT = 0.5

	p := basePos(1.6000)
	p.AddonCount = 99 // 已经加了 99 次
	p.AddonMargin = 9.9

	d := decideAddon(cfg, p, 1.5950, mkSignal(8, barMs*11), barMs, mkIns())
	if !d.Add {
		t.Fatalf("max_times=0 = 不限，加了 99 次也应继续加，却被拦下")
	}
	if d.Count != 100 {
		t.Fatalf("合并后次数应为 100，实际 %d", d.Count)
	}
	t.Log("✓ 场景F max_times=0（不限）→ 第 100 次照样加")
}

// TestAddon_SkipAtMaxTimes 写了正数才限制：max_times=3 且已加 3 次 → 不加。
//
// ★ 2026-10-01：加满之后只「不再补仓」，绝不平仓。
//   出场只剩 +1% 止盈 / 6 小时超时 / 布林上轨，没有一条看加仓次数。
func TestAddon_SkipAtMaxTimes(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = true
	cfg.Addon.MaxTimes = 3
	cfg.ScoreThreshold = 8
	cfg.Entry.MarginUSDT = 0.1
	cfg.Entry.MaxMarginUSDT = 0.5

	p := basePos(1.6000)
	p.AddonCount = 3

	if d := decideAddon(cfg, p, 1.5950, mkSignal(8, barMs*11), barMs, mkIns()); d.Add {
		t.Fatalf("已达 max_times=3，不该再加仓")
	}
	p.AddonCount = 2
	if d := decideAddon(cfg, p, 1.5950, mkSignal(8, barMs*11), barMs, mkIns()); !d.Add {
		t.Fatalf("只加过 2 次（上限 3），条件成立就该继续加")
	}
	t.Log("✓ 场景G 3 次已满不加 / 2 次未满继续加（且不会触发平仓）")
}

func TestAddon_SkipWhenDisabled(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = false
	cfg.ScoreThreshold = 8
	p := basePos(1.6000)

	if d := decideAddon(cfg, p, 1.5950, mkSignal(8, barMs*11), barMs, mkIns()); d.Add {
		t.Fatalf("开关关闭时不该加仓")
	}
	t.Log("✓ 场景H 加仓开关关闭 → 不加")
}

// ---------------------------------------------------------------------------
// I/J. 金额与合并
// ---------------------------------------------------------------------------

// TestAddon_RatioIsOneThird 加仓预算 = 原持仓保证金 × 1/3（用户口径「仓位为本金的三分之一」）。
func TestAddon_RatioIsOneThird(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = true
	cfg.Addon.MarginUSDT = 0 // 走 ratio 口径
	cfg.ScoreThreshold = 8
	if cfg.Addon.Ratio < 0.3332 || cfg.Addon.Ratio > 0.3334 {
		t.Fatalf("默认 ratio 应为 1/3，实际 %.6f", cfg.Addon.Ratio)
	}

	// 造一个「每张名义 0.30U」的小合约：0.0333U×20x = 0.6667U 名义 → 能买 2 张
	ins := Instrument{InstID: "T", CtVal: 0.3, CtMult: 1, LotSz: 1, MinSz: 1, LotSzDec: 0}
	p := basePos(1.0000) // Margin 0.1 → 预算 0.03333U
	d := decideAddon(cfg, p, 0.9985, mkSignal(8, barMs*11), barMs, ins)
	if !d.Add {
		t.Fatalf("应当加仓")
	}
	// 预算 0.1/3 = 0.03333U → 名义 0.6667U → 每张 0.2996U → floor(0.6667/0.2996)=2 张
	if d.Sz != 2 {
		t.Logf("张数 = %v（每张名义 %.4f）", d.Sz, 0.3*0.9985)
	}
	t.Logf("✓ 场景I 按 1/3 预算下单：%s 张 · 保证金 %.4fU · 均价 %.6f",
		fmtSz(d.Sz, 0), d.Margin, d.NewAvgPx)
}

func TestAddon_WeightedAverage(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = true
	cfg.ScoreThreshold = 8
	cfg.Entry.MarginUSDT = 0.1
	cfg.Entry.MaxMarginUSDT = 0.5

	ins := mkIns()
	p := basePos(1.6000)
	p.Sz = 2
	p.Margin = 0.2

	d := decideAddon(cfg, p, 1.5950, mkSignal(8, barMs*11), barMs, ins)
	if !d.Add {
		t.Fatalf("应当加仓")
	}
	expSz := p.Sz + d.Sz
	expAvg := (p.Sz*p.EntryPx + d.Sz*1.5950) / expSz
	expMargin := p.Margin + d.Margin
	if d.NewSz != expSz {
		t.Fatalf("合并张数 got %v want %v", d.NewSz, expSz)
	}
	if !almostEq(d.NewAvgPx, expAvg, 1e-9) {
		t.Fatalf("合并均价 got %.8f want %.8f", d.NewAvgPx, expAvg)
	}
	if !almostEq(d.NewMargin, expMargin, 1e-9) {
		t.Fatalf("合并保证金 got %.8f want %.8f", d.NewMargin, expMargin)
	}
	t.Logf("✓ 场景J 2 张@1.6000 + %s 张@%.4f → 均价 %.6f（原 1.6000，摊薄 %.4f%%）",
		fmtSz(d.Sz, 0), 1.5950, d.NewAvgPx, (1-d.NewAvgPx/1.6000)*100)
}

// ---------------------------------------------------------------------------
// K. 周期解析
// ---------------------------------------------------------------------------

// TestAddon_AutoBarUsesPositionBar rise_bar="auto" 时按「该仓位自己的周期」判定。
//
// 一期四个周期都参与开仓之后，这条必须成立：
// 1m 开的仓要按 1m 判加仓，15m 开的仓按 15m 判 —— 否则「加仓条件与买入一致」
// 在短周期仓位上是假的（拿 15m 的信号去加 1m 的仓）。
func TestAddon_AutoBarUsesPositionBar(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Bar = "15m"

	cases := []struct {
		name    string
		riseBar string
		posBar  string
		want    string
	}{
		{"auto + 1m 仓", conf.AddonAutoBar, "1m", "1m"},
		{"auto + 5m 仓", conf.AddonAutoBar, "5m", "5m"},
		{"auto + 15m 仓", conf.AddonAutoBar, "15m", "15m"},
		{"auto + 老仓（bar 为空）→ 退回主周期", conf.AddonAutoBar, "", "15m"},
		{"显式写 15m → 忽略仓位周期", "15m", "1m", "15m"},
		// 空串与 "auto" 等价：fillDefaults 本来就会把空串补成 auto，
		// 这里再认一次是为了「配置块缺失 / 手写漏了字段」时行为一致。
		{"空串等价于 auto", "", "1m", "1m"},
		{"空串 + 老仓 → 退回主周期", "", "", "15m"},
	}
	for _, tc := range cases {
		got := addonBarFor(tc.riseBar, cfg.Bar, tc.posBar)
		if got != tc.want {
			t.Errorf("%s：addonBarFor(%q, %q, %q) = %q，期望 %q",
				tc.name, tc.riseBar, cfg.Bar, tc.posBar, got, tc.want)
		}
	}
	t.Log("✓ 场景K auto 用仓位自己的周期；老仓 / 空值退回主周期")
}

func almostEq(a, b, eps float64) bool { return a-b < eps && b-a < eps }

// ---------------------------------------------------------------------------
// 三期（2026-10-01）：加仓也必须「触发那根 K 线真涨」
// ---------------------------------------------------------------------------

// TestAddon_SkipWhenBarDidNotRise 分数够、但触发那根 K 线没真涨 → 不加仓。
//
// 用户口径：「score >3 + 额外条件 有信号的那个 K 线必须大于 1% 涨幅才行，
// 就买入和加仓」。加仓与买入共用 SignalQualified，所以这里也必须被拦住。
//
// 这条特别值得测：涨幅条件是在 SignalQualified 里判的。若哪天有人把加仓
// 改回「自己判分数」，涨幅条件就会只在买入路径生效 —— 而加仓次数不限，
// 会在一根下跌的 K 线上反复补仓。亏得最快的就是这种。
func TestAddon_SkipWhenBarDidNotRise(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = true
	cfg.ScoreThreshold = 4 // 三期默认（= score > 3）
	cfg.Entry.MarginUSDT = 0.1
	cfg.Entry.Leverage = 20
	cfg.Entry.MaxMarginUSDT = 0.5

	ins := mkIns()
	p := basePos(1.6000)

	// 分数够（8/8），但这根只涨 0.3% → 不加
	sig := mkSignal(8, barMs*11)
	sig.RisePct = 0.3
	if d := decideAddon(cfg, p, 1.5950, sig, barMs, ins); d.Add {
		t.Fatalf("触发那根只涨 0.3%%（< 1%%）不该加仓")
	}

	// 涨 0.99% 仍然不加（口径是**严格**大于 1%）
	sig.RisePct = 0.99
	if d := decideAddon(cfg, p, 1.5950, sig, barMs, ins); d.Add {
		t.Fatalf("涨 0.99%% 未超过 1%%，不该加仓（口径是严格大于）")
	}

	// 涨 1.2% → 加
	sig.RisePct = 1.2
	if d := decideAddon(cfg, p, 1.5950, sig, barMs, ins); !d.Add {
		t.Fatalf("涨 1.2%% 且 8/8 共振 → 应当加仓")
	}

	// 把门槛显式关掉（写 0）之后，涨 0.3% 也能加 —— 证明这个开关真的接在判定上
	zero := 0.0
	cfg.Entry.MinBarRisePct = &zero
	sig.RisePct = 0.3
	if d := decideAddon(cfg, p, 1.5950, sig, barMs, ins); !d.Add {
		t.Fatalf("min_bar_rise_pct=0 表示关闭该条件，应当加仓（0 不能被反压成 1.0）")
	}
}

// TestAddon_ThreePeriodDefaultScore 三期默认口径（threshold=4）下 score=4 也要能加仓。
//
// 这是「加仓阈值默认值」与「买入阈值默认值」必须同一个入口的守门测试：
// 两者都走 cfg.ThresholdFor，任何一边单独改默认值都会在这里露馅。
func TestAddon_ThreePeriodDefaultScore(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = true
	cfg.Entry.MarginUSDT = 0.1
	cfg.Entry.Leverage = 20
	cfg.Entry.MaxMarginUSDT = 0.5

	// 刻意不显式设置 ScoreThreshold：默认必须是 4（= 用户说的「score > 3」）
	if got := cfg.ThresholdFor("TEST-USDT-SWAP"); got != 4 {
		t.Fatalf("三期默认阈值应为 4（等价 score>3），实际 %d", got)
	}
	p := basePos(1.6000)
	if d := decideAddon(cfg, p, 1.5950, mkSignal(4, barMs*11), barMs, mkIns()); !d.Add {
		t.Fatalf("score=4（>3）且涨 2%% → 应当加仓，说明默认阈值没落到 4")
	}
	// score=3 在「>3」的边界外侧 → 不加
	if d := decideAddon(cfg, p, 1.5950, mkSignal(3, barMs*11), barMs, mkIns()); d.Add {
		t.Fatalf("score=3 不满足「>3」，不该加仓")
	}
}
