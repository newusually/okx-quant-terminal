package service

// universe.go —— 合约准入过滤（「哪些合约可以买」的唯一判定处）
//
// 三条规则，全部在这里落地，网页侧和引擎侧共用同一份逻辑：
//
//   规则 1  不买美股 / ETF / 商品
//           OKX 的 instCategory：1=加密  3=美股/ETF  4=商品(黄金/原油/白银…)
//           只留 category=1。
//
//   规则 2  不买「刚上线」的
//           合约的 listTime 距今不足 N 天（默认 30 天）直接排除。
//           新币上线初期深度差、插针多，且没有足够历史 K 线做共振判断。
//
//   规则 3  不买「要下线」的
//           来自 OKX 公告中心 announcements-delistings 解析出的名单（见 announce.go）。
//
// 另外还有两条资金/流动性约束（对应「每次买 0.1 美金、不要买多」）：
//
//   规则 4  成交额下限
//           24h 成交额低于 MinQuoteVolume24h 的不进候选池（默认 100 万 USDT）。
//           深度差的小币种滑点会吃掉本金，0.1U 的单子更经不起滑点。
//
//   规则 5  单笔保证金必须 ≤ 0.5U
//           一笔的保证金 = 张数 × ctVal × ctMult × 价格 ÷ 杠杆
//           最小张数由 minSz 决定，所以「0.5U 买得起」等价于
//               minSz × ctVal × ctMult × price ÷ lever ≤ 0.5
//           上限由 MarginPolicy 决定：
//               min_one → 上限 = MaxMarginUSDT（默认 0.5）。0.1U 买不起 1 张的，
//                         放大到刚好买 1 张来下单；超过 0.5U 的直接排除。
//               fixed   → 上限 = MarginUSDT。严格 0.1U，买不起就不买。
//           不满足的合约（例如 BTC-USDT-SWAP 单张就 830 U）直接不进候选池。

import (
	"math"
	"sort"
	"time"

	"finally-main/internal/model"
)

// minLever 兜底杠杆（合约没给 lever 时用）
const minLever = 1

// UniversePolicy 合约准入策略
type UniversePolicy struct {
	// ExcludeStockETF 排除美股 / ETF / 商品（只留 instCategory=1 的加密品种）
	ExcludeStockETF bool

	// ExcludeNewListingDays 上市不足这么多天的不买（0 = 不排除）
	ExcludeNewListingDays int

	// ExcludeDelisting 排除公告里说要下线的
	ExcludeDelisting bool

	// MarginUSDT 单笔保证金预算（每笔入金，例如 0.1）
	MarginUSDT float64

	// Leverage 计划使用的杠杆（用于算「这笔买不买得起」）
	Leverage int

	// MarginPolicy 资金口径：
	//   "fixed"   = 严格按 MarginUSDT：买不起最小一手就排除（默认，忠于「每笔都是 0.1U」）
	//   "min_one" = 允许放大到刚好买 1 张，但不超过 MaxMarginUSDT
	MarginPolicy string

	// MaxMarginUSDT 单笔保证金硬上限（例如 0.5）。min_one 口径下的天花板。
	MaxMarginUSDT float64

	// MinQuoteVolume24h 24h 成交额下限（USDT）
	MinQuoteVolume24h float64

	// ExtraExclude 手动黑名单
	ExtraExclude []string
}

// DefaultUniversePolicy 默认策略：0.1U/笔、20 倍、不买美股ETF、不买新上线、不买要下线
func DefaultUniversePolicy() UniversePolicy {
	return UniversePolicy{
		ExcludeStockETF:       true,
		ExcludeNewListingDays: 30,
		ExcludeDelisting:      true,
		MarginUSDT:            0.1,
		Leverage:              20,
		MarginPolicy:          "min_one",
		MaxMarginUSDT:         0.5,
		MinQuoteVolume24h:     0,
	}
}

// OrderMarginCap 放宽后的单笔保证金天花板。
//
//	fixed    → MarginUSDT（严格，不放大）
//	min_one  → MaxMarginUSDT（可放大，但封顶）
func (p UniversePolicy) OrderMarginCap() float64 {
	if p.MarginPolicy == "min_one" && p.MaxMarginUSDT > 0 {
		return p.MaxMarginUSDT
	}
	if p.MarginUSDT > 0 {
		return p.MarginUSDT
	}
	if p.MaxMarginUSDT > 0 {
		return p.MaxMarginUSDT
	}
	return 0
}

// ---------------------------------------------------------------------------
// 单张 / 最小张的资金需求
// ---------------------------------------------------------------------------

// effLever 实际生效杠杆 = min(合约上限, 策略配置)，至少 1
func effLever(instLever, policyLever int) int {
	l := instLever
	if policyLever > 0 && (l <= 0 || policyLever < l) {
		l = policyLever
	}
	if l < minLever {
		l = minLever
	}
	return l
}

// ContractNotional 单张名义价值（USDT） = ctVal × ctMult × 价格
func ContractNotional(ctVal, ctMult, price float64) float64 {
	if ctVal <= 0 || price <= 0 {
		return 0
	}
	if ctMult <= 0 {
		ctMult = 1
	}
	return ctVal * ctMult * price
}

// MinOrderMargin 下最小一手需要的保证金（USDT）
//
//	= minSz × ctVal × ctMult × price ÷ 杠杆
func MinOrderMargin(it model.Instrument, price float64, policy LeveragePolicy) float64 {
	lv := effLever(it.Lever, policy.Leverage)
	notional := ContractNotional(it.CtVal, it.CtMult, price)
	minSz := it.MinSz
	if minSz <= 0 {
		minSz = it.LotSz
	}
	if minSz <= 0 {
		minSz = 1
	}
	return minSz * notional / float64(lv)
}

// LeveragePolicy 只带杠杆的轻量口径（避免到处传整个 UniversePolicy）
type LeveragePolicy struct {
	Leverage int
}

// MaxAffordableSize 在给定保证金下，最多能买多少张（已对齐 lotSz，且 ≥ minSz 才有效）
func MaxAffordableSize(it model.Instrument, price, marginUSDT float64, leverage int) (sz float64, ok bool) {
	notional := ContractNotional(it.CtVal, it.CtMult, price)
	if notional <= 0 {
		return 0, false
	}
	lv := effLever(it.Lever, leverage)
	raw := marginUSDT * float64(lv) / notional
	lot := it.LotSz
	if lot <= 0 {
		lot = 1
	}
	steps := math.Floor(raw/lot + 1e-9)
	sz = steps * lot
	minSz := it.MinSz
	if minSz <= 0 {
		minSz = lot
	}
	if sz < minSz {
		return 0, false
	}
	return sz, true
}

// ---------------------------------------------------------------------------
// 过滤主函数
// ---------------------------------------------------------------------------

// FilterUniverse 按策略过滤合约。
//
// tickers 用于拿最新价算资金需求；delist 是下线名单（可为 nil）。
// 返回通过过滤的合约（按 24h 成交额降序）和过滤统计。
func FilterUniverse(
	list []model.Instrument,
	tickers map[string]model.Ticker,
	delist map[string]DelistEntry,
	p UniversePolicy,
) ([]model.Instrument, FilterStats) {

	st := FilterStats{ByCategory: map[string]int{}}
	st.Total = len(list)

	excl := map[string]bool{}
	for _, e := range p.ExtraExclude {
		excl[e] = true
	}

	cutListMs := int64(0)
	if p.ExcludeNewListingDays > 0 {
		cutListMs = nowMsFn() - int64(p.ExcludeNewListingDays)*86400000
	}

	kept := make([]model.Instrument, 0, len(list))
	delistHit := map[string]bool{}
	newHit := map[string]bool{}

	for _, it := range list {
		if excl[it.InstID] {
			st.DroppedState++
			continue
		}

		// ---- 规则 1：美股 / ETF / 商品 ----
		if p.ExcludeStockETF {
			cat := it.InstCategory
			if cat != "" {
				st.ByCategory[cat]++
			}
			if cat != "" && cat != "1" {
				st.DroppedCategory++
				continue
			}
		}

		// ---- 状态：只做 live ----
		if it.State != "" && it.State != "live" {
			st.DroppedState++
			continue
		}

		// ---- 规则 2：刚上线 ----
		if p.ExcludeNewListingDays > 0 && it.ListTime > 0 && it.ListTime > cutListMs {
			st.DroppedNew++
			newHit[instSymbol(it)] = true
			continue
		}

		// ---- 规则 3：要下线 ----
		if p.ExcludeDelisting && len(delist) > 0 {
			if e, ok := delist[instSymbol(it)]; ok {
				st.DroppedDelist++
				delistHit[e.Symbol] = true
				continue
			}
		}

		// ---- 成交额下限 ----
		if p.MinQuoteVolume24h > 0 {
			tk, ok := tickers[it.InstID]
			if !ok || tk.Last <= 0 || tk.QuoteVol24h < p.MinQuoteVolume24h {
				st.DroppedVolume++
				continue
			}
		}

		// ---- 规则 4：单笔保证金必须落在准入上限内 ----
		//   上限由 margin_policy 决定（见 OrderMarginCap）：
		//     fixed   → 严格 0.1U，最小一手买不起就排除（不放大）
		//     min_one → 放大到刚好 1 张，但不超过 max_margin_usdt
		cap := p.OrderMarginCap()
		if p.MarginUSDT > 0 && p.Leverage > 0 && cap > 0 {
			tk, ok := tickers[it.InstID]
			if ok && tk.Last > 0 {
				need := MinOrderMargin(it, tk.Last, LeveragePolicy{Leverage: p.Leverage})
				if need > cap+1e-9 {
					st.DroppedNotional++
					continue
				}
				if need > p.MarginUSDT+1e-9 {
					// 0.1U 买不起 1 张，但没超过硬上限 → 下单时会放大到 need
					st.ScaledUp++
				}
			}
		}

		kept = append(kept, it)
	}

	// 按成交额降序（没有行情的排后面）
	sort.SliceStable(kept, func(i, j int) bool {
		vi := tickers[kept[i].InstID].QuoteVol24h
		vj := tickers[kept[j].InstID].QuoteVol24h
		return vi > vj
	})

	st.Kept = len(kept)
	for s := range delistHit {
		st.DelistSymbols = append(st.DelistSymbols, s)
	}
	for s := range newHit {
		st.NewListingSymbol = append(st.NewListingSymbol, s)
	}
	sort.Strings(st.DelistSymbols)
	sort.Strings(st.NewListingSymbol)
	return kept, st
}

// instSymbol 取合约的基础币种（BTC-USDT-SWAP → BTC）
func instSymbol(it model.Instrument) string {
	if it.BaseCcy != "" {
		return it.BaseCcy
	}
	id := it.InstID
	if i := indexByte(id, '-'); i > 0 {
		return id[:i]
	}
	return id
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// BreakEvenMovePct 在给定杠杆 / 保证金下，价格要动多少才够回本手续费
//
// 只用于前端提示，让「0.1U 一笔」的成本一眼可见。
// takerFee 通常 0.0005（0.05%），一进一出算两次。
func BreakEvenMovePct(leverage int, takerFee float64) float64 {
	if leverage <= 0 {
		return 0
	}
	if takerFee <= 0 {
		takerFee = 0.0005
	}
	return 2 * takerFee * 100
}

// nowMsFn 可替换的时钟（测试用）
var nowMsFn = func() int64 { return time.Now().UnixMilli() }

// ---------------------------------------------------------------------------
// 排除原因（exclude_reason）文案
// ---------------------------------------------------------------------------
//
// 原因码由 cmd/okxweb 的 reasonOf() 写进 inst.exclude_reason，
// 文案统一放在这里，保证「网页显示的话」和「落库的码」永远对得上。
const (
	ReasonNone       = ""
	ReasonStockETF   = "stock_etf" // 美股 / ETF
	ReasonCommodity  = "commodity" // 商品（黄金原油等）
	ReasonCategory   = "category"  // 其它非加密分类
	ReasonState      = "state"     // 合约不是 live
	ReasonNewListing = "new_listing"
	ReasonDelisting  = "delisting"
	ReasonNoTicker   = "no_ticker"  // 没有行情快照，算不出买不买得起
	ReasonLowVolume  = "low_volume" // 24h 成交额不足
	ReasonNotional   = "notional"   // 最小一手保证金超上限
	ReasonManual     = "manual"
	ReasonOther      = "other"
)

// ExcludeLabel 把排除原因码翻成一句人话（网页 / 接口共用）
func ExcludeLabel(reason string) string {
	switch reason {
	case ReasonNone:
		return ""
	case ReasonStockETF:
		return "美股/ETF，不做"
	case ReasonCommodity:
		return "商品合约，不做"
	case ReasonCategory:
		return "非加密品种，不做"
	case ReasonState:
		return "合约非正常交易状态"
	case ReasonNewListing:
		return "刚上线，历史太短"
	case ReasonDelisting:
		return "OKX 已公告下线"
	case ReasonNoTicker:
		return "暂无行情，无法定价"
	case ReasonLowVolume:
		return "24h 成交额不足"
	case ReasonNotional:
		return "最小一手保证金超上限"
	case ReasonManual:
		return "手工排除"
	case ReasonOther:
		return "其它原因"
	default:
		return reason
	}
}
