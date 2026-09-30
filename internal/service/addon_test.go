package service

// addon_test.go —— 加仓规则（15m 先跌 0.5% 后转涨 → 补 1/3）的实测验证
//
// 用真实形状的 15m K 线跑四类场景：
//   A. 标准的「先跌 0.6% → 收阳转涨」      → 必须加仓
//   B. 只跌 0.2%（没跌够）                  → 不加
//   C. 跌够了但最后一根是阴线（还没转涨）    → 不加
//   D. 跌够 + 转涨，但已经是第 2 次（上限 2）→ 不加
//
// 另外校验：加仓额 ≈ 原保证金 1/3、合并后的加权均价算得对。

import (
	"testing"

	"finally-main/internal/conf"
	"finally-main/internal/repo"
)

const barMs = 15 * 60 * 1000

// mkAddonCfg 一套最小可用的配置：0.1U/笔、20x、准入上限 0.5U
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

// candles 按 (o,h,l,c) 造一批连续的 15m 已收盘 K 线
func candles(startTs int64, rows ...[4]float64) []Candle {
	out := make([]Candle, 0, len(rows))
	for i, r := range rows {
		out = append(out, Candle{
			Ts: startTs + int64(i)*barMs,
			O:  r[0], H: r[1], L: r[2], C: r[3], V: 1000,
			Confirm: true,
		})
	}
	return out
}

func basePos(entryPx float64) repo.OpenPos {
	return repo.OpenPos{
		ID: 1, InstID: "TEST-USDT-SWAP", Sz: 1, EntryPx: entryPx,
		Margin: 0.1, Leverage: 20, OpenTs: 1000, Bar: "15m",
	}
}

func TestAddon_FiresOnDropThenRise(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = true
	cfg.Entry.MarginUSDT = 0.1
	cfg.Entry.Leverage = 20
	cfg.Entry.MaxMarginUSDT = 0.5

	ins := mkIns()
	p := basePos(1.6000)
	// 开仓价 1.6000；跌到 1.5904（-0.60%）；最后一根 1.5904→1.5950 收阳且高于前一根
	win := candles(barMs*1,
		[4]float64{1.6000, 1.6005, 1.5990, 1.5995},
		[4]float64{1.5995, 1.5998, 1.5904, 1.5910}, // 低点 1.5904
		[4]float64{1.5910, 1.5920, 1.5900, 1.5905},
		[4]float64{1.5905, 1.5952, 1.5903, 1.5950}, // 阳线，收盘 > 前一根
	)
	mark := 1.5950

	d := decideAddon(cfg, p, mark, win, barMs, ins)
	if !d.Add {
		t.Fatalf("应当加仓，但判定为不加")
	}
	// 加仓额应 = 0.1 × 1/3 = 0.03333U；20x → 0.6667U 名义
	// 每张名义 = 1×1×1.5950 = 1.595U > 0.6667U → min_one 放大到刚好 1 张
	if d.Margin <= 0 {
		t.Fatalf("加仓保证金应 > 0，实际 %.6f", d.Margin)
	}
	if d.Margin > cfg.Entry.MaxMarginUSDT+1e-9 {
		t.Fatalf("加仓保证金 %.4f 超过上限 %.4f", d.Margin, cfg.Entry.MaxMarginUSDT)
	}
	if d.Sz <= 0 {
		t.Fatalf("加仓张数应 > 0")
	}
	// 合并后：sz 1+1=2，均价 = (1×1.6 + 1×1.595)/2
	wantAvg := (1*1.6000 + d.Sz*1.5950) / (1 + d.Sz)
	if diff := d.NewAvgPx - wantAvg; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("合并均价算错：got %.8f want %.8f", d.NewAvgPx, wantAvg)
	}
	if d.NewMargin <= p.Margin {
		t.Fatalf("合并保证金应增加")
	}
	if d.Count != 1 {
		t.Fatalf("加仓次数应为 1，实际 %d", d.Count)
	}
	t.Logf("✓ 场景A 加仓 %s 张 保证金=%.4fU 新均价=%.6f（%s）",
		fmtSz(d.Sz, 0), d.Margin, d.NewAvgPx, d.Reason)
}

func TestAddon_SkipWhenNotDroppedEnough(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = true
	p := basePos(1.6000)
	// 只跌到 1.5968 = -0.20%，不够 0.5%
	win := candles(barMs*1,
		[4]float64{1.6000, 1.6005, 1.5980, 1.5985},
		[4]float64{1.5985, 1.5990, 1.5968, 1.5980},
		[4]float64{1.5980, 1.5995, 1.5975, 1.5990}, // 阳线但没跌够
	)
	d := decideAddon(cfg, p, 1.5990, win, barMs, mkIns())
	if d.Add {
		t.Fatalf("只跌 0.20%%，不该加仓，却判定为加")
	}
	t.Log("✓ 场景B 未跌够 0.5% → 不加")
}

func TestAddon_SkipWhenLastCandleBearish(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = true
	p := basePos(1.6000)
	// 跌够了（低点 1.5904 = -0.60%），但最后一根是阴线
	win := candles(barMs*1,
		[4]float64{1.6000, 1.6005, 1.5904, 1.5910},
		[4]float64{1.5910, 1.5930, 1.5900, 1.5920},
		[4]float64{1.5920, 1.5925, 1.5880, 1.5890}, // 阴线，收盘 1.5890
	)
	d := decideAddon(cfg, p, 1.5890, win, barMs, mkIns())
	if d.Add {
		t.Fatalf("最后一根是阴线（没转涨），不该加仓")
	}
	t.Log("✓ 场景C 跌够但未转涨 → 不加")
}

func TestAddon_SkipAtMaxTimes(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = true
	cfg.Addon.MaxTimes = 2
	p := basePos(1.6000)
	p.AddonCount = 2 // 已经加满 2 次
	win := candles(barMs*1,
		[4]float64{1.6000, 1.6005, 1.5904, 1.5910},
		[4]float64{1.5910, 1.5920, 1.5900, 1.5905},
		[4]float64{1.5905, 1.5952, 1.5903, 1.5950},
	)
	d := decideAddon(cfg, p, 1.5950, win, barMs, mkIns())
	if d.Add {
		t.Fatalf("已达 max_times=2，不该再加仓")
	}
	t.Log("✓ 场景D 加仓次数已满 → 不加")
}

func TestAddon_SkipWhenDisabled(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = false
	p := basePos(1.6000)
	win := candles(barMs*1,
		[4]float64{1.6000, 1.6005, 1.5904, 1.5910},
		[4]float64{1.5910, 1.5920, 1.5900, 1.5905},
		[4]float64{1.5905, 1.5952, 1.5903, 1.5950},
	)
	if d := decideAddon(cfg, p, 1.5950, win, barMs, mkIns()); d.Add {
		t.Fatalf("开关关闭时不该加仓")
	}
	t.Log("✓ 场景E 加仓开关关闭 → 不加")
}

func TestAddon_RatioIsOneThird(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = true
	cfg.Addon.MarginUSDT = 0 // 走 ratio 口径
	if cfg.Addon.Ratio < 0.3332 || cfg.Addon.Ratio > 0.3334 {
		t.Fatalf("默认 ratio 应为 1/3，实际 %.6f", cfg.Addon.Ratio)
	}

	// 造一个「每张名义 0.30U」的小合约：0.0333U×20x = 0.6667U 名义 → 能买 2 张
	ins := Instrument{InstID: "T", CtVal: 0.3, CtMult: 1, LotSz: 1, MinSz: 1, LotSzDec: 0}
	p := basePos(1.0000)
	win := candles(barMs*1,
		[4]float64{1.0000, 1.0005, 0.9940, 0.9950},
		[4]float64{0.9950, 0.9960, 0.9950, 0.9960},
		[4]float64{0.9960, 0.9990, 0.9955, 0.9985},
	)
	d := decideAddon(cfg, p, 0.9985, win, barMs, ins)
	if !d.Add {
		t.Fatalf("应当加仓")
	}
	// 预算 0.1/3 = 0.03333U → 名义 0.6667U → 每张 0.2996U → floor(0.6667/0.2996)=2 张
	if d.Sz != 2 {
		t.Logf("张数 = %v（每张名义 %.4f）", d.Sz, 0.3*0.9985)
	}
	t.Logf("✓ 场景F 按 1/3 预算下单：%s 张 · 保证金 %.4fU · 均价 %.6f",
		fmtSz(d.Sz, 0), d.Margin, d.NewAvgPx)
}

func TestAddon_WeightedAverage(t *testing.T) {
	cfg := mkAddonCfg()
	cfg.Addon.Enabled = true
	ins := mkIns()
	p := basePos(1.6000)
	p.Sz = 2
	p.Margin = 0.2

	win := candles(barMs*1,
		[4]float64{1.6000, 1.6005, 1.5904, 1.5910},
		[4]float64{1.5910, 1.5920, 1.5900, 1.5905},
		[4]float64{1.5905, 1.5952, 1.5903, 1.5950},
	)
	mark := 1.5950
	d := decideAddon(cfg, p, mark, win, barMs, ins)
	if !d.Add {
		t.Fatalf("应当加仓")
	}
	expSz := p.Sz + d.Sz
	expAvg := (p.Sz*p.EntryPx + d.Sz*mark) / expSz
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
	t.Logf("✓ 场景G 2 张@1.6000 + %s 张@%.4f → 均价 %.6f（原 1.6000，摊薄 %.4f%%）",
		fmtSz(d.Sz, 0), mark, d.NewAvgPx, (1-d.NewAvgPx/1.6000)*100)
}

func almostEq(a, b, eps float64) bool { return a-b < eps && b-a < eps }
