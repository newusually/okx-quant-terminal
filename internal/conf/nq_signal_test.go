package conf

import "testing"

// nq_signal_test.go —— 只读板块 NQ 专属信号口径的**兜底值**（2026-10-02 十三期）
//
// 这里守的是「defaultConfig() 与真源 JSON 同口径」这条项目铁律：
// 配置读不到时会整体退回 defaultConfig()，如果两边不一致，
// 「配置文件读不到」就会变成另一套完全不同的行为，而且只在故障时才暴露。
//
// 对 NQ 来说后果尤其重：一旦退回全局的 entry.min_bar_rise_pct = -0.7，
// 指数上永远凑不出合格根 → **恒等于 0 条信号**，页面上什么都不显示、日志无异常。
func TestNQSignalConfigDefaults(t *testing.T) {
	n := defaultConfig().NQSignal
	if n == nil {
		t.Fatal("defaultConfig() 缺 NQSignal —— 配置读不到时 NQ 会退回全市场口径")
	}
	if !n.IsEnabled() {
		t.Error("默认必须启用（不写 enabled 就是启用）")
	}
	if n.ScoreThreshold != 4 {
		t.Errorf("默认共振门槛应为 4（用户口径「共振 4+」），实际 %d", n.ScoreThreshold)
	}
	if n.MaxRisePct != 0 {
		t.Errorf("默认涨跌幅门槛应为 0（只要收阴），实际 %v", n.MaxRisePct)
	}
}

// TestNQSignalConfigIsEnabledTriState enabled 用指针三态：不写 = 开，显式 false = 关。
// 若有人把它改成 bool，缺键就会变成"关"，NQ 会静默退回旧口径。
func TestNQSignalConfigIsEnabledTriState(t *testing.T) {
	on, off := true, false
	cases := []struct {
		name string
		cfg  *NQSignalCfg
		want bool
	}{
		{"整块缺失", nil, true}, // nil 视为启用（Config 里是 *NQSignalCfg）
		{"没写 enabled", &NQSignalCfg{ScoreThreshold: 4}, true},
		{"显式 true", &NQSignalCfg{Enabled: &on}, true},
		{"显式 false", &NQSignalCfg{Enabled: &off}, false},
	}
	for _, c := range cases {
		if got := c.cfg.IsEnabled(); got != c.want {
			t.Errorf("%s：IsEnabled() = %v，期望 %v", c.name, got, c.want)
		}
	}
}

// TestNQSignalRule_FallbackDirection 「没配到」时的兜底方向必须正确。
//
// 这块配置的存在意义就是「全局 -0.7% 在指数上恒不触发」，所以**任何**
// 读取失败/缺失的路径都不允许退回全市场口径 —— 那等于一条信号都没有，
// 且不报错。默认构造器与 NQSignalRule() 必须给出同一套值。
func TestNQSignalRule_FallbackDirection(t *testing.T) {
	off := false

	// 整块缺失 → 默认口径且启用（不是关闭）
	for _, c := range []*Config{nil, {}} {
		th, rise, ok := c.NQSignalRule()
		if !ok {
			t.Fatalf("配置缺失时必须退回默认口径并启用：cfg=%+v", c)
		}
		if th != DefaultNQSignalScoreThreshold || rise != DefaultNQSignalMaxRisePct {
			t.Fatalf("缺失时的默认值应为 %d/%.1f，实际 %d/%.1f",
				DefaultNQSignalScoreThreshold, DefaultNQSignalMaxRisePct, th, rise)
		}
	}

	// 显式关闭 → 如实返回未启用
	if _, _, ok := (&Config{NQSignal: &NQSignalCfg{Enabled: &off, ScoreThreshold: 4}}).NQSignalRule(); ok {
		t.Fatal("enabled=false 应返回 ok=false")
	}

	// defaultConfig 与 NQSignalRule 必须同口径（否则"配置读不到"= 另一套行为）
	d := defaultConfig()
	th, rise, ok := d.NQSignalRule()
	if !ok || th != DefaultNQSignalScoreThreshold || rise != DefaultNQSignalMaxRisePct {
		t.Fatalf("defaultConfig() 的口径与兜底常量不一致：%d/%.1f (ok=%v)", th, rise, ok)
	}
}

// TestNQSignalFillDefaults 归一化：
//
//	score_threshold <= 0 → 补默认 4（0 在这里没有合理语义：
//	                        「共振 0 个以上」比全市场还松，不可能是用户想要的）
//	score_threshold > 8  → 夹到 8（Score 是 0~8）
//	缺整块               → 用默认块
//	enabled=false        → 原样保留（不能被归一化改成 true）
func TestNQSignalFillDefaults(t *testing.T) {
	off := false

	c := &Config{Entry: &EntryCfg{}}
	fillDefaults(c)
	if c.NQSignal == nil || c.NQSignal.ScoreThreshold != 4 || c.NQSignal.MaxRisePct != 0 {
		t.Fatalf("缺块时应补默认 4/0，实际 %+v", c.NQSignal)
	}

	c = &Config{Entry: &EntryCfg{}, NQSignal: &NQSignalCfg{ScoreThreshold: 0}}
	fillDefaults(c)
	if c.NQSignal.ScoreThreshold != 4 {
		t.Fatalf("score_threshold=0 应补成默认 4，实际 %d", c.NQSignal.ScoreThreshold)
	}

	c = &Config{Entry: &EntryCfg{}, NQSignal: &NQSignalCfg{ScoreThreshold: 99}}
	fillDefaults(c)
	if c.NQSignal.ScoreThreshold != 8 {
		t.Fatalf("超出 8 的阈值应夹到 8，实际 %d", c.NQSignal.ScoreThreshold)
	}

	c = &Config{Entry: &EntryCfg{}, NQSignal: &NQSignalCfg{Enabled: &off, ScoreThreshold: 5}}
	fillDefaults(c)
	if c.NQSignal.IsEnabled() {
		t.Fatal("显式 enabled=false 被归一化改成了 true —— 用户关不掉这块")
	}
	if c.NQSignal.ScoreThreshold != 5 {
		t.Fatalf("显式 5 应原样保留，实际 %d", c.NQSignal.ScoreThreshold)
	}
}
