package handler

// api_account.go —— 账户类接口：当前持仓 / 历史仓位 / 信号流水 / 权益曲线

import (
	"net/http"
	"strings"
	"time"

	"finally-main/internal/model"
	"finally-main/internal/repo"
	"finally-main/internal/service"
)

// ---------------------------------------------------------------------------
// /api/account —— 顶栏实时数字（轻量，2 秒一次）
// ---------------------------------------------------------------------------

// AccountSnapshot 账户快照：顶栏「权益 / 可用 / 本金 / 浮盈 / 总盈亏」的数据源。
//
// 本金怎么来的：假设期间没有出入金，则
//
//	权益 = 本金 + 已实现盈亏 + 未实现盈亏
//
// 反推 本金 = 权益 − 浮盈 − 已实现盈亏。有出入金时会漂，
// 那时在 configs/okx_strategy.json 里填 principal_usdt 就以内填值为准。
func (s *Server) AccountSnapshot() map[string]any {
	st, _ := s.db.Stats()
	return s.snapshot(st)
}

// snapshot 用已经取好的 stats 组装快照。
//
// 单独抽出来的原因：/api/state 自己也会调一次 Stats()，以前两边各调一次，
// 同一次请求里把 trade/signals 的聚合查询做了两遍。传进来复用即可。
func (s *Server) snapshot(st repo.Stats) map[string]any {
	eq, hasEq, _ := s.db.LatestEquity()

	principal := eq.TotalEq - eq.Upl - st.PnlTotal
	if s.strategy != nil && s.strategy.PrincipalUSDT > 0 {
		principal = s.strategy.PrincipalUSDT
	}
	// 总盈亏 = 累计已实现 + 当前浮动盈亏（有持仓时每秒都在动）。
	totalPnl := eq.Upl + st.PnlTotal
	// 今日盈亏同理：今日已实现 + 当前浮动盈亏。
	// 原来只算「今日已平仓」，没平仓就恒为 0，顶栏看着像坏了。
	todayPnl := st.TodayPnl + eq.Upl
	roi := 0.0
	if principal > 0 {
		roi = totalPnl / principal * 100
	}
	return map[string]any{
		"hasEquity":    hasEq,
		"totalEq":      eq.TotalEq,
		"avail":        eq.Avail,
		"upl":          eq.Upl,
		"posCount":     st.PosCount,
		"equityTs":     eq.Ts,
		"principal":    principal,
		"realized":     st.PnlTotal,
		"todayRealized": st.TodayPnl,
		"todayPnl":     todayPnl,
		"totalPnl":     totalPnl,
		"roi":          roi,
		"winRate":      st.WinRate,
		"tradesTotal":  st.TradesTotal,
		"signalsToday": st.SignalsToday,
		"ordersToday":  st.OrdersToday,
		"klineRows":    st.KlineRows,
		"instCount":    st.InstCount,
		"serverTime":   st.ServerTime,
	}
}

// handleAccount 只回账户快照 + 实时引擎状态，供前端 2 秒轮询顶栏。
//
// 为什么不直接让前端轮询 /api/state：那个还要顺 information_schema 数
// 每张表的行数和占用，2 秒一次纯属浪费。
func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) (any, error) {
	return map[string]any{
		"ok":      true,
		"account": s.AccountSnapshot(),
		"live":    service.LiveStatusSnapshot(),
	}, nil
}

// ---------------------------------------------------------------------------
// /api/positions —— 持仓 + 实时盈亏
// ---------------------------------------------------------------------------

// LivePosition 前端用的持仓结构（含实时行情算出来的盈亏）
type LivePosition struct {
	ID           int64   `json:"id"`
	InstID       string  `json:"instId"`
	Name         string  `json:"name"`
	Side         string  `json:"side"`
	Sz           float64 `json:"sz"`
	EntryPx      float64 `json:"entryPx"`
	Last         float64 `json:"last"`
	MarkPx       float64 `json:"markPx"`
	LiqPx        float64 `json:"liqPx"`
	Margin       float64 `json:"margin"`
	Leverage     int     `json:"leverage"`
	Notional     float64 `json:"notional"`
	Upl          float64 `json:"upl"`
	UplPct       float64 `json:"uplPct"`
	Roi          float64 `json:"roi"`
	OpenTs       int64   `json:"openTs"`
	HoldMin      float64 `json:"holdMin"`
	Bar          string  `json:"bar"`
	Score        int     `json:"score"`
	AINote       string  `json:"aiNote"`
	TakeProfitPx float64 `json:"takeProfitPx"`
	StopLossPx   float64 `json:"stopLossPx"`
}

func (s *Server) handlePositions(w http.ResponseWriter, r *http.Request) (any, error) {
	poss, err := s.db.OpenPositions()
	if err != nil {
		return nil, err
	}
	tickers, _ := s.db.ListTickers()
	priceOf := make(map[string]float64, len(tickers))
	chgOf := make(map[string]float64, len(tickers))
	for _, t := range tickers {
		priceOf[t.InstID] = t.Last
		chgOf[t.InstID] = t.ChgPct
	}
	insts, _ := s.db.ListInstruments()
	nameOf := make(map[string]string, len(insts))
	for _, it := range insts {
		nameOf[it.InstID] = it.BaseCcy + "/USDT"
	}

	tp := 0.0
	sl := 0.0
	if s.strategy != nil {
		tp = s.strategy.Exit.TakeProfitPct
		sl = s.strategy.Exit.StopLossPct
	}

	now := time.Now().UnixMilli()
	out := make([]LivePosition, 0, len(poss))
	var sumUpl, sumMargin float64
	for _, p := range poss {
		last := priceOf[p.InstID]
		if last <= 0 {
			last = p.EntryPx
		}
		lev := p.Leverage
		if lev <= 0 {
			lev = 1
		}
		dir := 1.0
		if strings.EqualFold(p.Side, "sell") || strings.EqualFold(p.Side, "short") {
			dir = -1
		}
		uplPct := 0.0
		if p.EntryPx > 0 {
			uplPct = (last/p.EntryPx - 1) * 100 * dir
		}
		notional := p.Margin * float64(lev)
		upl := notional * uplPct / 100
		roi := 0.0
		if p.Margin > 0 {
			roi = upl / p.Margin * 100
		}
		// 简易强平价：逐仓、单向做多，距开仓价 1/杠杆（含维持保证金近似）
		liq := 0.0
		if lev > 0 {
			if dir > 0 {
				liq = p.EntryPx * (1 - 1/float64(lev) + 0.005)
			} else {
				liq = p.EntryPx * (1 + 1/float64(lev) - 0.005)
			}
		}
		tpPx, slPx := 0.0, 0.0
		if tp > 0 {
			tpPx = p.EntryPx * (1 + tp/100*dir)
		}
		if sl > 0 {
			slPx = p.EntryPx * (1 - sl/100*dir)
		}
		sumUpl += upl
		sumMargin += p.Margin
		out = append(out, LivePosition{
			ID: p.ID, InstID: p.InstID, Name: nameOf[p.InstID], Side: p.Side,
			Sz: p.Sz, EntryPx: p.EntryPx, Last: last, MarkPx: last, LiqPx: liq,
			Margin: p.Margin, Leverage: lev, Notional: notional,
			Upl: upl, UplPct: uplPct, Roi: roi,
			OpenTs: p.OpenTs, HoldMin: float64(now-p.OpenTs) / 60000.0,
			Bar: p.Bar, Score: p.Score, AINote: p.AINote,
			TakeProfitPx: tpPx, StopLossPx: slPx,
		})
	}
	return map[string]any{
		"ok":        true,
		"count":     len(out),
		"sumUpl":    sumUpl,
		"sumMargin": sumMargin,
		"list":      out,
	}, nil
}

// ---------------------------------------------------------------------------
// /api/history
// ---------------------------------------------------------------------------

// handleHistory 历史仓位。支持服务端分页（page/size），统计口径覆盖全部数据。
//
// 分页为什么放在服务端：前端一次只看 20~100 行，没必要把 500 行全传下来；
// 页码翻到后面时更不该把前面所有页的数据都传一遍。
// 这里只取「当前页」，但 sumPnl / winRate 是用聚合 SQL 对整个窗口算的
// —— 否则胜率会随着翻页变化，那是明显的错。
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) (any, error) {
	days := atoiDefault(r.URL.Query().Get("days"), 3)
	var since int64
	if days > 0 {
		since = time.Now().AddDate(0, 0, -days).UnixMilli()
	}
	size := atoiDefault(r.URL.Query().Get("size"), atoiDefault(r.URL.Query().Get("limit"), 50))
	page := atoiDefault(r.URL.Query().Get("page"), 1)
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * size

	insts, _ := s.db.ListInstruments()
	nameOf := make(map[string]string, len(insts))
	for _, it := range insts {
		nameOf[it.InstID] = it.BaseCcy + "/USDT"
	}

	// 持仓中的仓位永远排在最前面，且**不受 days 过滤** ——
	// 一个仓位开了 5 天还没平，它仍然是「当前持仓」，按天数筛掉才是真的错。
	openRows, err := s.db.OpenTradesPage(500, 0)
	if err != nil {
		return nil, err
	}
	closedTotal, sumPnl, wins, err := s.db.ClosedTradesAgg(since)
	if err != nil {
		return nil, err
	}
	openTotal := int64(len(openRows))
	total := openTotal + closedTotal

	type item struct {
		model.ClosedTrade
		Name string `json:"name"`
	}
	out := make([]item, 0, size)

	// 把「持仓 + 已平仓」拼成一条逻辑列表，再从 offset 处切 size 条。
	if int64(offset) < openTotal {
		end := int64(offset) + int64(size)
		if end > openTotal {
			end = openTotal
		}
		for _, t := range openRows[offset:end] {
			if t.Status == "" {
				t.Status = "open"
			}
			out = append(out, item{ClosedTrade: t, Name: nameOf[t.InstID]})
		}
	}
	if need := size - len(out); need > 0 {
		coff := offset - int(openTotal)
		if coff < 0 {
			coff = 0
		}
		crows, err := s.db.ClosedTradesPage(since, need, coff)
		if err != nil {
			return nil, err
		}
		for _, t := range crows {
			out = append(out, item{ClosedTrade: t, Name: nameOf[t.InstID]})
		}
	}

	winRate := 0.0
	if closedTotal > 0 {
		winRate = float64(wins) / float64(closedTotal) * 100
	}

	return map[string]any{
		"ok": true, "count": len(out), "total": total,
		"page": page, "size": size, "pages": pagesOf(total, size),
		"openCount": openTotal, "closedCount": closedTotal,
		"sumPnl": sumPnl, "days": days,
		"wins": wins, "winRate": winRate, "list": out,
	}, nil
}

// pagesOf 总页数（向上取整，至少 1 页）
func pagesOf(total int64, size int) int {
	if size <= 0 {
		return 1
	}
	p := int((total + int64(size) - 1) / int64(size))
	if p < 1 {
		p = 1
	}
	return p
}

// ---------------------------------------------------------------------------
// /api/events —— 交易记录详情（开仓 / 加仓 / 平仓流水）
// ---------------------------------------------------------------------------

// handleEvents 返回最近 N 天的逐笔交易事件（服务端分页）。
//
// 和 /api/history 的区别：history 是「一个仓位一行」的汇总视图，
// events 是「一次一笔」的流水 —— 所以它能显示「这笔单子加过几次仓、
// 每次什么价、加了多少钱」，这是汇总视图给不了的。
//
// kind 参数可以只看某一类动作（open / addon / close），表头统计始终覆盖全部。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) (any, error) {
	days := atoiDefault(r.URL.Query().Get("days"), 3)
	since := int64(0)
	if days > 0 {
		since = time.Now().AddDate(0, 0, -days).UnixMilli()
	}
	size := atoiDefault(r.URL.Query().Get("size"), atoiDefault(r.URL.Query().Get("limit"), 50))
	page := atoiDefault(r.URL.Query().Get("page"), 1)
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * size
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	switch kind {
	case "open", "addon", "close":
	default:
		kind = ""
	}

	rows, err := s.db.RecentEventsPage(since, kind, size, offset)
	if err != nil {
		return nil, err
	}
	// 表头统计（买入/加仓/平仓笔数、已实现盈亏）覆盖全部数据，不受分页影响
	agg, err := s.db.RecentEventsAgg(since)
	if err != nil {
		return nil, err
	}
	var total int64
	switch kind {
	case "open":
		total, _ = agg["open"].(int64)
	case "addon":
		total, _ = agg["addon"].(int64)
	case "close":
		total, _ = agg["close"].(int64)
	default:
		total, _ = agg["total"].(int64)
	}

	insts, _ := s.db.ListInstruments()
	nameOf := make(map[string]string, len(insts))
	for _, it := range insts {
		nameOf[it.InstID] = it.BaseCcy + "/USDT"
	}
	type item struct {
		repo.TradeEventPoint
		Name string `json:"name"`
	}
	out := make([]item, 0, len(rows))
	for _, e := range rows {
		out = append(out, item{TradeEventPoint: e, Name: nameOf[e.InstID]})
	}
	return map[string]any{
		"ok": true, "count": len(out), "total": total,
		"page": page, "size": size, "pages": pagesOf(total, size),
		"days": days, "kind": kind,
		"openCount": agg["open"], "addonCount": agg["addon"], "closeCount": agg["close"],
		"pnlSum": agg["pnlSum"], "list": out,
	}, nil
}

// ---------------------------------------------------------------------------
// /api/signals
// ---------------------------------------------------------------------------

func (s *Server) handleSignals(w http.ResponseWriter, r *http.Request) (any, error) {
	size := atoiDefault(r.URL.Query().Get("size"), atoiDefault(r.URL.Query().Get("limit"), 50))
	page := atoiDefault(r.URL.Query().Get("page"), 1)
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * size

	instID := strings.TrimSpace(r.URL.Query().Get("inst"))
	var (
		rows  []model.SignalRow
		total int64
		err   error
	)
	if instID != "" {
		rows, err = s.db.SignalsByInstPage(instID, size, offset)
	} else {
		rows, err = s.db.SignalsPage(size, offset)
	}
	if err != nil {
		return nil, err
	}
	if total, err = s.db.SignalsCount(); err != nil {
		return nil, err
	}

	insts, _ := s.db.ListInstruments()
	nameOf := make(map[string]string, len(insts))
	for _, it := range insts {
		nameOf[it.InstID] = it.BaseCcy + "/USDT"
	}
	type item struct {
		model.SignalRow
		Name string `json:"name"`
	}
	out := make([]item, 0, len(rows))
	for _, s2 := range rows {
		out = append(out, item{SignalRow: s2, Name: nameOf[s2.InstID]})
	}
	return map[string]any{
		"ok": true, "count": len(out), "total": total,
		"page": page, "size": size, "pages": pagesOf(total, size),
		"inst": instID, "list": out,
	}, nil
}

// ---------------------------------------------------------------------------
// /api/pnl
// ---------------------------------------------------------------------------

// pnlPoint 权益曲线上的一个采样点
type pnlPoint struct {
	Ts      int64   `json:"ts"`
	TotalEq float64 `json:"totalEq"`
	Avail   float64 `json:"avail"`
	Upl     float64 `json:"upl"`
	PosCnt  int     `json:"posCount"`
}

// handlePnl 权益曲线数据。
//
// 参数：
//
//	days  只看最近多少天（默认 7，前端画「最近一周」；传 0 = 不限）
//	limit 最多取多少行原始快照（默认 20000）
//	max   抽稀后最多返回多少个点（默认 1500）
//
// 为什么要抽稀：引擎每 3 秒写一条权益快照，一周就是 20 万条 ——
// 直接塞给前端画图，浏览器会卡死。这里按等间隔抽，首尾必留。
func (s *Server) handlePnl(w http.ResponseWriter, r *http.Request) (any, error) {
	days := atoiDefault(r.URL.Query().Get("days"), 7)
	if r.URL.Query().Get("days") == "0" {
		days = 0
	}
	limit := atoiDefault(r.URL.Query().Get("limit"), 20000)
	maxPts := atoiDefault(r.URL.Query().Get("max"), 1500)

	sqlStr := `SELECT ts,total_eq,COALESCE(avail,0),COALESCE(upl,0),COALESCE(pos_count,0)
		 FROM equity WHERE 1=1`
	args := []any{}
	if days > 0 {
		sqlStr += " AND ts >= ?"
		args = append(args, time.Now().AddDate(0, 0, -days).UnixMilli())
	}
	sqlStr += " ORDER BY ts DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.SQL().Query(sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tmp := []pnlPoint{}
	for rows.Next() {
		var p pnlPoint
		if err := rows.Scan(&p.Ts, &p.TotalEq, &p.Avail, &p.Upl, &p.PosCnt); err != nil {
			return nil, err
		}
		tmp = append(tmp, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// DESC 取回来 → 反转成时间升序（图表要的）
	for i, j := 0, len(tmp)-1; i < j; i, j = i+1, j-1 {
		tmp[i], tmp[j] = tmp[j], tmp[i]
	}

	raw := len(tmp)
	if maxPts > 0 && len(tmp) > maxPts {
		tmp = downsamplePnl(tmp, maxPts)
	}
	return map[string]any{
		"ok": true, "count": len(tmp), "rawCount": raw, "days": days, "list": tmp,
	}, nil
}

// downsamplePnl 等间隔抽稀，首尾必留（首尾是曲线的起止点，丢了会看起来不对）。
func downsamplePnl(in []pnlPoint, maxPts int) []pnlPoint {
	n := len(in)
	out := make([]pnlPoint, 0, maxPts+1)
	step := float64(n) / float64(maxPts)
	for f := 0.0; f < float64(n); f += step {
		out = append(out, in[int(f)])
	}
	if len(out) == 0 || out[len(out)-1].Ts != in[n-1].Ts {
		out = append(out, in[n-1])
	}
	return out
}
