package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"finally-main/internal/conf"
)

// TestRealConfig_WriteNeverCorrupts 用**真源配置**跑一次写回：
// 真源里根级 score_threshold(64) 与 addon.score_threshold(194) 重名、
// entry.margin_usdt(88) 与 addon.margin_usdt(201) 重名 ——
// 这正是「定点替换」最容易翻车的形状。fixture 只能证明逻辑，
// 真源才能证明**没写坏**。
func TestRealConfig_WriteNeverCorrupts(t *testing.T) {
	src := filepath.Join("..", "..", "configs", "okx_strategy.json")
	if _, err := os.Stat(src); err != nil {
		t.Skip("真源配置不存在，跳过")
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	orig := string(raw)
	// 注释行数（写回后必须一致）
	beforeCmt := strings.Count(orig, "//")

	d := t.TempDir()
	p := d + "/okx_strategy.json"
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	w := NewStrategyWriter(p, nil)

	changed, err := w.Apply(&Patch{
		ScoreThreshold: iptr(4),
		MinBarRisePct:  fptr(-0.8),
		BuyMarginUSDT:  fptr(0.2),
		MaxMarginUSDT:  fptr(1.0),
		AddonMode:      sptr("price"),
		AddonScore:     iptr(3),
		AddonBarRise:   fptr(0.9),
		AddonDropPct:   fptr(1.5),
		AddonRisePct:   fptr(1.2),
		AddonMarginU:   fptr(0.15),
		AddonRatio:     fptr(0.25),
	})
	if err != nil {
		t.Fatalf("对真源写回失败（说明真源会写坏）：%v", err)
	}

	after := readBack(t, p)
	if got := strings.Count(after, "//"); got != beforeCmt {
		t.Fatalf("真源注释条数变了：%d → %d", beforeCmt, got)
	}
	// 行数必须一致（定点替换不该增删行）
	if a, b := strings.Count(orig, "\n"), strings.Count(after, "\n"); a != b {
		t.Fatalf("真源行数变了：%d → %d", a, b)
	}
	// 解析必然成功（Apply 内部已校验，这里再独立确认一次）
	cfg, err := LoadStrategy(p)
	if err != nil {
		t.Fatalf("写回后真源解析失败：%v", err)
	}
	if cfg.ScoreThreshold != 4 {
		t.Fatalf("根级买入门槛 期望 4 实际 %d", cfg.ScoreThreshold)
	}
	if cfg.Entry.MinBarRisePct == nil || *cfg.Entry.MinBarRisePct != -0.8 {
		t.Fatalf("entry.min_bar_rise_pct 期望 -0.8 实际 %v", cfg.Entry.MinBarRisePct)
	}
	if cfg.Entry.MarginUSDT != 0.2 {
		t.Fatalf("entry.margin_usdt 期望 0.2 实际 %v", cfg.Entry.MarginUSDT)
	}
	if cfg.Addon.Mode != "price" {
		t.Fatalf("addon.mode 期望 price 实际 %q", cfg.Addon.Mode)
	}
	if cfg.Addon.ScoreThres != 3 {
		t.Fatalf("addon.score_threshold 期望 3 实际 %d（若为 4 说明改串到根级了）", cfg.Addon.ScoreThres)
	}
	if cfg.Addon.DropPct != 1.5 || cfg.Addon.BarRisePct != 0.9 {
		t.Fatalf("加仓门槛写错：drop=%v barRise=%v", cfg.Addon.DropPct, cfg.Addon.BarRisePct)
	}
	if cfg.Addon.MarginUSDT != 0.15 || cfg.Addon.Ratio != 0.25 {
		t.Fatalf("加仓金额/比例写错：%v / %v", cfg.Addon.MarginUSDT, cfg.Addon.Ratio)
	}
	t.Logf("✓ 真源配置写回无损坏：%d 项落盘、注释 %d 条不变、行数不变、逐项读回一致",
		len(changed), beforeCmt)
	for _, c := range changed {
		t.Logf("    · %s", c)
	}
}

// TestRealConfig_TenPhaseValues 直接读**仓库里那份真源**，断言它就是十期口径。
//
// 为什么值得单独测一次：
//
//	十期改了四个副本（okx_strategy.json / .example.json / conf.defaultConfig /
//	service.LoadStrategy 的 def）。单测能证明"默认值构造器对了"，但证明不了
//	**磁盘上那份 JSON 也对了** —— 而引擎线上读的恰恰是磁盘这份。
//	漏改 JSON 的症状是：单测全绿、配置文件里还是旧值、用户改了页面也不生效。
//
//	所以这条测试不看任何构造器，只读 configs/okx_strategy.json，
//	用跟引擎同一条 LoadStrategy 解析，逐项比对。
func TestRealConfig_TenPhaseValues(t *testing.T) {
	src := filepath.Join("..", "..", "configs", "okx_strategy.json")
	if _, err := os.Stat(src); err != nil {
		t.Skip("真源配置不存在，跳过")
	}
	cfg, err := LoadStrategy(src)
	if err != nil {
		t.Fatalf("真源配置解析失败：%v", err)
	}

	// —— ③ 开仓闸门：冷却下线、改成持仓数量 ——
	//   十期口径是 30；用户后来在管理台把上限调到 80（真源为准），
	//   二十一期把断言同步到 80 —— 真源漂移要改测试而不是改回配置。
	if cfg.Entry.CooldownBars != 0 {
		t.Errorf("十期：entry.cooldown_bars 应为 0（冷却已删除），实际 %d —— "+
			"非 0 会让 trader.go 重新拦「冷却中（距上次开仓不足 N 根）」",
			cfg.Entry.CooldownBars)
	}
	if cfg.Entry.MaxConcurrentPositions != 80 {
		t.Errorf("持仓数量上限应为 80（用户在管理台调整后的真源值），实际 %d",
			cfg.Entry.MaxConcurrentPositions)
	}

	// —— ⑤ 加仓：价格模式 + 共振分，跌 2% + 该根涨 0.4%，金额 0.1U，当前停用 ——
	//   ★ 二十二期（2026-10-03）：「先不用加仓了 先要买入准确」→ enabled=false；
	//     参数已按新口径配好（score_threshold=3 / drop=2 / rise=0.4 / margin=0.1），
	//     想开仓时把 enabled 改回 true 即可，参数不用重填。
	if cfg.Addon.Mode != conf.AddonModePrice {
		t.Errorf("二十二期：addon.mode 应为 %q，实际 %q", conf.AddonModePrice, cfg.Addon.Mode)
	}
	if cfg.Addon.Enabled {
		t.Errorf("二十二期：addon.enabled 应为 false（先停用加仓，专注买入准确），实际 true")
	}
	if cfg.Addon.ScoreThres != 3 {
		t.Errorf("二十二期：addon.score_threshold 应为 3（score > 3 = 共振大于 3），实际 %d", cfg.Addon.ScoreThres)
	}
	if cfg.Addon.DropPct != 2.0 {
		t.Errorf("二十二期：addon.drop_pct 应为 2（跌破买价 2%%），实际 %v", cfg.Addon.DropPct)
	}
	if cfg.Addon.PriceRise != 0.4 {
		t.Errorf("二十二期：addon.price_rise_pct 应为 0.4，实际 %v", cfg.Addon.PriceRise)
	}
	if cfg.Addon.MarginUSDT != 0.1 {
		t.Errorf("二十二期：addon.margin_usdt 应为 0.1（买入和加仓都是 0.1 美金），实际 %v", cfg.Addon.MarginUSDT)
	}
	// 共振模式的参数要**留着**，切回 resonance 时不用重填（八期的坑：
	// 两套参数共用键会互相污染，所以是两套独立的键，谁不生效就留着不删）。
	if cfg.Addon.BarRisePct != 0.7 {
		t.Errorf("十期：addon.bar_rise_pct 应保留 0.7（共振模式参数），实际 %v", cfg.Addon.BarRisePct)
	}

	// —— ⑥ 二十二期买入区间：min_bar_rise_pct=-1 且 max_bar_drop_pct=2 ——
	if cfg.Entry.MinBarRisePct == nil || *cfg.Entry.MinBarRisePct != -1.0 {
		t.Errorf("二十二期：entry.min_bar_rise_pct 应为 -1（必须跌超 1%%），实际 %v", cfg.Entry.MinBarRisePct)
	}
	if cfg.Entry.MaxBarDropPct == nil || *cfg.Entry.MaxBarDropPct != 2.0 {
		t.Errorf("二十二期：entry.max_bar_drop_pct 应为 2（跌幅必须小于 2%%），实际 %v", cfg.Entry.MaxBarDropPct)
	}

	t.Logf("✓ 真源配置 == 二十二期口径：cooldown=%d / max_pos=%d / "+
		"min_rise=%.1f max_drop=%.1f / addon(mode=%s enabled=%v score>%d drop=%.1f rise=%.1f margin=%.2f) bar_rise=%.2f保留",
		cfg.Entry.CooldownBars, cfg.Entry.MaxConcurrentPositions,
		*cfg.Entry.MinBarRisePct, *cfg.Entry.MaxBarDropPct,
		cfg.Addon.Mode, cfg.Addon.Enabled, cfg.Addon.ScoreThres,
		cfg.Addon.DropPct, cfg.Addon.PriceRise, cfg.Addon.MarginUSDT, cfg.Addon.BarRisePct)
}
