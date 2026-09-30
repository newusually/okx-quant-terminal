package handler

// api_market.go —— 行情类接口：总览 / 合约列表 / 实时行情 / K 线 / 图表数据

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"time"

	"finally-main/internal/model"
	"finally-main/internal/service"
)

// ---------------------------------------------------------------------------
// /api/state
// ---------------------------------------------------------------------------

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) (any, error) {
	st, err := s.db.Stats()
	if err != nil {
		return nil, err
	}
	counts, _ := s.db.TableCounts()
	return map[string]any{
		"ok":           true,
		"stats":        st,
		"strategy":     s.strategy,
		"marginText":   s.strategy.MarginText(),
		"bars":         service.SupportedBars,
		"dbPath":       s.db.Path(),
		"tables":       counts,
		"uptimeSec":    int(time.Since(s.startAt).Seconds()),
		"queueLen":     s.bf.QueueLen(),
		"backfillDays": s.bf.Config().Days,
		"version":      "okx-web/1.0 (pure go)",
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

	lev := s.strategy.Entry.Leverage
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

func (s *Server) handleKline(w http.ResponseWriter, r *http.Request) (any, error) {
	q := r.URL.Query()
	inst := q.Get("inst")
	bar := q.Get("bar")
	days := atoiDefault(q.Get("days"), s.bf.Config().Days)
	limit := atoiDefault(q.Get("limit"), 0)
	auto := q.Get("auto") != "0" // 默认自动按需回补

	if inst == "" || !service.IsSupportedBar(bar) {
		return nil, fmt.Errorf("参数不合法：inst=%q bar=%q（周期只支持 %v）", inst, bar, service.SupportedBars)
	}

	// 不够一个月就排队去补（异步，不阻塞这次请求）
	cov, _ := s.db.Coverage(inst, bar)
	needBackfill := false
	if auto {
		if cov.Count == 0 || cov.Days < float64(days)-0.5 {
			needBackfill = s.bf.Enqueue(inst, bar)
		}
	}

	fromTs := time.Now().AddDate(0, 0, -days).UnixMilli()
	rows, err := s.db.QueryKlines(model.KlineQuery{InstID: inst, Bar: bar, FromTs: fromTs, Limit: limit})
	if err != nil {
		return nil, err
	}
	// 本地不足时，兜底直接从 OKX 现拉最新一批，保证图不空
	if len(rows) == 0 {
		if latest, ferr := s.feed.FetchCandles(inst, bar, 300); ferr == nil && len(latest) > 0 {
			_, _ = s.db.UpsertKlines(latest)
			rows = latest
			cov.Count = int64(len(latest))
		}
	}

	return map[string]any{
		"ok":          true,
		"inst":        inst,
		"bar":         bar,
		"days":        days,
		"count":       len(rows),
		"coverage":    cov,
		"backfilling": needBackfill,
		"list":        rows,
	}, nil
}

// ---------------------------------------------------------------------------
// /api/mark —— K 线 + 均线 + 布林 + MACD（前端画指标用，一次拿全）
// ---------------------------------------------------------------------------

func (s *Server) handleMark(w http.ResponseWriter, r *http.Request) (any, error) {
	q := r.URL.Query()
	inst := q.Get("inst")
	bar := q.Get("bar")
	days := atoiDefault(q.Get("days"), s.bf.Config().Days)
	if inst == "" || !service.IsSupportedBar(bar) {
		return nil, fmt.Errorf("参数不合法：inst=%q bar=%q", inst, bar)
	}
	fromTs := time.Now().AddDate(0, 0, -days).UnixMilli()
	rows, err := s.db.QueryKlines(model.KlineQuery{InstID: inst, Bar: bar, FromTs: fromTs})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		if latest, ferr := s.feed.FetchCandles(inst, bar, 300); ferr == nil && len(latest) > 0 {
			_, _ = s.db.UpsertKlines(latest)
			rows = latest
		}
	}

	closes := make([]float64, len(rows))
	for i, k := range rows {
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
	toPts := func(vals []float64) []pt {
		out := []pt{}
		for i, v := range vals {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				continue
			}
			out = append(out, pt{Ts: rows[i].Ts, V: v})
		}
		return out
	}

	return map[string]any{
		"ok":      true,
		"inst":    inst,
		"bar":     bar,
		"count":   len(rows),
		"kline":   rows,
		"ma7":     toPts(ma7),
		"ma25":    toPts(ma25),
		"ma99":    toPts(ma99),
		"bollUp":  toPts(bollUp),
		"bollMid": toPts(bollMid),
		"bollLo":  toPts(bollLo),
	}, nil
}
