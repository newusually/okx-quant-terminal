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
	totalPnl := eq.Upl + st.PnlTotal
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
		"todayPnl":     st.TodayPnl,
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
	limit := atoiDefault(r.URL.Query().Get("limit"), 200)
	rows, err := s.db.ClosedTrades(limit)
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
	out := make([]item, 0, len(rows))
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
		"ok": true, "count": len(out), "sumPnl": sum,
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

func (s *Server) handlePnl(w http.ResponseWriter, r *http.Request) (any, error) {
	limit := atoiDefault(r.URL.Query().Get("limit"), 2000)
	rows, err := s.db.SQL().Query(
		`SELECT ts,total_eq,COALESCE(avail,0),COALESCE(upl,0),COALESCE(pos_count,0)
		 FROM equity ORDER BY ts DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type pt struct {
		Ts      int64   `json:"ts"`
		TotalEq float64 `json:"totalEq"`
		Avail   float64 `json:"avail"`
		Upl     float64 `json:"upl"`
		PosCnt  int     `json:"posCount"`
	}
	tmp := []pt{}
	for rows.Next() {
		var p pt
		if err := rows.Scan(&p.Ts, &p.TotalEq, &p.Avail, &p.Upl, &p.PosCnt); err != nil {
			return nil, err
		}
		tmp = append(tmp, p)
	}
	for i, j := 0, len(tmp)-1; i < j; i, j = i+1, j-1 {
		tmp[i], tmp[j] = tmp[j], tmp[i]
	}
	return map[string]any{"ok": true, "count": len(tmp), "list": tmp}, nil
}
