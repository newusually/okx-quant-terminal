package service

import (
	"testing"

	"finally-main/internal/conf"
)

// 这两条测试守的是「准入过滤」与「真实下单」必须用同一个杠杆口径。
//
// 踩过的坑（2026-10-01）：universe.go 算「这笔买不买得起」用的是
// effLever(合约上限, 策略杠杆)，而 calcSizeWith / SetLeverage 用的是裸 cfg.Entry.Leverage。
// 结果是一批 OKX 上限只有 10x 的合约（成交额前 80 里就有 4 个：USELESS / CAP / ONE / PROS）
// 准入放行、下单却被 OKX 拒（59102 Leverage exceeds the maximum limit），
// 紧接着下单 code=1 全失败 —— 这些合约**永远买不进来**，日志里只有一条 WARN。
// 用户看到的现象就是「买得太少」。
//
// 所以这里断言的不是某个具体数字，而是**两条路必须一致**。

func calcTestConfig(lever int) *conf.Config {
	margin := 1.0
	maxMargin := 1.5
	return &conf.Config{
		Bar: "15m",
		Entry: &conf.EntryCfg{
			TdMode: "cross", PosSide: "net", OrdType: "market",
			MarginUSDT: margin, Leverage: lever,
			MarginPolicy: "min_one", MaxMarginUSDT: maxMargin,
		},
	}
}

// 合约上限低于配置时，张数必须按合约上限算，不能按配置的 20x 算。
func TestCalcSizeClampsLeverToInstrumentMax(t *testing.T) {
	cfg := calcTestConfig(20)
	// 一张名义 4U，价格 1，ctVal 4 → 每张 4U
	ins := Instrument{InstID: "X-USDT-SWAP", CtVal: 4, CtMult: 1, LotSz: 1, MinSz: 1, Lever: 10, TickSz: 0.01}
	const price = 1.0

	sz, margin, err := calcSize(cfg, ins, price)
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	// 1U × 10x = 10U 名义 ÷ 每张 4U = 2 张
	if sz != 2 {
		t.Fatalf("按合约上限 10x 应下 2 张，实际 %v 张（说明还在用配置的 20x）", sz)
	}
	if margin > cfg.Entry.MaxMarginUSDT+1e-9 {
		t.Fatalf("保证金 %.4f 超过上限 %.2f", margin, cfg.Entry.MaxMarginUSDT)
	}
}

// 合约上限高于配置时，仍然按配置的杠杆算（收敛只降不升）。
func TestCalcSizeKeepsPolicyLeverWhenInstrumentAllowsMore(t *testing.T) {
	cfg := calcTestConfig(20)
	ins := Instrument{InstID: "X-USDT-SWAP", CtVal: 4, CtMult: 1, LotSz: 1, MinSz: 1, Lever: 100, TickSz: 0.01}

	sz, _, err := calcSize(cfg, ins, 1.0)
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	// 1U × 20x = 20U 名义 ÷ 每张 4U = 5 张
	if sz != 5 {
		t.Fatalf("合约给 100x 时仍应按配置 20x 算（5 张），实际 %v 张", sz)
	}
}

// 合约没给 lever（=0，数据缺失）时不能把杠杆压成 0 —— 那是除零。
func TestCalcSizeHandlesMissingInstrumentLever(t *testing.T) {
	cfg := calcTestConfig(20)
	ins := Instrument{InstID: "X-USDT-SWAP", CtVal: 4, CtMult: 1, LotSz: 1, MinSz: 1, Lever: 0, TickSz: 0.01}

	sz, _, err := calcSize(cfg, ins, 1.0)
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if sz != 5 {
		t.Fatalf("lever 缺失时应回落到配置的 20x（5 张），实际 %v 张", sz)
	}
}

// calcSizeWith 与准入过滤用的 effLever 必须是同一个值 —— 这条就是「两条路一致」的守门测试。
func TestCalcSizeLeverMatchesAdmissionLever(t *testing.T) {
	for _, instLever := range []int{0, 1, 5, 10, 20, 50, 100} {
		for _, policyLever := range []int{1, 10, 20, 50} {
			want := float64(effLever(instLever, policyLever))
			cfg := calcTestConfig(policyLever)
			// 找一组能算出 sz≥1 的参数更容易观察到差异：每张名义 = 1，margin = want
			ins := Instrument{InstID: "X-USDT-SWAP", CtVal: 1, CtMult: 1, LotSz: 1, MinSz: 1,
				Lever: instLever, TickSz: 0.01}
			sz, _, err := calcSizeWith(cfg, ins, 1.0, want, 0, "fixed")
			if err != nil {
				t.Fatalf("lever=%d winner=%d：不该报错 %v", instLever, policyLever, err)
			}
			// margin = want、每张名义 = 1 → sz = want × want / 1 = want²（张数向下取整）
			if exp := want * want; sz != exp {
				t.Fatalf("lever=%d policy=%d 用倍率 %.0f 应得 %v 张，实际 %v 张（下单与准入口径不一致）",
					instLever, policyLever, want, exp, sz)
			}
		}
	}
}
