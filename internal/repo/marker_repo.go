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

// SignalsInRange 取某合约某周期、指定时间区间内的信号
func (d *DB) SignalsInRange(instID, bar string, fromTs, toTs int64) ([]SignalPoint, error) {
	q := `SELECT ts,COALESCE(close,0),COALESCE(score,0),COALESCE(mask,0),
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
		if err := rows.Scan(&p.Ts, &p.Close, &p.Score, &p.Mask,
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
