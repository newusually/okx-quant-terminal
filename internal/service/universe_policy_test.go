package service

// universe_policy_test.go —— 准入策略构造的单测。
//
// ★ 为什么专门测这个 ★
//
// 2026-10-01 二期踩了一个**完全静默**的坑：Scan() 里手写了 UniversePolicy 字面量，
// 新增 margin_policy 字段时漏填了 MarginPolicy。它一空，OrderMarginCap() 就跳过
// min_one 分支、掉回 MarginUSDT：
//
//	min_one → MaxMarginUSDT = 1.0U   （应该走这里：170 个合约都买得起）
//	掉回后 → MarginUSDT     = 0.01U  （实际走这里：只剩 33 个候选）
//
// 表现只是日志里多一句「资金不够 -137」，配置/README/面板全都还写着 1U。
// 不崩、不报错、日志干净 —— 只是策略悄悄只做 1/5 的币。
//
// 这类「同一个量被两条路算过」的 bug 只能靠测试钉住，所以：
//   · 钉死 min_one 下 cap 必须等于 maxOrderMargin（而不是 perTradeMargin）；
//   · 钉死字段为空时**不能**被解读成严格 0.01U；
//   · 钉死 fixed 口径下确实按 MarginUSDT 收紧。

import (
	"testing"

	"finally-main/internal/conf"
	"finally-main/internal/model"
)

func mkCfg(margin, maxMargin float64, policy string) *conf.Config {
	return &conf.Config{
		MaxOrderMarginUSDT: maxMargin,
		Entry: &conf.EntryCfg{
			MarginUSDT: margin, Leverage: 20,
			MarginPolicy: policy, MaxMarginUSDT: maxMargin,
		},
	}
}

// A. 本次踩坑的回归：min_one 下准入上限必须是 1U，不是 0.01U
func TestUniversePolicy_MinOneUsesMaxNotPerTrade(t *testing.T) {
	p := UniversePolicyFromConfig(mkCfg(0.01, 1.0, "min_one"))

	if got := p.OrderMarginCap(); got != 1.0 {
		t.Fatalf("min_one 下准入上限应为 1.0U（max_order_margin_usdt），实际 %.4fU —— "+
			"这就是「480 个合约只剩 33 个候选」的成因", got)
	}
	if p.MarginPolicy != "min_one" {
		t.Fatalf("MarginPolicy 必须被填上（漏填会让 cap 掉回 MarginUSDT），实际 %q", p.MarginPolicy)
	}
	if p.MarginUSDT != 0.01 {
		t.Fatalf("单笔预算应原样带过来 0.01，实际 %.4f", p.MarginUSDT)
	}
	if p.Leverage != 20 {
		t.Fatalf("杠杆应为 20，实际 %d", p.Leverage)
	}
}

// B. 字段为空不能变成「严格 0.01U」
//
// 老配置没有 margin_policy 这个键。空串要是被解读成 fixed，
// cap 就变成 0.01U，静默砍掉 4/5 的币。
func TestUniversePolicy_EmptyPolicyFallsBackToMinOne(t *testing.T) {
	p := UniversePolicyFromConfig(mkCfg(0.01, 1.0, ""))

	if p.MarginPolicy != "min_one" {
		t.Fatalf("空 margin_policy 应兜底成 min_one，实际 %q", p.MarginPolicy)
	}
	if got := p.OrderMarginCap(); got != 1.0 {
		t.Fatalf("空 margin_policy 时准入上限仍应取 max（1.0U），实际 %.4fU", got)
	}
}

// C. fixed 是「有意收紧」：这时才应该按单笔预算卡
func TestUniversePolicy_FixedUsesPerTradeMargin(t *testing.T) {
	p := UniversePolicyFromConfig(mkCfg(0.01, 1.0, "fixed"))

	if got := p.OrderMarginCap(); got != 0.01 {
		t.Fatalf("fixed 下准入上限应等于单笔预算 0.01U，实际 %.4fU", got)
	}
}

// D. max_order_margin_usdt 缺省时回落到 entry.max_margin_usdt
func TestUniversePolicy_FallsBackToEntryMax(t *testing.T) {
	cfg := mkCfg(0.01, 1.0, "min_one")
	cfg.MaxOrderMarginUSDT = 0 // 配置里没写准入上限
	p := UniversePolicyFromConfig(cfg)

	if got := p.OrderMarginCap(); got != 1.0 {
		t.Fatalf("准入上限缺省时应回落到 entry.max_margin_usdt = 1.0U，实际 %.4fU", got)
	}
}

// E. nil 配置不能 panic，且要落到兜底策略
func TestUniversePolicy_NilConfigUsesDefault(t *testing.T) {
	p := UniversePolicyFromConfig(nil)
	if p.OrderMarginCap() <= 0 {
		t.Fatalf("nil 配置应落到 DefaultUniversePolicy（cap 必须 > 0），实际 %.4f", p.OrderMarginCap())
	}
	// Entry 为 nil 时也不能 panic
	if p2 := UniversePolicyFromConfig(&conf.Config{MaxOrderMarginUSDT: 2}); p2.OrderMarginCap() != 2 {
		t.Fatalf("Entry 为 nil 时仍应取到 maxOrderMargin，实际 %.4f", p2.OrderMarginCap())
	}
}

// F. 端到端：拿真实量级的加密合约跑一遍 FilterUniverse
//
// 参数取自库里实测的分布：加密合约最小一手保证金普遍在 0.01~0.8U，
// 所以在 1U 准入下这些必须**一个都不被剔除**。
// 这一条正是「资金不够 -137」会失败的断言。
func TestUniversePolicy_OneUSDPassesCheapContracts(t *testing.T) {
	policy := UniversePolicyFromConfig(mkCfg(0.01, 1.0, "min_one"))

	insts := []model.Instrument{
		{InstID: "XLM-USDT-SWAP", BaseCcy: "XLM", CtVal: 100, CtMult: 1, LotSz: 0.1, MinSz: 0.1, Lever: 50, State: "live"},
		{InstID: "DOGE-USDT-SWAP", BaseCcy: "DOGE", CtVal: 1000, CtMult: 1, LotSz: 0.1, MinSz: 0.1, Lever: 50, State: "live"},
		{InstID: "TRX-USDT-SWAP", BaseCcy: "TRX", CtVal: 100, CtMult: 1, LotSz: 0.1, MinSz: 0.1, Lever: 50, State: "live"},
		{InstID: "SUSHI-USDT-SWAP", BaseCcy: "SUSHI", CtVal: 1, CtMult: 1, LotSz: 0.1, MinSz: 0.1, Lever: 50, State: "live"},
		{InstID: "CRV-USDT-SWAP", BaseCcy: "CRV", CtVal: 10, CtMult: 1, LotSz: 0.1, MinSz: 0.1, Lever: 50, State: "live"},
	}
	tks := map[string]model.Ticker{
		"XLM-USDT-SWAP":   {InstID: "XLM-USDT-SWAP", Last: 0.22, QuoteVol24h: 5e7},
		"DOGE-USDT-SWAP":  {InstID: "DOGE-USDT-SWAP", Last: 0.17, QuoteVol24h: 5e8},
		"TRX-USDT-SWAP":   {InstID: "TRX-USDT-SWAP", Last: 0.28, QuoteVol24h: 5e7},
		"SUSHI-USDT-SWAP": {InstID: "SUSHI-USDT-SWAP", Last: 0.26, QuoteVol24h: 5e7},
		"CRV-USDT-SWAP":   {InstID: "CRV-USDT-SWAP", Last: 0.38, QuoteVol24h: 5e7},
	}

	// 先自证夹具合理：每个合约的最小一手保证金都必须 ≤ 准入上限。
	// 不先自证的话，夹具一旦造贵了，测试失败会被误读成「准入又坏了」。
	lp := LeveragePolicy{Leverage: policy.Leverage}
	for _, it := range insts {
		tk := tks[it.InstID]
		need := MinOrderMargin(it, tk.Last, lp)
		if need > policy.OrderMarginCap() {
			t.Fatalf("测试夹具造错了：%s 最小一手 %.4fU 已超准入上限 %.4fU，"+
				"请调 ctVal/价格，别让夹具本身成为失败原因",
				it.InstID, need, policy.OrderMarginCap())
		}
	}

	kept, fst := FilterUniverse(insts, tks, nil, policy)

	if fst.DroppedNotional != 0 {
		t.Fatalf("1U 准入下这些便宜合约不该因「资金不够」被剔除，实际剔了 %d 个"+
			"（cap=%.4fU —— 掉回 0.01U 就是这个症状）", fst.DroppedNotional, policy.OrderMarginCap())
	}
	if len(kept) != len(insts) {
		t.Fatalf("应全部保留 %d 个，实际 %d 个（剔除原因：%+v）", len(insts), len(kept), fst)
	}
}
