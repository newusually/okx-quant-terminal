package conf

import (
	"testing"

	"finally-main/internal/model"
)

// 这两个字段的语义是「<= 0 = 不限」（2026-10-01 用户口径「取消限制」）。
//
// 为什么值得单独写测试：这里踩过一次很典型的坑 ——
// 归一化里原本写的是「<= 0 → 兜底回默认值（8 / 30）」，
// 于是**配置文件里把 8 改成 0 完全没用**，会被反压回 8，看起来改了其实还在拦。
// 症状就是用户反馈的「持仓和当日买入数量太少了」。
//
// 所以这组测试守的不是「数字对不对」，而是「0 到底代表什么」。
// 只要有人再把 `<= 0 → 默认值` 那种兜底加回来，这里立刻红。
func TestEntryLimitsZeroMeansUnlimited(t *testing.T) {
	// 0 = 不限：归一化必须原样保留 0，不能变成默认的 8 / 30。
	c := &Config{Entry: &EntryCfg{MaxConcurrentPositions: 0, DailyMaxEntries: 0}}
	fillDefaults(c)
	if c.Entry.MaxConcurrentPositions != 0 {
		t.Fatalf("max_concurrent_positions 写 0（=不限）被改成了 %d，限制会复活",
			c.Entry.MaxConcurrentPositions)
	}
	if c.Entry.DailyMaxEntries != 0 {
		t.Fatalf("daily_max_entries 写 0（=不限）被改成了 %d，限制会复活",
			c.Entry.DailyMaxEntries)
	}
}

// 显式写正数时必须原样保留 —— 「取消限制」不等于「再也设不回来」。
func TestEntryLimitsPositivePreserved(t *testing.T) {
	c := &Config{Entry: &EntryCfg{MaxConcurrentPositions: 5, DailyMaxEntries: 12}}
	fillDefaults(c)
	if c.Entry.MaxConcurrentPositions != 5 {
		t.Fatalf("max_concurrent_positions 写 5 变成了 %d", c.Entry.MaxConcurrentPositions)
	}
	if c.Entry.DailyMaxEntries != 12 {
		t.Fatalf("daily_max_entries 写 12 变成了 %d", c.Entry.DailyMaxEntries)
	}
}

// 负数归 0：交易判定只看 `> 0`，负数与 0 等价，
// 但留在配置里会让 /api/state 显示 -3 这种令人困惑的值。
func TestEntryLimitsNegativeNormalizedToZero(t *testing.T) {
	c := &Config{Entry: &EntryCfg{MaxConcurrentPositions: -1, DailyMaxEntries: -99}}
	fillDefaults(c)
	if c.Entry.MaxConcurrentPositions != 0 {
		t.Fatalf("负数应归 0（不限），实际 %d", c.Entry.MaxConcurrentPositions)
	}
	if c.Entry.DailyMaxEntries != 0 {
		t.Fatalf("负数应归 0（不限），实际 %d", c.Entry.DailyMaxEntries)
	}
}

// entry 块整个缺失时走默认值。默认值也必须是 0 ——
// 否则「键名拼错 / 块被删掉」会让限制悄悄复活，正是这次要消灭的现象。
func TestDefaultEntryLimitsAreUnlimited(t *testing.T) {
	d := defaultConfig()
	if d.Entry == nil {
		t.Fatal("默认配置里 entry 不应为 nil")
	}
	if d.Entry.MaxConcurrentPositions != 0 || d.Entry.DailyMaxEntries != 0 {
		t.Fatalf("默认值必须是 0（不限），实际 %d / %d",
			d.Entry.MaxConcurrentPositions, d.Entry.DailyMaxEntries)
	}

	// 连 entry 都没有的配置，走完归一化后也应当是不限。
	c := &Config{}
	fillDefaults(c)
	if c.Entry.MaxConcurrentPositions != 0 || c.Entry.DailyMaxEntries != 0 {
		t.Fatalf("entry 缺失时也应当不限，实际 %d / %d",
			c.Entry.MaxConcurrentPositions, c.Entry.DailyMaxEntries)
	}
}

// 冷却（cooldown_bars）这次没动，仍然是「同一合约 6 根内不重复开仓」。
// 写这个断言是为了说明它是**刻意保留**的，不是漏改 ——
// 它限制的是「同一合约反复开」，不是「总持仓数 / 总笔数」。
func TestCooldownUnchanged(t *testing.T) {
	c := &Config{Entry: &EntryCfg{CooldownBars: 6}}
	fillDefaults(c)
	if c.Entry.CooldownBars != 6 {
		t.Fatalf("cooldown_bars 应为 6，实际 %d", c.Entry.CooldownBars)
	}
}

// ---------------------------------------------------------------------------
// 2026-10-01 二期：加仓次数 / K 线保留窗口 / 周期名单
// ---------------------------------------------------------------------------

// TestAddonMaxTimesZeroMeansUnlimited 加仓次数是同一类「计数/开关」字段：
// **<= 0 = 不限**（用户口径「加仓没有任何限制」）。
//
// 这是同一个坑的第三次：一期在 max_concurrent_positions / daily_max_entries 上踩过，
// 归一化里写 `<= 0 → 兜底回默认值`，于是「配置里写 0」被反压回 3，看起来改了实际还在拦。
// 这条测试保证它不会再回来。
func TestAddonMaxTimesZeroMeansUnlimited(t *testing.T) {
	c := &Config{Addon: &AddonCfg{MaxTimes: 0}}
	fillDefaults(c)
	if c.Addon.MaxTimes != 0 {
		t.Fatalf("max_times 写 0（=不限）被改成了 %d，加仓次数限制会复活", c.Addon.MaxTimes)
	}
	// 负数归 0（显示干净，判定只看 > 0）
	c = &Config{Addon: &AddonCfg{MaxTimes: -5}}
	fillDefaults(c)
	if c.Addon.MaxTimes != 0 {
		t.Fatalf("max_times 负数应归 0（不限），实际 %d", c.Addon.MaxTimes)
	}
	// 写正数仍然生效 —— 「不限」不等于「再也设不回来」
	c = &Config{Addon: &AddonCfg{MaxTimes: 3}}
	fillDefaults(c)
	if c.Addon.MaxTimes != 3 {
		t.Fatalf("max_times 写 3 变成了 %d", c.Addon.MaxTimes)
	}
	// 默认值本身也必须是 0
	if d := defaultConfig(); d.Addon.MaxTimes != 0 {
		t.Fatalf("默认 max_times 必须是 0（不限），实际 %d", d.Addon.MaxTimes)
	}
}

// TestAddonRiseBarDefaultsToAuto 加仓周期默认 "auto"（= 用该仓位自己的周期）。
//
// 四个周期都参与开仓之后，加仓必须按「开仓时那个周期」判，
// 否则会拿 A 周期的信号去加 B 周期的仓，而且不报错。
func TestAddonRiseBarDefaultsToAuto(t *testing.T) {
	if d := defaultConfig(); d.Addon.RiseBar != AddonAutoBar {
		t.Fatalf("默认 rise_bar 应为 %q，实际 %q", AddonAutoBar, d.Addon.RiseBar)
	}
	c := &Config{Addon: &AddonCfg{RiseBar: ""}}
	fillDefaults(c)
	if c.Addon.RiseBar != AddonAutoBar {
		t.Fatalf("rise_bar 留空应补成 %q，实际 %q", AddonAutoBar, c.Addon.RiseBar)
	}
}

// TestKlineRetainDefaultsTo10Days K 线保留窗口默认 10 天（用户口径
// 「只能查询保存最近 10 天数据，不能多，多出来就删除」）。
func TestKlineRetainDefaultsTo10Days(t *testing.T) {
	d := defaultConfig()
	if d.Store == nil || d.Store.KlineRetainDays != 10 {
		t.Fatalf("默认 kline_retain_days 应为 10，实际 %+v", d.Store)
	}
	// 记录表仍是独立的 30 天红线，不能被顺手改掉
	if d.Store.RetainDays != 30 {
		t.Fatalf("记录表 retain_days 应为 30（与 K 线分开），实际 %d", d.Store.RetainDays)
	}
	// KlineRetainDays 没写时兜底 10；写了就用写的
	c := &Config{Store: &StoreCfg{KlineRetainDays: 0, KeepKlineDays: 0}}
	fillDefaults(c)
	if c.Store.KlineRetainDays != 10 {
		t.Fatalf("kline_retain_days 缺失时应兜底 10，实际 %d", c.Store.KlineRetainDays)
	}
	// 老配置兼容：只写了废弃的 keep_kline_days 时沿用它（这是有意保留的降级路径，
	// 因为老配置文件里可能只有这一个键）。
	c = &Config{Store: &StoreCfg{KlineRetainDays: 0, KeepKlineDays: 30}}
	fillDefaults(c)
	if c.Store.KlineRetainDays != 30 {
		t.Fatalf("老键 keep_kline_days 应被沿用为 30，实际 %d", c.Store.KlineRetainDays)
	}
	// 两者都写了 → 新键优先
	c = &Config{Store: &StoreCfg{KlineRetainDays: 10, KeepKlineDays: 30}}
	fillDefaults(c)
	if c.Store.KlineRetainDays != 10 {
		t.Fatalf("新键应优先于老键，实际 %d", c.Store.KlineRetainDays)
	}
	c = &Config{Store: &StoreCfg{KlineRetainDays: 14}}
	fillDefaults(c)
	if c.Store.KlineRetainDays != 14 {
		t.Fatalf("显式写 14 应保留，实际 %d", c.Store.KlineRetainDays)
	}
}

// TestBarsDefaultsCoverFourPeriods 默认周期名单 = 1m/3m/5m/15m，四个都参与扫描开仓。
func TestBarsDefaultsCoverFourPeriods(t *testing.T) {
	want := []string{"1m", "3m", "5m", "15m"}
	d := defaultConfig()
	if len(d.BarsEnabled) != len(want) {
		t.Fatalf("默认 bars_enabled 应为 %v，实际 %v", want, d.BarsEnabled)
	}
	for i, b := range want {
		if d.BarsEnabled[i] != b {
			t.Fatalf("默认 bars_enabled[%d] 应为 %q，实际 %q（整体 %v）", i, b, d.BarsEnabled[i], d.BarsEnabled)
		}
	}
	for _, b := range want {
		if !d.BarEnabled(b) {
			t.Fatalf("默认配置里 %q 应当允许扫描开仓", b)
		}
		if !model.BarEnabled(b) {
			t.Fatalf("model.EnabledBars 里应当包含 %q", b)
		}
	}
	// 没写这个键时也走默认（4 个周期）
	c := &Config{}
	fillDefaults(c)
	if len(c.BarsEnabled) != len(want) {
		t.Fatalf("bars_enabled 缺失时应兜底 4 个周期，实际 %v", c.BarsEnabled)
	}
}

// TestScoreThresholdDefaultsToFour 买入/加仓阈值默认 4 —— 等价于用户说的「score > 3」。
//
// 判定处是 `score >= score_threshold`，而 Score 是 0~8 的整数，
// 所以「> 3」与「>= 4」完全等价。用 4 表达就不必把判定改成严格大于
// （改了会让所有历史口径跟着漂）。
func TestScoreThresholdDefaultsToFour(t *testing.T) {
	d := defaultConfig()
	if d.ScoreThreshold != 4 {
		t.Fatalf("默认 score_threshold 应为 4（等价 score>3），实际 %d", d.ScoreThreshold)
	}
	// 兜底里不给 BTC/ETH 单独放宽 —— 用户要的是「全市场同一个阈值」
	if len(d.ScoreThresholdMap) != 0 {
		t.Fatalf("默认 score_threshold_map 应为空，实际 %v", d.ScoreThresholdMap)
	}
	// 没写阈值时走 4；显式写的仍然生效
	c := &Config{}
	fillDefaults(c)
	if c.ThresholdFor("BTC-USDT-SWAP") != 4 {
		t.Fatalf("阈值缺失时应为 4，实际 %d", c.ThresholdFor("BTC-USDT-SWAP"))
	}
	c = &Config{ScoreThreshold: 6}
	fillDefaults(c)
	if c.ThresholdFor("BTC-USDT-SWAP") != 6 {
		t.Fatalf("显式写 6 应保留，实际 %d", c.ThresholdFor("BTC-USDT-SWAP"))
	}
	// 超过 8 的会被夹到 8（因子只有 8 个）
	c = &Config{ScoreThreshold: 99}
	fillDefaults(c)
	if c.ScoreThreshold != 8 {
		t.Fatalf("超过 8 的阈值应夹到 8，实际 %d", c.ScoreThreshold)
	}
}

// TestEntryMarginDefaultsTo01USDT 单笔目标保证金默认 0.1U，硬顶 1U。
func TestEntryMarginDefaultsTo01USDT(t *testing.T) {
	d := defaultConfig()
	if d.Entry.MarginUSDT != 0.1 {
		t.Fatalf("默认 margin_usdt 应为 0.1，实际 %v", d.Entry.MarginUSDT)
	}
	if d.Entry.MaxMarginUSDT != 1.0 {
		t.Fatalf("默认 max_margin_usdt 应为 1.0，实际 %v", d.Entry.MaxMarginUSDT)
	}
	if d.MaxOrderMarginUSDT != 1.0 {
		t.Fatalf("默认 max_order_margin_usdt 应为 1.0，实际 %v", d.MaxOrderMarginUSDT)
	}
	// 0.1 是「有效的小正数」，不能被归一化当成「没填」而回落到别的值
	c := &Config{Entry: &EntryCfg{MarginUSDT: 0.1, MaxMarginUSDT: 1.0}}
	fillDefaults(c)
	if c.Entry.MarginUSDT != 0.1 {
		t.Fatalf("margin_usdt=0.1 被改成了 %v", c.Entry.MarginUSDT)
	}
}

// TestMinBarRisePctThreeStates 「这根 K 线必须真涨」的三种状态必须泾渭分明。
//
//	min_bar_rise_pct 用**指针**，为的就是区分「没写」与「写了 0」：
//	  没写   → 默认 1.0（漏配时条件仍在，不会静默放开全市场下单）
//	  写 0   → 真的关掉这个条件
//	  写负数 → 视为写错，按默认 1.0
//
// 为什么值得单独守：二期在 max_concurrent_positions / daily_max_entries 上
// 正是栽在「用户写的 0 被 `<= 0` 反压回默认值」—— 配置看起来改了、其实没生效。
// 若这里退化成值类型 + `<=0 兜底`，同一个坑会原样复现。
func TestMinBarRisePctThreeStates(t *testing.T) {
	// ① 没写（nil）→ 默认 1.0
	d := defaultConfig()
	if d.MinBarRisePct() != 1.0 {
		t.Fatalf("默认 min_bar_rise_pct 应为 1.0，实际 %v", d.MinBarRisePct())
	}
	c := &Config{}
	fillDefaults(c)
	if c.MinBarRisePct() != 1.0 {
		t.Fatalf("键缺失时应为 1.0，实际 %v", c.MinBarRisePct())
	}
	// ② 显式写 0 → 关闭条件（绝不能被反压回 1.0）
	zero := 0.0
	c = &Config{Entry: &EntryCfg{MinBarRisePct: &zero}}
	fillDefaults(c)
	if c.MinBarRisePct() != 0 {
		t.Fatalf("min_bar_rise_pct=0（显式关闭）被改成了 %v，条件会静默复活", c.MinBarRisePct())
	}
	// ③ 显式写正数 → 原样生效
	v := 2.5
	c = &Config{Entry: &EntryCfg{MinBarRisePct: &v}}
	fillDefaults(c)
	if c.MinBarRisePct() != 2.5 {
		t.Fatalf("min_bar_rise_pct=2.5 应原样保留，实际 %v", c.MinBarRisePct())
	}
	// ④ 负数 = 写错 → 按默认 1.0（既不能变成「关闭」，也不能倒扣）
	neg := -3.0
	c = &Config{Entry: &EntryCfg{MinBarRisePct: &neg}}
	fillDefaults(c)
	if c.MinBarRisePct() != 1.0 {
		t.Fatalf("负数应按默认 1.0 处理，实际 %v", c.MinBarRisePct())
	}
	// ⑤ Entry 整块缺失也不能 panic，同样退回默认
	c = &Config{Entry: nil}
	if c.MinBarRisePct() != 1.0 {
		t.Fatalf("Entry 为 nil 时应退回 1.0，实际 %v", c.MinBarRisePct())
	}
}

// TestExcludeStockETFDefaultsOff 三期取消了「不买美股/ETF/商品」的过滤。
//
// 用户口径：「取消美股 etf 不做的功能，只要买入上限小于 1U 就做」。
// 这是**兜底默认值**，必须与 service.DefaultUniversePolicy 一致 ——
// 两处只要有一处留着 true，配置读不到时这条被取消的规则就会悄悄复活。
func TestExcludeStockETFDefaultsOff(t *testing.T) {
	if defaultConfig().ExcludeStockETF {
		t.Fatalf("默认 exclude_stock_etf 应为 false（三期已取消该过滤）")
	}
}
