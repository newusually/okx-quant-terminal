package handler

// api_account.go —— 账户类接口：当前持仓 / 历史仓位 / 信号流水 / 权益曲线

import (
	"net/http"
	"strings"
	"time"

	"finally-main/internal/model"
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
	eq, hasEq, _ := s.db.LatestEquity()

	st, _ := s.db.Stats()

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

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) (any, error) {
	limit := atoiDefault(r.URL.Query().Get("limit"), 300)
	if limit > 2000 {
		limit = 2000
	}
	rows, err := s.db.ClosedTrades(limit)
	if err != nil {
		return nil, err
	}
	// 当前还持仓的仓位排在最前面：用户开完仓就能在列表里看到这一笔，
	// 而不是等平仓后才「突然出现」。它的实时盈亏由前端用行情补。
	openRows, err := s.db.OpenTrades(limit)
	if err != nil {
		return nil, err
	}
	insts, _ := s.db.ListInstruments()
	nameOf := make(map[string]string, len(insts))
	for _, it := range insts {
		nameOf[it.InstID] = it.BaseCcy + "/USDT"
	}
	type item struct {
		model.ClosedTrade
		Name string `json:"name"`
	}
	out := make([]item, 0, len(rows)+len(openRows))
	for _, t := range openRows {
		if t.Status == "" {
			t.Status = "open"
		}
		out = append(out, item{ClosedTrade: t, Name: nameOf[t.InstID]})
	}
	// 已实现的统计口径只算已平仓，不受持仓影响
	var sum float64
	wins := 0
	for _, t := range rows {
		sum += t.Pnl
		if t.Pnl > 0 {
			wins++
		}
		out = append(out, item{ClosedTrade: t, Name: nameOf[t.InstID]})
	}
	winRate := 0.0
	if len(rows) > 0 {
		winRate = float64(wins) / float64(len(rows)) * 100
	}
	return map[string]any{
		"ok": true, "count": len(out), "openCount": len(openRows),
		"closedCount": len(rows), "sumPnl": sum,
		"wins": wins, "winRate": winRate, "list": out,
	}, nil
}

// ---------------------------------------------------------------------------
// /api/signals
// ---------------------------------------------------------------------------

func (s *Server) handleSignals(w http.ResponseWriter, r *http.Request) (any, error) {
	limit := atoiDefault(r.URL.Query().Get("limit"), 100)
	rows, err := s.db.Signals(limit)
	if err != nil {
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
	return map[string]any{"ok": true, "count": len(out), "list": out}, nil
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
