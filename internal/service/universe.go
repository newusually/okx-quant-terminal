package service

// universe.go —— 合约准入过滤（「哪些合约可以买」的唯一判定处）
//
// 三条规则，全部在这里落地，网页侧和引擎侧共用同一份逻辑：
//
//   规则 1  不买美股 / ETF / 商品  —— ★ 2026-10-02 三期已按用户要求关闭 ★
//           OKX 的 instCategory：1=加密  3=美股/ETF  4=商品(黄金/原油/白银…)
//           原口径是「只留 category=1」。三期用户明确：「取消美股 etf 不做的功能，
//           只要买入上限小于 1U 就做」—— 于是默认改成不做品类区分，
//           能不能做只看下面规则 5（最小一手保证金 ≤ 上限）。
//           想恢复：把 configs/okx_strategy.json 的 exclude_stock_etf 改回 true。
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
//   规则 5  单笔保证金必须 ≤ max_order_margin_usdt
//           一笔的保证金 = 张数 × ctVal × ctMult × 价格 ÷ 杠杆
//           最小张数由 minSz 决定，所以「买得起」等价于
//               minSz × ctVal × ctMult × price ÷ lever ≤ max_order_margin_usdt
//           上限由 MarginPolicy 决定：
//               min_one → 上限 = MaxMarginUSDT。目标 1U 买不起 1 张的，
//                         放大到刚好买 1 张来下单；超过上限的直接排除。
//               fixed   → 上限 = MarginUSDT。严格按目标值，买不起就不买。
//           当前口径（2026-10-01）：目标 1U/笔、上限 1.5U。
//           不满足的合约（例如 BTC-USDT-SWAP 单张就 830 U）直接不进候选池。

import (
	"math"
	"sort"
	"time"

	"finally-main/internal/conf"
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

// DefaultUniversePolicy 库内兜底策略。
//
// ★ 这不是「要求」—— 真正生效的口径来自 configs/okx_strategy.json
//   （`max_order_margin_usdt` / `entry.*` / `min_quote_volume_24h` 等），
//   改完热生效。这个函数只在调用方没给策略时兜底。
func DefaultUniversePolicy() UniversePolicy {
	return UniversePolicy{
		// ★ 2026-10-02 三期：默认关闭品类过滤（用户：「取消美股 etf 不做的功能，
		//   只要买入上限小于 1U 就做」）。它是「兜底默认值」，只在
		//   configs/okx_strategy.json 缺失时用到 —— 兜底也必须是关的，
		//   否则配置文件一旦读不到，被取消的那条规则会悄悄复活。
		ExcludeStockETF:       false,
		ExcludeNewListingDays: 30,
		ExcludeDelisting:      true,
		MarginUSDT:            0.1,
		Leverage:              20,
		MarginPolicy:          "min_one",
		MaxMarginUSDT:         0.5,
		MinQuoteVolume24h:     0,
	}
}

// UniversePolicyFromConfig 从策略配置（真源 configs/okx_strategy.json）拼出准入策略。
//
// ★★ 准入策略只能从这里造，不要在调用点手写 UniversePolicy 字面量 ★★
//
// 踩过的坑（2026-10-01 二期，静默失效，没有任何报错）：
//
//	Scan() 里原来手写了一个字面量。新增 `margin_policy` 字段时**漏填了 MarginPolicy**，
//	它一空，OrderMarginCap() 就跳过了 min_one 分支、掉回 MarginUSDT：
//
//	    min_one → MaxMarginUSDT = 1.0U   （应该走这里，全市场 170 个都能买）
//	    掉回后 → MarginUSDT     = 0.01U  （实际走这里，只剩 33 个候选）
//
//	表现是日志里「合约准入过滤：480 → 33（… 资金不够 -137 …）」，
//	而配置、README、网页面板全都写着「准入上限 1U / 170 个合约」——
//	**同一个量被两条路算过，口径不一致**，属于最阴的一类 bug：
//	不崩、不报错、日志干净，只是策略悄悄只做 1/5 的币。
//
// 所以把构造收成一个函数：以后加/删字段只改这一处，调用点不可能漏填。
func UniversePolicyFromConfig(cfg *conf.Config) UniversePolicy {
	if cfg == nil {
		return DefaultUniversePolicy()
	}
	p := DefaultUniversePolicy()
	p.ExcludeStockETF = cfg.ExcludeStockETF
	p.ExcludeNewListingDays = cfg.ExcludeNewListingDays
	p.ExcludeDelisting = cfg.ExcludeDelisting
	p.MinQuoteVolume24h = cfg.MinQuoteVolume24h
	p.ExtraExclude = cfg.ExcludeInst
	if cfg.Entry != nil {
		p.MarginUSDT = cfg.Entry.MarginUSDT
		p.Leverage = cfg.Entry.Leverage
		p.MarginPolicy = cfg.Entry.MarginPolicy
	}
	// 上限统一走 Config.OrderMarginCap()：max_order_margin_usdt 与
	// entry.max_margin_usdt 的优先级已经在那边理清了，别在这里再判一次
	// （两处各判一次 = 又是「同一个量两条路算」）。
	p.MaxMarginUSDT = cfg.OrderMarginCap()
	// 老配置里没有 margin_policy 这个键：归一化会补成 min_one，
	// 这里再兜一次，保证「字段为空」永远不会被解读成「严格 0.01U」。
	if p.MarginPolicy == "" {
		p.MarginPolicy = "min_one"
	}
	return p
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

		// ---- 规则 0：只读板块（NQ 等外部数据源）----
		//
		// 这类合约**刻意**留在 inst 表里（前端列表、图表要能查到它），
		// 所以必须在这里显式排除。不能指望「它没有 OKX ticker → 成交额不足」
		// 那条间接效果 —— 哪天给只读合约补上行情快照，它就会被判成可交易。
		// 判据只有一处：IsReadonlyInst。
		if IsReadonlyInst(it.InstID) {
			st.DroppedReadonly++
			continue
		}

		// ---- 规则 1：美股 / ETF / 商品（★ 2026-10-02 三期：默认已关闭）----
		//
		// 用户口径：「取消美股 etf 不做的功能，只要买入上限小于 1U 就做」。
		// 关上之后这条不再排除任何品类，真正的准入约束只剩
		//   状态 live / 非新上线 / 非待下线 / 成交额达标 / 最小一手 ≤ 上限。
		//
		// ⚠ 品类统计刻意留在 if 外面：原来它写在开关里面，
		//   开关一关 `-universe` 诊断的「分类分布」会变成空 map，
		//   看起来像「合约列表没拉到」。统计是诊断用的，与开关无关。
		cat := it.InstCategory
		if cat != "" {
			st.ByCategory[cat]++
		}
		if p.ExcludeStockETF && cat != "" && cat != "1" {
			st.DroppedCategory++
			continue
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

// InstDisplayName 合约展示名 —— 全项目唯一构造入口。
//
// 背景：`BaseCcy + "/USDT"` 这段拼接原来散落在 7 个 handler 里
// （api_market 2 处、api_admin 1 处、api_account 5 处）。
// 结果是「只读板块要显示成 NQ / 纳斯达克100」就得改 7 遍，
// 改漏一处就会出现同一个合约在不同面板叫不同名字。
//
// 现在统一走这里：只读板块用专属名，其余维持原口径。
func InstDisplayName(it model.Instrument) string {
	if IsReadonlyInst(it.InstID) {
		return ReadonlyInstName(it.InstID)
	}
	if it.BaseCcy != "" {
		return it.BaseCcy + "/USDT"
	}
	return it.InstID
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
	// ReasonReadonly 只读板块（NQ 等外部数据源）：有行情有信号，永不交易。
	// 与上面那些「暂时不合格」的原因有本质区别 —— 它是设计约束，不随参数放宽而改变。
	ReasonReadonly = "readonly"
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
	case ReasonReadonly:
		return "只读展示，不可交易"
	case ReasonOther:
		return "其它原因"
	default:
		return reason
	}
}
