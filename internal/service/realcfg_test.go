package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
