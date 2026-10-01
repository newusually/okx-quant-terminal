package conf

import "testing"

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
