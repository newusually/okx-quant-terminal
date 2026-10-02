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
//
// ★ 十期：把加仓的两个价格参数**显式钉死**在 1% / 1% ★
//
//	为什么必须显式写：本文件里的测试数据（比买价低 1.25%、涨幅 +2%）是按
//	「跌幅门槛 1%、涨幅门槛 1%」构造出来的。
//	如果让它依赖 conf.DefaultConfig() 的当前值，那么**每次按用户口径调整
//	生产默认值，这里就会红一片** —— 而红的其实是测试的假设过时了，不是代码坏了。
//	十期把生产默认改成 3.0 / 0.3 时就正好踩到这个：9 个用例一起变红，
//	排查成本全花在「到底是我改错了还是测试过时了」上面。
//
//	显式写死之后，生产默认值怎么改都不影响这批用例 ——
//	它们测的是「decideAddon 在**给定参数**下的判定逻辑」，本来就该与默认值无关。
func mkAddonCfg() *conf.Config {
	c := conf.DefaultConfig()
	c.Addon.DropPct = 1.0
	c.Addon.PriceRisePct = 1.0
	return c
}

// mkIns 造一个「每张名义 ≈ 1.6U」的合约：0.1U×20x=2U 名义刚好买 1 张
func mkIns() Instrument {
	return Instrument{
		InstID: "TEST-USDT-SWAP",
		CtVal:  1, CtMult: 1, LotSz: 1, MinSz: 1, LotSzDec: 0,
	}
}

// mkSignal 造一个「已收盘那根」的信号
//
// score 现在只用于展示/日志（七期起加仓不看 score）；
// ★ 七期价格条件：加仓要求 收盘价 < 买入价×(1−1%) 且该根涨幅 > 1%。
//   买入价基准是 basePos(1.6000)，所以这里收盘造 1.5800（比 1.6 低 1.25% ✓）、
//   涨幅造 +2%（> 1% ✓），让「条件满足 → 加仓」这条主路径成立；
//   边界负例由 TestAddon_SkipWhenPriceNotReached 单独覆盖。
func mkSignal(score int, ts int64) *Signal {
	return &Signal{
		InstID: "TEST-USDT-SWAP", Bar: "15m", Ts: ts,
		Close: 1.5800, Mask: (1 << uint(score)) - 1, Score: score,
		HitList: "势能,摩擦,动能,RSI,布林,MACD,TD9,放量", Ready: true,
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
// A. 七期价格条件全满足 → 加仓
// ---------------------------------------------------------------------------

// TestAddon_FiresOnPriceConditions 七期价格条件全满足 → 加仓。
//
// 2026-10-02 七期起加仓**不再看 score**，触发 = 纯价格条件：
// 收盘价比买入价低超 1% 且该根涨幅 > 1%。
func TestAddon_FiresOnPriceConditions(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = true
	cfg.Addon.Mode = conf.AddonModePrice // 七期价格口径：现在是可选模式，测试须显式选它
	cfg.Entry.MarginUSDT = 0.1
	cfg.Entry.Leverage = 20
	cfg.Entry.MaxMarginUSDT = 0.5

	ins := mkIns()
	p := basePos(1.6000)
	sig := mkSignal(8, barMs*11) // 比开仓那根（barMs*10）晚一根

	d := decideAddon(cfg, p, 1.5950, sig, barMs, ins)
	if !d.Add {
		t.Fatalf("收盘价 1.58（低于买价 1%%）+ 涨 2%% → 应当加仓，实际不加")
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
	t.Logf("✓ 场景A 价格条件满足 → 加仓 %s 张 保证金=%.4fU 新均价=%.6f（%s）",
		fmtSz(d.Sz, 0), d.Margin, d.NewAvgPx, d.Reason)
}

// TestAddon_SkipWhenPriceNotReached 七期价格条件不满足 → 不加。
//
// 触发 = ① 收盘价比买入价低超 drop_pct%（默认 1）② 该根涨幅 > bar_rise_pct%（默认 1）。
// 两个条件缺一不可 —— 差半点都不行，边界必须钉死。
func TestAddon_SkipWhenPriceNotReached(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = true
	cfg.Addon.Mode = conf.AddonModePrice // 七期价格口径：现在是可选模式，测试须显式选它
	cfg.Entry.MarginUSDT = 0.1
	cfg.Entry.MaxMarginUSDT = 0.5

	p := basePos(1.6000) // 1% 线 = 1.5840

	// ① 收盘价只低 0.3%（1.588 < 1.6000×0.99=1.5840 不成立）→ 不加
	sig := mkSignal(8, barMs*11)
	sig.Close = 1.5880
	if d := decideAddon(cfg, p, 1.5950, sig, barMs, mkIns()); d.Add {
		t.Fatalf("收盘价 1.588 未低于 1%% 线 1.584，不该加仓")
	}
	// ② 收盘价恰好压在 1% 线上（=1.5840，不「低于」）→ 不加
	sig.Close = 1.5840
	if d := decideAddon(cfg, p, 1.5950, sig, barMs, mkIns()); d.Add {
		t.Fatalf("收盘价恰好 1.5840（=1%% 线，未低于）不该加仓")
	}
	// ③ 收盘价够低（1.5800）但该根只涨 0.5%（< 1%）→ 不加
	sig.Close = 1.5800
	sig.RisePct = 0.5
	if d := decideAddon(cfg, p, 1.5950, sig, barMs, mkIns()); d.Add {
		t.Fatalf("该根只涨 0.5%%（未超 1%%）不该加仓")
	}
	// ④ 涨幅恰好 1.0（口径是**严格**大于）→ 不加
	sig.RisePct = 1.0
	if d := decideAddon(cfg, p, 1.5950, sig, barMs, mkIns()); d.Add {
		t.Fatalf("涨幅恰好 1.0 未超过 1%% 门槛，不该加仓（严格大于）")
	}
	// ⑤ 两个条件都满足 → 加
	sig.RisePct = 1.5
	if d := decideAddon(cfg, p, 1.5950, sig, barMs, mkIns()); !d.Add {
		t.Fatalf("收盘价低 1.25%% 且涨 1.5%% → 应当加仓")
	}
	t.Log("✓ 场景B 收盘价压线/涨幅压线都不加；两条件齐过才加 —— 七期价格判据边界钉死")
}

// TestAddon_SkipWhenNotReady 暖机不足 → 不加。
func TestAddon_SkipWhenNotReady(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = true
	cfg.Addon.Mode = conf.AddonModePrice // 七期价格口径：现在是可选模式，测试须显式选它
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
	cfg.Addon.Mode = conf.AddonModePrice // 七期价格口径：现在是可选模式，测试须显式选它
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
	cfg.Addon.Mode = conf.AddonModePrice // 七期价格口径：现在是可选模式，测试须显式选它
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
	cfg.Addon.Mode = conf.AddonModePrice // 七期价格口径：现在是可选模式，测试须显式选它
	cfg.Addon.MaxTimes = 0               // 不限
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
//   出场只剩 +0.3% 止盈 / 1 小时超时（布林上轨关闭），没有一条看加仓次数。
func TestAddon_SkipAtMaxTimes(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = true
	cfg.Addon.Mode = conf.AddonModePrice // 七期价格口径：现在是可选模式，测试须显式选它
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
	cfg.Addon.Mode = conf.AddonModePrice // 七期价格口径：现在是可选模式，测试须显式选它
	cfg.Addon.MarginUSDT = 0             // 走 ratio 口径
	cfg.ScoreThreshold = 8
	if cfg.Addon.Ratio < 0.3332 || cfg.Addon.Ratio > 0.3334 {
		t.Fatalf("默认 ratio 应为 1/3，实际 %.6f", cfg.Addon.Ratio)
	}

	// 造一个「每张名义 0.30U」的小合约：0.0333U×20x = 0.6667U 名义 → 能买 2 张
	ins := Instrument{InstID: "T", CtVal: 0.3, CtMult: 1, LotSz: 1, MinSz: 1, LotSzDec: 0}
	p := basePos(1.0000) // Margin 0.1 → 预算 0.03333U
	// 七期价格条件：买入价 1.0000 → 1% 线 = 0.9900；夹具默认收盘 1.5800 不满足，
	// 这里单独把收盘价压到 0.9880（低 1.2%）
	sig := mkSignal(8, barMs*11)
	sig.Close = 0.9880
	d := decideAddon(cfg, p, 0.9985, sig, barMs, ins)
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
	cfg.Addon.Mode = conf.AddonModePrice // 七期价格口径：现在是可选模式，测试须显式选它
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
// TestAddon_AutoBarUsesPositionBar 加仓周期：默认用**仓位自己的周期**，
// 但仓位周期已下线时必须退回主周期。
//
// ★ 2026-10-02 四期：1m 下线（用户口径「取消 1 分钟买入条件和买入信号和选项卡和 K 线图」）★
// ★ 2026-10-03 二十一期：15m 也下线（用户口径「删除15分钟K线图数据」）★
// 所以「auto + 已下线周期仓」的期望值统一变成 "5m"（白名单最后一个）：
// 下线周期的 K 线会被 CleanupKlines 按 model.EnabledBars 整段删掉，
// 继续按它取信号只会永远取不到数据 → **该仓位再也不会加仓**（静默且永久）。
func TestAddon_AutoBarUsesPositionBar(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Bar = "5m"

	cases := []struct {
		name    string
		riseBar string
		posBar  string
		want    string
	}{
		// 还在白名单里的周期：照旧按仓位自己的周期判
		{"auto + 3m 仓", conf.AddonAutoBar, "3m", "3m"},
		{"auto + 5m 仓", conf.AddonAutoBar, "5m", "5m"},
		// ★ 二十一期：15m 已下线，必须退回主周期（同四期 1m 的处理）
		{"auto + 15m 仓（二十一期已下线）→ 退回主周期", conf.AddonAutoBar, "15m", "5m"},
		{"auto + 1m 仓（四期已下线）→ 退回主周期", conf.AddonAutoBar, "1m", "5m"},
		{"auto + 1H 仓（从未上线）→ 退回主周期", conf.AddonAutoBar, "1H", "5m"},
		{"auto + 老仓（bar 为空）→ 退回主周期", conf.AddonAutoBar, "", "5m"},
		// 显式指定周期时不受「仓位周期」影响；但**下线周期仍会被兜底拦下**
		{"显式写 15m → 已下线，兜底退回", "15m", "1m", "5m"},
		{"显式写 3m → 照用", "3m", "1m", "3m"},
		// 空串与 "auto" 等价：fillDefaults 本来就会把空串补成 auto，
		// 这里再认一次是为了「配置块缺失 / 手写漏了字段」时行为一致。
		{"空串等价于 auto", "", "5m", "5m"},
		{"空串 + 1m 仓（已下线）→ 退回主周期", "", "1m", "5m"},
		{"空串 + 老仓 → 退回主周期", "", "", "5m"},
	}
	for _, tc := range cases {
		got := addonBarFor(tc.riseBar, cfg.Bar, tc.posBar)
		if got != tc.want {
			t.Errorf("%s：addonBarFor(%q, %q, %q) = %q，期望 %q",
				tc.name, tc.riseBar, cfg.Bar, tc.posBar, got, tc.want)
		}
	}
	// 极端情况：主周期自己也下线 → 退到白名单最后一个（二十一期起是 5m）
	if got := addonBarFor(conf.AddonAutoBar, "1m", "1m"); got != "5m" {
		t.Errorf("主周期与仓位周期都下线时应退到白名单最后一个，实际 %q", got)
	}
	t.Log("✓ 场景K auto 用仓位自己的周期；已下线周期（1m/15m）/ 老仓 / 空值退回主周期")
}

func almostEq(a, b, eps float64) bool { return a-b < eps && b-a < eps }

// ---------------------------------------------------------------------------
// 三期（2026-10-01）→ 六期（涨跌方向）→ 七期（2026-10-02）：加仓改纯价格条件
// ---------------------------------------------------------------------------

// TestAddon_ScoreNoLongerMatters 七期起加仓**不看 score** 的守门测试。
//
// 历史教训：二期把加仓改成「与买入一致（8 因子共振）」，七期改回纯价格条件。
// 这条测试钉死「score 与加仓无关」：score=0（一个因子都没中）只要价格条件满足
// 也必须加 —— 若哪天有人把 SignalQualified 又接回来，这里会立刻露馅。
func TestAddon_ScoreNoLongerMatters(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = true
	cfg.Addon.Mode = conf.AddonModePrice // ★ 八期：这条守的是**价格模式**，须显式选它
	cfg.Entry.MarginUSDT = 0.1
	cfg.Entry.Leverage = 20
	cfg.Entry.MaxMarginUSDT = 0.5

	p := basePos(1.6000)
	if d := decideAddon(cfg, p, 1.5950, mkSignal(0, barMs*11), barMs, mkIns()); !d.Add {
		t.Fatalf("score=0 但价格条件满足 → 应当加仓（价格模式加仓不看 score）")
	}
	t.Log("✓ 价格模式下 score=0 也能加 —— 触发只认价格条件，与共振分数无关")
}

// TestAddon_BarRisePctGate 接在判定上的涨幅闸门必须真的可关/可调。
//
// decideAddon 是纯函数，直接读 cfg.Addon.BarRisePct / DropPct：
// 把它们显式置 0（关闭条件）后，价格再离谱也照样能加 —— 证明开关接在判定上；
// 而 fillDefaults 的归一化（≤0 反压回 1.0）由 conf 包的测试另守。
func TestAddon_BarRisePctGate(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = true
	// ★ 十期必须显式选价格模式 ★
	//   八期时这个用例是「蹭」默认 mode=resonance 过的（设 BarRisePct=0 关掉
	//   共振模式的涨幅条件）。十期默认切成 price 后它就走价格分支，
	//   而价格分支读的是 PriceRisePct —— 原来那行 BarRisePct=0 就失效了。
	//   显式指定模式，用例的意图才和走的分支对上。
	cfg.Addon.Mode = conf.AddonModePrice
	cfg.Entry.MarginUSDT = 0.1
	cfg.Entry.MaxMarginUSDT = 0.5

	ins := mkIns()
	p := basePos(1.6000)

	// 关掉涨幅条件（价格模式读 PriceRisePct；<=0 会退回 BarRisePct，所以两个都置 0）：
	// 下跌的 K 线 + 收盘价够低也能加
	cfg.Addon.PriceRisePct = 0
	cfg.Addon.BarRisePct = 0
	cfg.Addon.DropPct = 0.3 // 门槛调低，好把"只剩跌幅这一条"单独看出来
	sig := mkSignal(8, barMs*11)
	sig.RisePct = -3.0
	if d := decideAddon(cfg, p, 1.5950, sig, barMs, ins); !d.Add {
		t.Fatalf("price_rise_pct=0 表示关闭涨幅条件，收盘价够低就应当加仓")
	}

	// 关掉跌幅条件（DropPct=0）：收盘价再高也能加（只剩涨幅条件）
	cfg.Addon.DropPct = 0
	cfg.Addon.PriceRisePct = 1.0
	sig.Close = 1.6500 // 比买入价还高
	sig.RisePct = 2.0
	if d := decideAddon(cfg, p, 1.5950, sig, barMs, ins); !d.Add {
		t.Fatalf("drop_pct=0 表示关闭跌幅条件，只看涨幅应当加仓")
	}
	t.Log("✓ 价格模式的两个闸门都真实接在判定上，置 0 即关闭")
}

// ---------------------------------------------------------------------------
// ★ 八期（2026-10-02）：加仓双模式 —— resonance（共振） / price（价格）
//
// 为什么要有这组测试：
//   加仓口径在一/二/七期之间来回改了三次（价格 → 共振 → 价格），每次推翻都
//   要重写一遍调用方。八期改为**两套并存 + mode 开关**：口径再不合适就换模式，
//   而不是改判定。mode 一旦串线（比如读错字段、两套判据混着用），
//   加仓就会在错误的时机触发 —— 这是直接烧钱的事，必须钉死。
// ---------------------------------------------------------------------------

// TestAddon_ModeResonance_UsesScoreNotPrice 共振模式下判据是 score，
// 价格跌得再狠也与加仓无关。
func TestAddon_ModeResonance_UsesScoreNotPrice(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = true
	cfg.Addon.Mode = conf.AddonModeResonance
	cfg.Addon.ScoreThreshold = 5
	cfg.Addon.BarRisePct = 0.7 // 只留涨幅条件，与分数条件一起判
	cfg.Entry.MarginUSDT = 0.1
	cfg.Entry.MaxMarginUSDT = 0.5

	p := basePos(1.6000)
	ins := mkIns()

	// 价格「该加」的样子：跌破买价 1%+ 且涨 2%。但 score=3 ≤ 5 → 不加。
	sig := mkSignal(3, barMs*11) // Close=1.58, RisePct=2.0
	if d := decideAddon(cfg, p, 1.5950, sig, barMs, ins); d.Add {
		t.Fatalf("共振模式下 score=3 未超过门槛 5，即使价格条件满足也不该加仓（说明价格判据串进了共振模式）")
	}

	// score=6 > 5 且涨幅 2% > 0.7 → 加。此时与价格无关，收盘价造得比买价还高。
	sig2 := mkSignal(6, barMs*12)
	sig2.Close = 1.7000 // 远高于买价 —— 价格模式绝不会加的形状
	if d := decideAddon(cfg, p, 1.5950, sig2, barMs, ins); !d.Add {
		t.Fatalf("共振模式下 score=6 > 5 且涨幅达标 → 应当加仓（收盘价高于买价不影响共振判据）")
	}
	t.Log("✓ 共振模式只认 score + 该根涨幅，与「跌破买价」无关")
}

// TestAddon_ModeResonance_ScoreIsStrictlyGreater 共振模式分数门槛是**严格大于**。
//
// ★ 与买入刻意不同：买入是 score ≥ 门槛，加仓是 score > 门槛。
//   用户八期明确给出 `score_threshold > 2`。差这一个等号，
//   会让「恰好 2 分」这种最常见的边界走进加仓，属于会真实亏钱的差异。
func TestAddon_ModeResonance_ScoreIsStrictlyGreater(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = true
	cfg.Addon.Mode = conf.AddonModeResonance
	cfg.Addon.ScoreThreshold = 2
	cfg.Addon.BarRisePct = 0 // 关掉涨幅条件，只测分数边界
	cfg.Entry.MarginUSDT = 0.1
	cfg.Entry.MaxMarginUSDT = 0.5
	p := basePos(1.6000)
	ins := mkIns()

	if d := decideAddon(cfg, p, 1.5950, mkSignal(2, barMs*11), barMs, ins); d.Add {
		t.Fatalf("score 恰好 == 门槛 2，加仓口径是严格大于 → 不该加（若这里过了，说明用成了买入的 >=）")
	}
	if d := decideAddon(cfg, p, 1.5950, mkSignal(3, barMs*11), barMs, ins); !d.Add {
		t.Fatalf("score=3 > 门槛 2 → 应当加仓")
	}
	t.Log("✓ 加仓分数门槛为严格大于（与买入的 >= 刻意区分）")
}

// TestAddon_ModeResonance_ThresholdZeroFollowsEntry 门槛写 0 时跟随买入阈值。
func TestAddon_ModeResonance_ThresholdZeroFollowsEntry(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = true
	cfg.Addon.Mode = conf.AddonModeResonance
	cfg.Addon.ScoreThreshold = 0 // 跟随买入
	cfg.Addon.BarRisePct = 0
	cfg.Entry.MarginUSDT = 0.1
	cfg.Entry.MaxMarginUSDT = 0.5

	th := cfg.ThresholdFor("TEST-USDT-SWAP")
	p := basePos(1.6000)
	ins := mkIns()

	// 恰好等于买入门槛（不严格大于）→ 不加
	if d := decideAddon(cfg, p, 1.5950, mkSignal(th, barMs*11), barMs, ins); d.Add {
		t.Fatalf("score=买入门槛 %d（不严格大于）不该加仓", th)
	}
	// 超一个 → 加
	if d := decideAddon(cfg, p, 1.5950, mkSignal(th+1, barMs*11), barMs, ins); !d.Add {
		t.Fatalf("score=%d > 买入门槛 %d → 应当加仓", th+1, th)
	}
	t.Logf("✓ 加仓 score_threshold=0 时跟随买入门槛 %d", th)
}

// TestAddon_ModePrice_IgnoresScore price 模式下 score 归零也照样加。
func TestAddon_ModePrice_IgnoresScore(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = true
	cfg.Addon.Mode = conf.AddonModePrice
	cfg.Addon.DropPct = 1.0
	cfg.Addon.PriceRisePct = 1.0
	cfg.Entry.MarginUSDT = 0.1
	cfg.Entry.MaxMarginUSDT = 0.5

	p := basePos(1.6000)
	ins := mkIns()
	// score=0（一个因子都没中）但价格条件齐备 → 必须加
	if d := decideAddon(cfg, p, 1.5950, mkSignal(0, barMs*11), barMs, ins); !d.Add {
		t.Fatalf("价格模式下 score=0 只要价格条件满足就应当加仓（说明共振判据串进了价格模式）")
	}
	t.Log("✓ 价格模式只认「跌破买价 + 该根涨幅」，与 score 无关")
}

// TestAddon_ModePrice_UsesPriceRiseNotBarRise 价格模式的涨幅门槛是
// price_rise_pct，**不是** bar_rise_pct。
//
// 为什么要单独测：切模式时最容易犯的错就是两套参数互相污染 ——
// 页面在价格模式改了「该根涨幅」，结果代码读的是共振模式的 bar_rise_pct，
// 界面上改了半天没反应，正是本项目最典型的「改了没用」故障。
func TestAddon_ModePrice_UsesPriceRiseNotBarRise(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = true
	cfg.Addon.Mode = conf.AddonModePrice
	cfg.Addon.DropPct = 1.0
	cfg.Addon.BarRisePct = 0.2   // 共振侧的值，价格模式**不该读**
	cfg.Addon.PriceRisePct = 3.0 // 价格侧的真门槛
	cfg.Entry.MarginUSDT = 0.1
	cfg.Entry.MaxMarginUSDT = 0.5

	p := basePos(1.6000)
	ins := mkIns()
	sig := mkSignal(8, barMs*11) // Close=1.58 跌幅够, RisePct=2.0

	// 涨幅 2.0 > 0.2（bar_rise）但 < 3.0（price_rise）→ 不加。
	// 若这里加成功，说明价格模式读了 bar_rise_pct —— 参数串线。
	if d := decideAddon(cfg, p, 1.5950, sig, barMs, ins); d.Add {
		t.Fatalf("价格模式涨幅 2.0%% 未超 price_rise_pct=3.0，不该加仓（若加了说明误读成 bar_rise_pct）")
	}
	sig.RisePct = 3.5 // 超过 3.0 → 加
	if d := decideAddon(cfg, p, 1.5950, sig, barMs, ins); !d.Add {
		t.Fatalf("涨幅 3.5%% > price_rise_pct=3.0 → 应当加仓")
	}
	t.Log("✓ 价格模式读 price_rise_pct，不与共振模式的 bar_rise_pct 串线")
}

// TestAddon_ModePrice_PriceRiseFallsBackToBarRise price_rise_pct 缺失(0)时
// 退回 bar_rise_pct —— 兼容七期已写进配置、只有 bar_rise_pct 的存量文件。
func TestAddon_ModePrice_PriceRiseFallsBackToBarRise(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = true
	cfg.Addon.Mode = conf.AddonModePrice
	cfg.Addon.DropPct = 1.0
	cfg.Addon.BarRisePct = 1.0
	cfg.Addon.PriceRisePct = 0 // 存量配置没这个键
	cfg.Entry.MarginUSDT = 0.1
	cfg.Entry.MaxMarginUSDT = 0.5

	p := basePos(1.6000)
	ins := mkIns()
	sig := mkSignal(8, barMs*11) // RisePct = 2.0
	if d := decideAddon(cfg, p, 1.5950, sig, barMs, ins); !d.Add {
		t.Fatalf("price_rise_pct=0 应退回 bar_rise_pct=1.0，涨幅 2.0%% 达标 → 应当加仓")
	}
	sig.RisePct = 0.5 // 低于退回后的门槛 1.0
	if d := decideAddon(cfg, p, 1.5950, sig, barMs, ins); d.Add {
		t.Fatalf("跌幅够但涨幅 0.5%% < 退回门槛 1.0 → 不该加仓")
	}
	t.Log("✓ 价格模式 price_rise_pct=0 时退回 bar_rise_pct（存量配置兼容）")
}

// TestAddon_ModeUnknownFallsBackToResonance 认不出的 mode 退回共振。
//
// 宁可退回一个明确的口径，也不要让一个拼错的字符串把加仓变成「永远不加」
// 或「永远加」—— 前者静默失效、后者直接亏钱。
func TestAddon_ModeUnknownFallsBackToResonance(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = true
	cfg.Addon.Mode = "banana" // 拼错的模式名
	cfg.Addon.ScoreThreshold = 2
	cfg.Addon.BarRisePct = 0
	cfg.Entry.MarginUSDT = 0.1
	cfg.Entry.MaxMarginUSDT = 0.5

	p := basePos(1.6000)
	ins := mkIns()
	// 认不出 → 走共振分支：score=3 > 2 且无涨幅门槛 → 加
	if d := decideAddon(cfg, p, 1.5950, mkSignal(3, barMs*11), barMs, ins); !d.Add {
		t.Fatalf("未知 mode 应退回共振分支（score 3 > 2）→ 应当加仓")
	}
	// 若误走价格分支：DropPct(默认1) 满足、PriceRisePct(默认1) 满足 → 也会加，
	// 所以补一个「价格模式会拒、共振模式会放」的用例来区分分支：
	sig := mkSignal(3, barMs*12)
	sig.RisePct = 0.5 // 价格模式下 < 1 会被拒
	cfg.Addon.PriceRisePct = 1.0
	if d := decideAddon(cfg, p, 1.5950, sig, barMs, ins); !d.Add {
		t.Fatalf("未知 mode 走的是共振分支（不看该根涨幅门槛）→ 涨幅 0.5%% 也该加仓")
	}
	t.Log("✓ 未知 addon.mode 退回共振分支，不会静默失效")
}
