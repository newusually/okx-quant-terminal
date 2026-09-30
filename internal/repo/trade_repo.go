package repo

// trade_repo.go —— 持仓 / 成交 / 信号 仓储（MySQL 版）
//
// 全是只读查询，「当前持仓 / 历史仓位 / 信号流水」三个底部 tab 的数据源。
// 表都带 status / ts 索引，几百条量级，毫秒返回。

// ---------------------------------------------------------------------------
// 持仓 / 交易 / 信号
// ---------------------------------------------------------------------------

// OpenPositions 在持仓
func (d *DB) OpenPositions() ([]OpenPosition, error) {
	rows, err := d.sql.Query(`SELECT id,inst_id,COALESCE(side,'buy'),sz,entry_px,margin,
		COALESCE(leverage,0),open_ts,COALESCE(bar,''),COALESCE(score,0),COALESCE(ai_note,''),
		COALESCE(addon_count,0),COALESCE(addon_margin,0),COALESCE(last_addon_ts,0)
		FROM trade WHERE status='open' ORDER BY open_ts DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]OpenPosition, 0, 64)
	for rows.Next() {
		var p OpenPosition
		if err := rows.Scan(&p.ID, &p.InstID, &p.Side, &p.Sz, &p.EntryPx, &p.Margin,
			&p.Leverage, &p.OpenTs, &p.Bar, &p.Score, &p.AINote,
			&p.AddonCount, &p.AddonMargin, &p.LastAddonTs); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ClosedTrades 历史仓位（已平仓）
func (d *DB) ClosedTrades(limit int) ([]ClosedTrade, error) {
	if limit <= 0 {
		limit = 200
	}
	return d.trades(limit, `status='closed'`, `close_ts DESC`)
}

// OpenTrades 当前还持仓的仓位（status=open）。
//
// 历史列表要把它们排在最前面，用户才能一眼看到「这笔还在跑」，
// 而不是开完仓之后历史区一直空着（以前就是这个毛病）。
func (d *DB) OpenTrades(limit int) ([]ClosedTrade, error) {
	return d.trades(limit, `status='open'`, `open_ts DESC`)
}

// trades 共用查询：只差一个 WHERE 条件和排序。
func (d *DB) trades(limit int, where, orderBy string) ([]ClosedTrade, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := d.sql.Query(`SELECT id,inst_id,COALESCE(side,'buy'),sz,entry_px,COALESCE(exit_px,0),
		margin,COALESCE(leverage,0),open_ts,COALESCE(close_ts,0),COALESCE(pnl,0),COALESCE(pnl_pct,0),
		COALESCE(reason,''),COALESCE(bar,''),COALESCE(ai_note,''),COALESCE(status,'closed')
		FROM trade WHERE `+where+` ORDER BY `+orderBy+` LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ClosedTrade, 0, limit)
	for rows.Next() {
		var t ClosedTrade
		if err := rows.Scan(&t.ID, &t.InstID, &t.Side, &t.Sz, &t.EntryPx, &t.ExitPx,
			&t.Margin, &t.Leverage, &t.OpenTs, &t.CloseTs, &t.Pnl, &t.PnlPct,
			&t.Reason, &t.Bar, &t.AINote, &t.Status); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Signals 最近的信号
func (d *DB) Signals(limit int) ([]SignalRow, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := d.sql.Query(`SELECT id,inst_id,bar,ts,COALESCE(close,0),COALESCE(mask,0),
		COALESCE(score,0),COALESCE(hit_list,''),COALESCE(rsi,0),COALESCE(td,0),
		COALESCE(acted,0),COALESCE(reason,''),COALESCE(ai_note,''),COALESCE(created_at,0)
		FROM signals ORDER BY ts DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SignalRow, 0, limit)
	for rows.Next() {
		var s SignalRow
		if err := rows.Scan(&s.ID, &s.InstID, &s.Bar, &s.Ts, &s.Close, &s.Mask,
			&s.Score, &s.HitList, &s.Rsi, &s.Td, &s.Acted, &s.Reason, &s.AINote, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SignalsByInst 某个合约最近的信号（前端切换下拉框时只拉这一个合约）
func (d *DB) SignalsByInst(instID string, limit int) ([]SignalRow, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := d.sql.Query(`SELECT id,inst_id,bar,ts,COALESCE(close,0),COALESCE(mask,0),
		COALESCE(score,0),COALESCE(hit_list,''),COALESCE(rsi,0),COALESCE(td,0),
		COALESCE(acted,0),COALESCE(reason,''),COALESCE(ai_note,''),COALESCE(created_at,0)
		FROM signals WHERE inst_id=? ORDER BY ts DESC, id DESC LIMIT ?`, instID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SignalRow, 0, limit)
	for rows.Next() {
		var s SignalRow
		if err := rows.Scan(&s.ID, &s.InstID, &s.Bar, &s.Ts, &s.Close, &s.Mask,
			&s.Score, &s.HitList, &s.Rsi, &s.Td, &s.Acted, &s.Reason, &s.AINote, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// PnlSeries 权益 / 盈亏曲线点（前端底部「权益曲线」tab）
func (d *DB) PnlSeries(limit int) ([]map[string]any, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := d.sql.Query(`SELECT ts,total_eq,avail,upl,pos_count FROM equity
		ORDER BY ts DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tmp := make([]map[string]any, 0, limit)
	for rows.Next() {
		var ts, posCount int64
		var totalEq, avail, upl float64
		if err := rows.Scan(&ts, &totalEq, &avail, &upl, &posCount); err != nil {
			return nil, err
		}
		tmp = append(tmp, map[string]any{
			"ts": ts, "totalEq": totalEq, "avail": avail, "upl": upl, "posCount": posCount,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 反转成时间升序
	for i, j := 0, len(tmp)-1; i < j; i, j = i+1, j-1 {
		tmp[i], tmp[j] = tmp[j], tmp[i]
	}
	return tmp, nil
}
