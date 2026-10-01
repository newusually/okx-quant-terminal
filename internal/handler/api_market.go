package handler

// api_market.go —— 行情类接口：总览 / 合约列表 / 实时行情 / K 线 / 图表数据

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	"finally-main/internal/model"
	"finally-main/internal/repo"
	"finally-main/internal/service"
)

// ---------------------------------------------------------------------------
// 金额格式化（K 线图上的标记文字用）
// ---------------------------------------------------------------------------

// fmtUSDT 金额格式化：0.1 → "0.10U"；100 → "100U"。
// 用户要求标记上直接写「买入多少美金」，所以这里把 USDT 当成展示单位。
func fmtUSDT(v float64) string {
	if v <= 0 {
		return "0U"
	}
	if v >= 100 {
		return strconv.FormatFloat(v, 'f', 0, 64) + "U"
	}
	return strconv.FormatFloat(v, 'f', 2, 64) + "U"
}

// fmtSignedUSDT 带符号的金额：+0.02U / -0.03U（平仓盈亏用）
func fmtSignedUSDT(v float64) string {
	s := fmtUSDT(math.Abs(v))
	if v >= 0 {
		return "+" + s
	}
	return "-" + s
}

// ---------------------------------------------------------------------------
// /api/state
// ---------------------------------------------------------------------------

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) (any, error) {
	st, err := s.db.Stats()
	if err != nil {
		return nil, err
	}
	counts, _ := s.db.TableCounts()

	// ---- 账户快照：顶栏「权益 / 可用 / 本金 / 浮盈 / 总盈亏」的数据源 ----
	// 口径与 /api/account 完全一致（同一个函数），两处不会打架。
	// ⚠ 复用上面已经算好的 st：以前这里会再调一次 Stats()，
	//   等于同一次请求把 trade/signals 的聚合查询做了两遍。
	account := s.snapshot(st)

	// ★ 每请求现取一次配置（热读），改 JSON 后网页下次轮询就是新口径
	scfg := s.cfg()

	return map[string]any{
		"ok":           true,
		"stats":        st,
		"account":      account,
		"live":         service.LiveStatusSnapshot(),
		"strategy":     scfg,
		"marginText":   scfg.MarginText(),
		// 准入上限：前端「服务信息」直接显示，方便确认 JSON 改了有没有生效
		"maxOrderMarginUsdt": scfg.MaxOrderMarginUSDT,
		"configPath":         s.strategy.Path(),
		"bars":               service.SupportedBars,
		"dbPath":             s.db.Path(),
		"tables":             counts,
		"uptimeSec":    int(time.Since(s.startAt).Seconds()),
		"queueLen":     s.bf.QueueLen(),
		"backfillDays": s.bf.Config().Days,
		// 三条独立的数据保留红线（2026-10-01 起）：
		//   retainDays     记录表（trade/trade_event/signals/equity/runlog）→ 月度任务里清
		//   klineRetainDays K 线 15m                                          → 年度任务里清
		//   logRetainDays  日志文件（logs/ + apache/logs/）                   → 月度任务里清
		// 前端「服务信息」直接读这三个值。
		"retainDays":      repo.RetainDays(),
		"klineRetainDays": repo.KlineRetainDays(),
		"logRetainDays":   repo.LogRetainDays(),
		// 磁盘守卫：C 盘可用低于这个 GB 数时，月度任务把 K 线收缩到「只留当月」
		"archiveMinFreeGB": repo.ArchiveMinFreeGB(),
		"archiveDir":       service.ArchiveDir(),
		"freeDiskGB":       float64(service.FreeDiskMB(`C:\`)) / 1024,
		"version":          "okx-web/1.3 (pure go · live · 月度/年度维护)",
	}, nil
}

// ---------------------------------------------------------------------------
// /api/instruments
// ---------------------------------------------------------------------------

func (s *Server) handleInstruments(w http.ResponseWriter, r *http.Request) (any, error) {
	list, err := s.db.ListInstruments()
	if err != nil {
		return nil, err
	}

	// scope 决定「给谁看」：
	//   tradeable（默认）→ 只给可交易合约，网页下拉框就是这个
	//   all              → 全量（含被排除的），用于「为什么没这只币」的排查页
	//   excluded         → 只看被排除的，配合 exclude_reason 看原因分布
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = "tradeable"
	}
	// 便利开关：?tradeable=0 等价于 ?scope=all
	if v := r.URL.Query().Get("tradeable"); v == "0" || v == "false" {
		scope = "all"
	}

	type item struct {
		InstID      string  `json:"instId"`
		Name        string  `json:"name"`
		Base        string  `json:"base"`
		Quote       string  `json:"quote"`
		Last        float64 `json:"last"`
		ChgPct      float64 `json:"chgPct"`
		QuoteVol24h float64 `json:"quoteVol24h"`
		CtVal       float64 `json:"ctVal"`
		CtMult      float64 `json:"ctMult"`
		LotSz       float64 `json:"lotSz"`
		MinSz       float64 `json:"minSz"`
		TickSz      float64 `json:"tickSz"`
		Lever       int     `json:"lever"`
		State       string  `json:"state"`
		// ---- 准入信息（网页用来显示「能不能买 / 为什么不能买」）----
		InstCategory  string `json:"instCategory"` // 1=加密 3=美股ETF 4=商品
		Tradeable     bool   `json:"tradeable"`
		TradeableInt  int    `json:"tradeableInt"`
		ExcludeReason string `json:"excludeReason"`
		ExcludeLabel  string `json:"excludeLabel"`
		// MarginUSDT 最小一手保证金（≤ max_order_margin_usdt 才可交易）
		MarginUSDT float64 `json:"marginUsdt"`
	}

	prices := map[string]model.Ticker{}
	if ts, err := s.db.ListTickers(); err == nil {
		for _, t := range ts {
			prices[t.InstID] = t
		}
	}

	lev := s.cfg().Entry.Leverage
	out := make([]item, 0, len(list))
	for _, it := range list {
		if scope == "tradeable" && it.Tradeable != 1 {
			continue
		}
		if scope == "excluded" && it.Tradeable == 1 {
			continue
		}
		p := prices[it.InstID]
		margin := 0.0
		if p.Last > 0 {
			margin = service.MinOrderMargin(it, p.Last, service.LeveragePolicy{Leverage: lev})
		}
		out = append(out, item{
			InstID: it.InstID, Name: it.BaseCcy + "/USDT", Base: it.BaseCcy, Quote: it.QuoteCcy,
			Last: p.Last, ChgPct: p.ChgPct, QuoteVol24h: it.QuoteVol24h,
			CtVal: it.CtVal, CtMult: it.CtMult, LotSz: it.LotSz, MinSz: it.MinSz,
			TickSz: it.TickSz, Lever: it.Lever, State: it.State,
			InstCategory: it.InstCategory, Tradeable: it.Tradeable == 1,
			TradeableInt: it.Tradeable, ExcludeReason: it.ExcludeReason,
			ExcludeLabel: service.ExcludeLabel(it.ExcludeReason), MarginUSDT: margin,
		})
	}

	// 默认按 24h 成交额从高到低，下拉框里热门的排前面
	sort.SliceStable(out, func(i, j int) bool { return out[i].QuoteVol24h > out[j].QuoteVol24h })

	// 排除原因分布（不受 scope 影响，始终按全量统计）
	reasonAll := map[string]int{}
	for _, it := range list {
		if it.Tradeable != 1 {
			reasonAll[it.ExcludeReason]++
		}
	}
	total, keptAll := len(list), len(list)-sumInts(reasonAll)

	return map[string]any{
		"ok":    true,
		"scope": scope,
		"count": len(out),
		"list":  out,
		"universe": map[string]any{
			"total":    total,
			"kept":     keptAll,
			"dropped":  total - keptAll,
			"byReason": reasonAll,
			"labels":   labelMap(reasonAll),
		},
		"leverage": lev,
	}, nil
}

// sumInts 小工具：把统计 map 的值加起来
func sumInts(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// labelMap 把「原因码 → 数量」补成「原因码 → 人话」，前端直接显示
func labelMap(byReason map[string]int) map[string]string {
	out := make(map[string]string, len(byReason))
	for k := range byReason {
		out[k] = service.ExcludeLabel(k)
	}
	return out
}

// ---------------------------------------------------------------------------
// /api/tickers
// ---------------------------------------------------------------------------

func (s *Server) handleTickers(w http.ResponseWriter, r *http.Request) (any, error) {
	list, err := s.db.ListTickers()
	if err != nil {
		return nil, err
	}
	insts, _ := s.db.ListInstruments()
	nameOf := make(map[string]string, len(insts))
	for _, it := range insts {
		nameOf[it.InstID] = it.BaseCcy + "/USDT"
	}
	type item struct {
		InstID string  `json:"instId"`
		Name   string  `json:"name"`
		Ts     int64   `json:"ts"`
		Last   float64 `json:"last"`
		ChgPct float64 `json:"chgPct"`
		High   float64 `json:"high24h"`
		Low    float64 `json:"low24h"`
		Vol    float64 `json:"quoteVol24h"`
	}
	out := make([]item, 0, len(list))
	for _, t := range list {
		out = append(out, item{
			InstID: t.InstID, Name: nameOf[t.InstID], Ts: t.Ts, Last: t.Last,
			ChgPct: t.ChgPct, High: t.High24h, Low: t.Low24h, Vol: t.QuoteVol24h,
		})
	}
	// 按涨跌幅排个序，前端滚动条看着舒服
	sort.Slice(out, func(i, j int) bool { return out[i].Vol > out[j].Vol })
	return map[string]any{"ok": true, "ts": time.Now().UnixMilli(), "list": out}, nil
}

// ---------------------------------------------------------------------------
// /api/kline
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// K 线窗口（支持「往左翻页」累加加载）
// ---------------------------------------------------------------------------

// indicatorWarmup 指标暖机根数。
//
// MA99 需要前 98 根才能算准第一根，布林要前 19 根。
// 所以每次多抓 99 根，算完指标再把多出来的裁掉——
// 这样翻页拼起来之后，均线在拼接处不会出现「突然从第 99 根才开始」的断层。
const indicatorWarmup = 99

// KlinePage 一页 K 线 + 翻页元信息
type KlinePage struct {
	// Raw 含暖机段的原始序列 —— 指标必须在它上面算，不能在 Rows 上算，
	// 否则每页最前面 98 根会因为缺少历史而没有 MA99。
	Raw  []model.Kline `json:"-"`
	Trim int           `json:"-"` // 要从头部裁掉多少根（= 暖机段长度）

	Rows       []model.Kline `json:"rows"`       // 已裁掉暖机段、按时间升序
	Limit      int           `json:"limit"`      // 本次请求的根数
	BeforeTs   int64         `json:"beforeTs"`   // 本次的「往前取」边界（0 = 取最新）
	FirstTs    int64         `json:"firstTs"`    // 本页最老一根
	LastTs     int64         `json:"lastTs"`     // 本页最新一根
	HasMore    bool          `json:"hasMore"`    // 更早还有没有数据
	EarliestTs int64         `json:"earliestTs"` // 库里该 (合约,周期) 的最老一根
	TotalBars  int64         `json:"totalBars"`  // 库里该 (合约,周期) 的总根数
}

// klineWindow 取一段 K 线窗口。
//
//	beforeTs > 0  → 取 ts < beforeTs 的最近 limit 根（向左翻页）
//	beforeTs == 0 → 取最新 limit 根
//	limit    <= 0 → 退回「按天数取」，一次拿全（老行为，给小 limit 的调用方用）
//
// ★ 分页模式下多抓 indicatorWarmup 根，并且【不在这里裁】：
// 指标要在含暖机段的 Raw 上算完，再按 Trim 同步裁掉，两边的下标才对得齐。
func (s *Server) klineWindow(inst, bar string, days int, beforeTs int64, limit int) (KlinePage, error) {
	pg := KlinePage{Limit: limit, BeforeTs: beforeTs}

	if limit <= 0 {
		fromTs := time.Now().AddDate(0, 0, -days).UnixMilli()
		rows, err := s.db.QueryKlines(model.KlineQuery{InstID: inst, Bar: bar, FromTs: fromTs})
		if err != nil {
			return pg, err
		}
		pg.Raw, pg.Trim, pg.Rows = rows, 0, rows
		if n := len(rows); n > 0 {
			pg.FirstTs, pg.LastTs = rows[0].Ts, rows[n-1].Ts
		}
		return pg, nil
	}

	// 多抓暖机段，算指标时才有历史
	q := model.KlineQuery{InstID: inst, Bar: bar, Limit: limit + indicatorWarmup}
	if beforeTs > 0 {
		q.ToTs = beforeTs - 1 // 严格往前，不含 beforeTs 那根
	}
	raw, err := s.db.QueryKlines(q)
	if err != nil {
		return pg, err
	}
	pg.Raw = raw
	if len(raw) > limit {
		pg.Trim = len(raw) - limit
	}
	pg.Rows = raw[pg.Trim:]
	if n := len(pg.Rows); n > 0 {
		pg.FirstTs, pg.LastTs = pg.Rows[0].Ts, pg.Rows[n-1].Ts
	}

	// 库里还有没有更早的 / 一共多少根
	if first, err := s.db.FirstKlineTs(inst, bar); err == nil {
		pg.EarliestTs = first
		pg.HasMore = pg.FirstTs > first && pg.FirstTs > 0
	}
	// 走 20 秒 SWR 缓存：命中零 DB 往返，过期先返回旧值再后台刷。
	// 前端轮询 /api/kline 时同一对 (inst,bar) 会被反复读，这里能省掉大量重复扫描。
	cov := s.db.CoverageCached(inst, bar)
	pg.TotalBars = cov.Count
	return pg, nil
}

// ---------------------------------------------------------------------------
// /api/kline
// ---------------------------------------------------------------------------

// clampQueryDays 把查询窗口夹在 K 线保留窗口内。
//
// ★ 2026-10-01 二期：用户口径「只能查询保存最近 10 天数据，不能多」★
//
// 库里本来就只留 10 天（每日任务删超期的），这里再夹一层是为了：
//   ① 前端传 days=30/365 时不会去算一个「跨越不存在的历史」的窗口；
//   ② 接口语义与保留策略一致 —— 写进文档的口径就是接口强制的口径，
//      不依赖调用方自觉。
// KlineRetainDays() <= 0（配置写坏）时不夹，宁可多查也不要变成查不到。
func clampQueryDays(days int) int {
	if cap := repo.KlineRetainDays(); cap > 0 && days > cap {
		return cap
	}
	return days
}

func (s *Server) handleKline(w http.ResponseWriter, r *http.Request) (any, error) {
	q := r.URL.Query()
	inst := q.Get("inst")
	bar := q.Get("bar")
	days := clampQueryDays(atoiDefault(q.Get("days"), s.bf.Config().Days))
	limit := atoiDefault(q.Get("limit"), 0)
	before := int64(atoiDefault(q.Get("before"), 0))
	auto := q.Get("auto") != "0" // 默认自动按需回补

	if inst == "" || !service.IsSupportedBar(bar) {
		return nil, fmt.Errorf("参数不合法：inst=%q bar=%q（周期只支持 %v）", inst, bar, service.SupportedBars)
	}

	// 不够一个月就排队去补（异步，不阻塞这次请求）
	cov := s.db.CoverageCached(inst, bar)
	needBackfill := false
	if auto {
		if cov.Count == 0 || cov.Days < float64(days)-0.5 {
			needBackfill = s.bf.Enqueue(inst, bar)
		}
	}

	pg, err := s.klineWindow(inst, bar, days, before, limit)
	if err != nil {
		return nil, err
	}
	// 本地不足时，兜底直接从 OKX 现拉最新一批，保证图不空
	if len(pg.Rows) == 0 && before == 0 {
		if latest, ferr := s.feed.FetchCandles(inst, bar, 300); ferr == nil && len(latest) > 0 {
			_, _ = s.db.UpsertKlines(latest)
			pg.Rows = latest
			pg.FirstTs, pg.LastTs = latest[0].Ts, latest[len(latest)-1].Ts
			cov.Count = int64(len(latest))
		}
	}

	return map[string]any{
		"ok":          true,
		"inst":        inst,
		"bar":         bar,
		"days":        days,
		"count":       len(pg.Rows),
		"coverage":    cov,
		"backfilling": needBackfill,
		"page": map[string]any{
			"limit":      pg.Limit,
			"beforeTs":   pg.BeforeTs,
			"firstTs":    pg.FirstTs,
			"lastTs":     pg.LastTs,
			"hasMore":    pg.HasMore,
			"earliestTs": pg.EarliestTs,
			"totalBars":  pg.TotalBars,
		},
		"list": pg.Rows,
	}, nil
}

// ---------------------------------------------------------------------------
// /api/mark —— K 线 + 均线 + 布林 + 指标（前端画图用，一次拿全）
// ---------------------------------------------------------------------------
//
// 支持分页：默认一页 1000 根；传 before=<ts> 拿更早的一页（向左无限翻）。
// 指标用「多抓 99 根暖机」的方式算，所以拼接处不会断层。

func (s *Server) handleMark(w http.ResponseWriter, r *http.Request) (any, error) {
	q := r.URL.Query()
	inst := q.Get("inst")
	bar := q.Get("bar")
	days := clampQueryDays(atoiDefault(q.Get("days"), s.bf.Config().Days))
	limit := atoiDefault(q.Get("limit"), DefaultKlinePage)
	before := int64(atoiDefault(q.Get("before"), 0))
	if inst == "" || !service.IsSupportedBar(bar) {
		return nil, fmt.Errorf("参数不合法：inst=%q bar=%q", inst, bar)
	}

	// ★ 实时性：先保证「当前这根」是最新的，再读库。
	// 前端每几秒轮询一次，这里带节流，不会打爆 OKX 限频。
	if q.Get("fresh") != "0" {
		s.ensureFresh(inst, bar)
	}

	pg, err := s.klineWindow(inst, bar, days, before, limit)
	if err != nil {
		return nil, err
	}
	if len(pg.Rows) == 0 && before == 0 {
		if latest, ferr := s.feed.FetchCandles(inst, bar, 300); ferr == nil && len(latest) > 0 {
			_, _ = s.db.UpsertKlines(latest)
			pg.Raw, pg.Trim, pg.Rows = latest, 0, latest
			pg.FirstTs, pg.LastTs = latest[0].Ts, latest[len(latest)-1].Ts
		}
	}

	// ★ 指标在「含暖机段的 Raw」上算，再把前面 Trim 根丢掉。
	// 这样每页最前面那些 bar 也能拿到正确的 MA99 / 布林，翻页拼接不会断层。
	src := pg.Raw
	if len(src) == 0 {
		src = pg.Rows
	}
	closes := make([]float64, len(src))
	for i, k := range src {
		closes[i] = k.C
	}
	ma7 := smaSeries(closes, 7)
	ma25 := smaSeries(closes, 25)
	ma99 := smaSeries(closes, 99)
	bollUp, bollMid, bollLo := bollSeries(closes, 20, 2.0)

	type pt struct {
		Ts int64   `json:"ts"`
		V  float64 `json:"v"`
	}
	trim := pg.Trim
	if trim > len(src) {
		trim = len(src)
	}
	// 对齐到输出行：Raw[trim:] 对应 Rows[0:]，所以指标也从下标 trim 开始吐
	toPts := func(vals []float64) []pt {
		out := []pt{}
		for i := trim; i < len(vals) && i < len(src); i++ {
			v := vals[i]
			if math.IsNaN(v) || math.IsInf(v, 0) {
				continue
			}
			out = append(out, pt{Ts: src[i].Ts, V: v})
		}
		return out
	}

	// ---- 图上标注：买入信号（小火箭）+ 开仓/平仓（火箭 / 绿叶）----
	//
	// 只在「当前这一页 K 线覆盖的时间区间」里取，翻页时前端重新请求，
	// 所以历史回补出来的那段也会照常标出来，不用一次性把全历史塞给浏览器。
	//
	// 关键字对齐：成交时间带毫秒（比如 16:42:16.374），而 K 线开盘时间是
	// 周期毫秒数的整数倍。不向下取整的话标记会落在两根 K 线中间，画不出来。
	markers := []map[string]any{}
	if q.Get("markers") != "0" && len(pg.Rows) > 0 {
		fromTs := pg.Rows[0].Ts
		toTs := pg.Rows[len(pg.Rows)-1].Ts

		barMs := service.BarDurationMs(bar)
		if barMs <= 0 {
			barMs = 15 * 60 * 1000
		}
		// ⚠ 单位必须是「秒」：图表里 K 线的 time 是 Math.floor(ts/1000)，
		// 而这里 ts / 成交时间都是毫秒。不除 1000 的话标记会落在几万年后，
		// 图表直接整批丢弃 → 表现就是「历史信号一个火箭都不显示」。
		snap := func(ts int64) int64 {
			if ts <= 0 {
				return 0
			}
			return (ts - ts%barMs) / 1000
		}

		// 只保留「确实有这根 K 线」的时间点，否则标记会被图表丢掉或报错
		hasBar := make(map[int64]bool, len(pg.Rows))
		for _, k := range pg.Rows {
			hasBar[k.Ts/1000] = true
		}
		push := func(m map[string]any) {
			t, _ := m["time"].(int64)
			if t > 0 && hasBar[t] {
				markers = append(markers, m)
			}
		}

		// ① 买入信号：score 够线就画。acted=1（真下过单）用实心火箭，
		//    只报信号没下单的用浅色小箭头，一眼分得清。
		//
		//    颜色用金色（主题强调色）而不是红/绿：红绿在 K 线上已经被
		//    「涨/跌」占用了，再拿来表示买卖只会看串。
		if sigs, serr := s.db.SignalsInRange(inst, bar, fromTs, toTs); serr == nil {
			// 去密：**同一根 K 线**上的重复信号只留一条 —— 避免「一根 K 线
			// 底下叠一排火箭」那种看起来像 bug 的画面。
			//
			// 不同 K 线的信号全保留。之前按 cooldown_bars（6 根）跨度过滤，
			// 把「实盘不会成交的那些」也一并抹掉了，结果图上只剩零星几个点，
			// 用户以为是数据丢了。冷却本来就只是下单闸门的事，和「这根 K 线
			// 出没出过信号」无关，图上应该如实显示。
			lastSigTs := int64(-1)
			for _, sg := range sigs {
				if sg.Score <= 0 {
					continue
				}
				if sg.Ts == lastSigTs {
					continue // 同一根重复
				}
				lastSigTs = sg.Ts
				m := map[string]any{
					"time": snap(sg.Ts), "position": "belowBar", "shape": "arrowUp",
					"color": "#fcd535", "text": "🚀", "size": 2,
					"kind": "signal", "price": sg.Close, "score": sg.Score,
					"hitList": sg.HitList, "reason": sg.Reason, "ts": sg.Ts,
				}
				if sg.Acted == 0 {
					// 只报信号没下单：同样是 🚀，但用暗金色 + 小一号，
					// 和「真买了」的实心金 🚀 一眼能分，又都满足「信号用火箭标」。
					m["color"] = "#8a7a2a"
					m["size"] = 1
					m["kind"] = "signal_only"
				}
				push(m)
			}
		}

		// ② 交易事件流水：开仓 / 加仓 / 平仓，一次一笔
		//
		//    这里读的是 trade_event 而不是 trade —— trade 表「一个仓位一行」，
		//    加仓是就地合并进原行的（张数/均价/保证金被覆盖），所以它**根本
		//    没有「加仓」这个时间点**，图上自然也就标不出加仓。
		//
		//    标记文字直接写金额，鼠标扫一眼就知道这笔买了多少钱：
		//      开仓 = 金色火箭「买入 0.10U」
		//      加仓 = 蓝色箭头「加仓 0.03U」
		//      平仓 = 绿叶「平仓 +0.02U」（亏损用灰，因为红在图上代表涨）
		if evs, eerr := s.db.EventsInRange(inst, fromTs, toTs); eerr == nil {
			for _, e := range evs {
				switch e.Kind {
				case "open":
					push(map[string]any{
						"time": snap(e.Ts), "position": "belowBar", "shape": "arrowUp",
						"color": "#fcd535", "text": "买入 " + fmtUSDT(e.Margin), "size": 2,
						"kind": "open", "price": e.Px, "sz": e.Sz, "margin": e.Margin,
						"leverage": e.Leverage, "score": e.Score, "id": e.ID, "ts": e.Ts,
					})
				case "addon":
					push(map[string]any{
						"time": snap(e.Ts), "position": "belowBar", "shape": "arrowUp",
						"color": "#3b82f6", "text": "加仓 " + fmtUSDT(e.Margin), "size": 1,
						"kind": "addon", "price": e.Px, "sz": e.Sz, "margin": e.Margin,
						"leverage": e.Leverage, "id": e.ID, "ts": e.Ts,
					})
				case "close":
					col := "#0ecb81" // 绿 = 赚了
					if e.Pnl < 0 {
						col = "#848e9c" // 灰 = 亏了（别用红，红在图上表示涨）
					}
					push(map[string]any{
						"time": snap(e.Ts), "position": "aboveBar", "shape": "arrowDown",
						"color": col, "text": "平仓 " + fmtSignedUSDT(e.Pnl), "size": 2,
						"kind": "close", "price": e.Px, "sz": e.Sz, "margin": e.Margin,
						"pnl": e.Pnl, "pnlPct": e.PnlPct, "reason": e.Reason,
						"id": e.ID, "ts": e.Ts,
					})
				}
			}
		}

		sort.SliceStable(markers, func(i, j int) bool {
			ti, _ := markers[i]["time"].(int64)
			tj, _ := markers[j]["time"].(int64)
			return ti < tj
		})
	}

	// 覆盖情况（「覆盖 N 天 / N 根」脚标）。前端 mergePage 直接读
	// j.coverage.days / j.coverage.count，缺了这个就会一直显示 0。
	cov := s.db.CoverageCached(inst, bar)

	return map[string]any{
		"ok":       true,
		"inst":     inst,
		"bar":      bar,
		"count":    len(pg.Rows),
		"coverage": cov,
		"kline":    pg.Rows,
		"markers":  markers,
		"ma7":      toPts(ma7),
		"ma25":     toPts(ma25),
		"ma99":     toPts(ma99),
		"bollUp":   toPts(bollUp),
		"bollMid":  toPts(bollMid),
		"bollLo":   toPts(bollLo),
		"page": map[string]any{
			"limit":      pg.Limit,
			"beforeTs":   pg.BeforeTs,
			"firstTs":    pg.FirstTs,
			"lastTs":     pg.LastTs,
			"hasMore":    pg.HasMore,
			"earliestTs": pg.EarliestTs,
			"totalBars":  pg.TotalBars,
		},
	}, nil
}

// DefaultKlinePage 前端一页默认加载多少根 K 线（向左翻页时每次也是这个数）
const DefaultKlinePage = 1000
