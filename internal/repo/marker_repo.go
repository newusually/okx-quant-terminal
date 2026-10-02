package repo

import (
	"strings"
	"time"
)

// marker_repo.go —— K 线图上的标注点（买入小火箭 / 卖出小绿叶）
//
// 图表要标两类东西：
//
//	信号点   signals 表里 score 够线的买入信号（画成小火箭）
//	成交点   trade 表里的开仓 / 平仓（开仓=火箭，平仓=绿叶）
//
// 两个都按「时间区间」查，因为前端是分页加载的：翻到哪一段就标哪一段，
// 不会因为要标全历史而把几万条记录一次性吐给浏览器。
//
// 时间对齐：OKX 的 K 线开盘时间必定是「周期毫秒数的整数倍」，
// 而成交时间带毫秒，直接拿去打点会落在两根 K 线之间、画不出来。
// 所以统一向下取整到 barMs 的整数倍（snapToBar）。

// SignalPoint 图上的一个信号点
type SignalPoint struct {
	Ts      int64   `json:"ts"`
	Close   float64 `json:"close"`
	Score   int     `json:"score"`
	RisePct float64 `json:"risePct"` // ★ 二十一期：触发那根的涨跌幅 (c-o)/o*100（负=跌）
	Mask    int     `json:"mask"`
	Acted   int     `json:"acted"`
	HitList string  `json:"hitList"`
	Reason  string  `json:"reason"`
}

// TradePoint 图上的一个成交点
type TradePoint struct {
	ID       int64   `json:"id"`
	Status   string  `json:"status"`
	Side     string  `json:"side"`
	Sz       float64 `json:"sz"`
	EntryPx  float64 `json:"entryPx"`
	ExitPx   float64 `json:"exitPx"`
	Margin   float64 `json:"margin"`
	Leverage int     `json:"leverage"`
	Pnl      float64 `json:"pnl"`
	PnlPct   float64 `json:"pnlPct"`
	OpenTs   int64   `json:"openTs"`
	CloseTs  int64   `json:"closeTs"`
	Reason   string  `json:"reason"`
	Bar      string  `json:"bar"`
}

// TradeEventPoint 图上 / 列表里的一个交易事件（开仓 · 加仓 · 平仓）
//
// 这是「一次一笔」的流水，和 TradePoint（trade 表的合并视图）互补：
// trade 表只知道「这个仓位最终均价多少」，它才知道「每次加仓加了多少钱、什么价」。
type TradeEventPoint struct {
	ID       int64   `json:"id"`
	InstID   string  `json:"instId"`
	Kind     string  `json:"kind"` // open / addon / close
	Ts       int64   `json:"ts"`
	Px       float64 `json:"px"`
	Sz       float64 `json:"sz"`
	Margin   float64 `json:"margin"`
	Leverage int     `json:"leverage"`
	Pnl      float64 `json:"pnl"`
	PnlPct   float64 `json:"pnlPct"`
	Score    int     `json:"score"`
	Reason   string  `json:"reason"`
}

// EventsInRange 取某合约在时间区间内的交易事件流水（K 线标记用）
func (d *DB) EventsInRange(instID string, fromTs, toTs int64) ([]TradeEventPoint, error) {
	rows, err := d.sql.Query(
		`SELECT id,inst_id,COALESCE(kind,''),COALESCE(ts,0),COALESCE(px,0),COALESCE(sz,0),
		        COALESCE(margin,0),COALESCE(leverage,0),COALESCE(pnl,0),COALESCE(pnl_pct,0),
		        COALESCE(score,0),COALESCE(reason,'')
		 FROM trade_event
		 WHERE inst_id=? AND ts>=? AND ts<=?
		 ORDER BY ts ASC LIMIT 4000`, instID, fromTs, toTs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TradeEventPoint, 0, 64)
	for rows.Next() {
		var e TradeEventPoint
		if err := rows.Scan(&e.ID, &e.InstID, &e.Kind, &e.Ts, &e.Px, &e.Sz,
			&e.Margin, &e.Leverage, &e.Pnl, &e.PnlPct, &e.Score, &e.Reason); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RecentEvents 最近一段时间的全部交易事件（历史里的「交易记录详情」用）。
//
// 走 ix_event_ts 索引，按时间倒序取，天然就是「最近 N 天」的语义。
func (d *DB) RecentEvents(sinceTs int64, limit int) ([]TradeEventPoint, error) {
	return d.RecentEventsPage(sinceTs, "", limit, 0)
}

// RecentEventsPage 交易事件分页查询。
//
// kind 为空表示不过滤；否则只取 open / addon / close 之一。
// 走 ix_event_ts（无 kind）或 ix_event_kind_ts（有 kind），都不需要 filesort。
func (d *DB) RecentEventsPage(sinceTs int64, kind string, limit, offset int) ([]TradeEventPoint, error) {
	limit, offset = pageArgs(limit, offset, 50, 5000)
	q := `SELECT id,inst_id,COALESCE(kind,''),COALESCE(ts,0),COALESCE(px,0),COALESCE(sz,0),
		        COALESCE(margin,0),COALESCE(leverage,0),COALESCE(pnl,0),COALESCE(pnl_pct,0),
		        COALESCE(score,0),COALESCE(reason,'')
		 FROM trade_event WHERE ts >= ?`
	args := []any{sinceTs}
	if kind != "" {
		q += ` AND kind = ?`
		args = append(args, kind)
	}
	q += ` ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := d.sql.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TradeEventPoint, 0, limit)
	for rows.Next() {
		var e TradeEventPoint
		if err := rows.Scan(&e.ID, &e.InstID, &e.Kind, &e.Ts, &e.Px, &e.Sz,
			&e.Margin, &e.Leverage, &e.Pnl, &e.PnlPct, &e.Score, &e.Reason); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RecentEventsCount 交易事件总数（分页要算总页数）
func (d *DB) RecentEventsCount(sinceTs int64, kind string) (int64, error) {
	if kind != "" {
		return d.countOf("trade_event", `ts >= ? AND kind = ?`, sinceTs, kind)
	}
	return d.countOf("trade_event", `ts >= ?`, sinceTs)
}

// RecentEventsAgg 按动作汇总（买入 / 加仓 / 平仓 各多少笔、已实现盈亏合计）。
//
// 这是分页列表的「表头统计」——它必须覆盖**全部**数据而不是当前页，
// 所以单独用一条聚合 SQL 算，不能在前端对当前页 reduce。
func (d *DB) RecentEventsAgg(sinceTs int64) (map[string]any, error) {
	rows, err := d.sql.Query(
		`SELECT kind, COUNT(*), COALESCE(SUM(pnl),0)
		 FROM trade_event WHERE ts >= ? GROUP BY kind`, sinceTs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]any{"open": 0, "addon": 0, "close": 0, "pnlSum": 0.0, "total": 0}
	var total int64
	for rows.Next() {
		var kind string
		var n int64
		var pnl float64
		if err := rows.Scan(&kind, &n, &pnl); err != nil {
			return nil, err
		}
		total += n
		switch kind {
		case "open", "addon", "close":
			out[kind] = n
			if kind == "close" {
				out["pnlSum"] = pnl
			}
		}
	}
	out["total"] = total
	return out, rows.Err()
}

// BackfillTradeEvents 把 trade 表里已经存在的开仓 / 平仓补进 trade_event 流水。
//
// trade_event 是后加的表，之前成交过的仓在它里面没有记录 —— 不回填的话
// 「老仓位在 K 线图上看不到买入标记」。
//
// 纯 SQL 批量搬，幂等（唯一键 inst_id+kind+ts + ON DUPLICATE KEY UPDATE），
// 启动时跑多少次结果都一样。
func (d *DB) BackfillTradeEvents() (int64, error) {
	now := time.Now().UnixMilli()

	// 开仓
	if _, err := d.sql.Exec(`
		INSERT INTO trade_event
			(inst_id,kind,ts,px,sz,margin,leverage,pnl,pnl_pct,score,reason,ord_id,trade_id,created_at)
		SELECT inst_id,'open',open_ts,entry_px,COALESCE(sz,0),COALESCE(margin,0),
		       COALESCE(leverage,0),0,0,COALESCE(score,0),COALESCE(reason,''),
		       COALESCE(ord_id,''),id,?
		FROM trade WHERE open_ts > 0
		ON DUPLICATE KEY UPDATE px=VALUES(px), sz=VALUES(sz), margin=VALUES(margin),
		                        leverage=VALUES(leverage), trade_id=VALUES(trade_id)`, now); err != nil {
		return 0, err
	}

	// 平仓
	if _, err := d.sql.Exec(`
		INSERT INTO trade_event
			(inst_id,kind,ts,px,sz,margin,leverage,pnl,pnl_pct,score,reason,ord_id,trade_id,created_at)
		SELECT inst_id,'close',close_ts,COALESCE(exit_px,0),COALESCE(sz,0),COALESCE(margin,0),
		       COALESCE(leverage,0),COALESCE(pnl,0),COALESCE(pnl_pct,0),0,
		       COALESCE(reason,''),COALESCE(ord_id,''),id,?
		FROM trade WHERE status='closed' AND close_ts > 0
		ON DUPLICATE KEY UPDATE px=VALUES(px), pnl=VALUES(pnl), pnl_pct=VALUES(pnl_pct),
		                        trade_id=VALUES(trade_id)`, now); err != nil {
		return 0, err
	}

	var n int64
	_ = d.sql.QueryRow(`SELECT COUNT(*) FROM trade_event`).Scan(&n)
	return n, nil
}

// SignalsInRange 取某合约某周期、指定时间区间内的信号
func (d *DB) SignalsInRange(instID, bar string, fromTs, toTs int64) ([]SignalPoint, error) {
	q := `SELECT ts,COALESCE(close,0),COALESCE(score,0),COALESCE(rise_pct,0),COALESCE(mask,0),
	             COALESCE(acted,0),COALESCE(hit_list,''),COALESCE(reason,'')
	      FROM signals WHERE inst_id=? AND ts>=? AND ts<=?`
	args := []any{instID, fromTs, toTs}
	if bar != "" {
		q += " AND bar=?"
		args = append(args, bar)
	}
	q += " ORDER BY ts ASC LIMIT 4000"

	rows, err := d.sql.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SignalPoint, 0, 128)
	for rows.Next() {
		var p SignalPoint
		if err := rows.Scan(&p.Ts, &p.Close, &p.Score, &p.RisePct, &p.Mask,
			&p.Acted, &p.HitList, &p.Reason); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// TradesInRange 取某合约在指定时间区间内「开过仓或平过仓」的成交。
//
// 不按 bar 过滤：一个仓位可能是在 15m 图上开的、在 1H 图上平的，
// 只要时间落在可视区间就都应该标出来，否则换个周期标记就凭空消失了。
func (d *DB) TradesInRange(instID string, fromTs, toTs int64) ([]TradePoint, error) {
	rows, err := d.sql.Query(
		`SELECT id,COALESCE(status,''),COALESCE(side,'buy'),COALESCE(sz,0),COALESCE(entry_px,0),
		        COALESCE(exit_px,0),COALESCE(margin,0),COALESCE(leverage,0),
		        COALESCE(open_ts,0),COALESCE(close_ts,0),COALESCE(pnl,0),COALESCE(pnl_pct,0),
		        COALESCE(reason,''),COALESCE(bar,'')
		 FROM trade
		 WHERE inst_id=?
		   AND ( (open_ts>=? AND open_ts<=?) OR (close_ts>=? AND close_ts<=?) )
		 ORDER BY open_ts ASC LIMIT 4000`,
		instID, fromTs, toTs, fromTs, toTs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TradePoint, 0, 64)
	for rows.Next() {
		var t TradePoint
		if err := rows.Scan(&t.ID, &t.Status, &t.Side, &t.Sz, &t.EntryPx,
			&t.ExitPx, &t.Margin, &t.Leverage, &t.OpenTs, &t.CloseTs,
			&t.Pnl, &t.PnlPct, &t.Reason, &t.Bar); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 历史信号回算的辅助查询
// ---------------------------------------------------------------------------

// TradeableInstIDs 准入通过的合约（inst.tradeable=1）。
// 历史信号只给「真能下单」的合约算，美股/ETF/新币那些算了也没人看。
func (d *DB) TradeableInstIDs() ([]string, error) {
	rows, err := d.sql.Query(`SELECT inst_id FROM inst WHERE tradeable=1 ORDER BY inst_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0, 256)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SignalWatermarks 每个 (inst,bar) 在 signals 表里已有的最大 ts。
// 回算用它当水位线：重启之后只算增量，不从零重跑（signals 有
// UNIQUE(inst_id,bar,ts)，重复算也会被 INSERT IGNORE 挡掉，但水位线省时间）。
func (d *DB) SignalWatermarks() (map[string]int64, error) {
	rows, err := d.sql.Query(`SELECT inst_id, bar, MAX(ts) FROM signals GROUP BY inst_id, bar`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var inst, bar string
		var ts int64
		if err := rows.Scan(&inst, &bar, &ts); err != nil {
			return nil, err
		}
		out[inst+"|"+bar] = ts
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 历史信号回算：双向扫描水位线
// ---------------------------------------------------------------------------

// SignalScanSpan 单个 (inst, bar) 已经扫过的 K 线时间区间（闭区间）。
//
// MinTs/MaxTs 都为 0 表示「一次都没扫过」。
// K 线回补是向左扩张的，所以只记 MaxTs 会漏掉后补进来的老 K 线；
// 两端都记，每轮只需补 [新最老, MinTs) ∪ (MaxTs, 新最新] 两段。
type SignalScanSpan struct {
	MinTs   int64
	MaxTs   int64
	Scanned int64
}

// LoadSignalScanSpans 读全部扫描水位线，key 为 "inst|bar"。
func (d *DB) LoadSignalScanSpans() (map[string]SignalScanSpan, error) {
	rows, err := d.sql.Query(`SELECT inst_id, bar, min_ts, max_ts, scanned FROM signal_scan_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]SignalScanSpan{}
	for rows.Next() {
		var inst, bar string
		var sp SignalScanSpan
		if err := rows.Scan(&inst, &bar, &sp.MinTs, &sp.MaxTs, &sp.Scanned); err != nil {
			return nil, err
		}
		out[inst+"|"+bar] = sp
	}
	return out, rows.Err()
}

// SaveSignalScanSpans 批量落盘（幂等 upsert）。只在有变化时调用。
func (d *DB) SaveSignalScanSpans(spans map[string]SignalScanSpan) error {
	if len(spans) == 0 {
		return nil
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO signal_scan_state
		(inst_id, bar, min_ts, max_ts, scanned, updated_at) VALUES (?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE min_ts=VALUES(min_ts), max_ts=VALUES(max_ts),
			scanned=VALUES(scanned), updated_at=VALUES(updated_at)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	nowMs := time.Now().UnixMilli()
	for k, sp := range spans {
		i := strings.IndexByte(k, '|')
		if i <= 0 || i == len(k)-1 {
			continue
		}
		if _, err := stmt.Exec(k[:i], k[i+1:], sp.MinTs, sp.MaxTs, sp.Scanned, nowMs); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteSignalScanSpansForInst 删掉某个合约的全部扫描水位线（让下次回算从头算）。
//
// ★ 2026-10-02 十三期新增：口径变更时必须配套使用 ★
//
// 水位线记的是「这段区间算过了」。判定门槛一变，那段区间里"当时不合格"的 K 线
// 不会重算，现象就是**改了配置毫无反应且不报错**。
// 调用点见 service.ForgetSignalScanSpans。
func (d *DB) DeleteSignalScanSpansForInst(instID string) error {
	_, err := d.sql.Exec(`DELETE FROM signal_scan_state WHERE inst_id=?`, instID)
	return err
}

// DeleteSignalsForInst 删掉某个合约的全部信号行，返回删除条数。
//
// 同样给「口径变更」用：signals 是 UNIQUE(inst_id,bar,ts) + INSERT IGNORE，
// 旧口径写下的行**永远不会被新口径覆盖**（重算时连 INSERT 的机会都没有，
// 因为水位线把它跳过了；就算重算，IGNORE 也会静默丢弃）。所以只能删。
func (d *DB) DeleteSignalsForInst(instID string) (int64, error) {
	res, err := d.sql.Exec(`DELETE FROM signals WHERE inst_id=?`, instID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
