package repo

// stats_repo.go —— 前端顶部条用的汇总统计（MySQL 版）
//
// 顶部条 10 个数字，每 30 秒拉一次。
// 单条查询都是 COUNT / SUM 走索引；kline 行数走的是聚簇索引全扫，
// 3000 万行在 InnoDB 里约 0.5~2 秒 —— 所以下面用 information_schema
// 的估算值给 kline，避免每次刷新都全表扫一遍。

import (
	"database/sql"
	"time"
)

// ---------------------------------------------------------------------------
// 统计
// ---------------------------------------------------------------------------

// Stats 汇总统计
func (d *DB) Stats() (Stats, error) {
	var s Stats
	s.ServerTime = time.Now().Format("2006-01-02 15:04:05")
	s.Ts = time.Now().UnixMilli()

	lt := time.Now()
	today0 := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, lt.Location()).UnixMilli()

	q1 := func(sqlStr string, args ...any) (float64, error) {
		var v sql.NullFloat64
		if err := d.sql.QueryRow(sqlStr, args...).Scan(&v); err != nil {
			return 0, err
		}
		return v.Float64, nil
	}
	var err error
	if s.InstCount, err = d.count("SELECT COUNT(*) FROM inst"); err != nil {
		return s, err
	}
	// kline 行数用估算值：information_schema 里的 TABLE_ROWS 对这个量级的表
	// 误差 <5%，但耗时从「秒级」降到「毫秒级」
	var est sql.NullInt64
	_ = d.sql.QueryRow(
		`SELECT table_rows FROM information_schema.tables
		 WHERE table_schema=DATABASE() AND table_name='kline'`).Scan(&est)
	s.KlineRows = est.Int64
	if s.KlineRows == 0 {
		// 估算拿不到（比如刚建表）就精确数一次
		s.KlineRows, _ = d.count("SELECT COUNT(*) FROM kline")
	}
	if s.SignalsToday, err = d.count("SELECT COUNT(*) FROM signals WHERE ts >= ?", today0); err != nil {
		return s, err
	}
	if s.OrdersToday, err = d.count("SELECT COUNT(*) FROM trade WHERE open_ts >= ?", today0); err != nil {
		return s, err
	}
	if s.PosCount, err = d.count("SELECT COUNT(*) FROM trade WHERE status='open'"); err != nil {
		return s, err
	}
	if s.TradesTotal, err = d.count("SELECT COUNT(*) FROM trade WHERE status='closed'"); err != nil {
		return s, err
	}
	if s.TodayPnl, err = q1("SELECT COALESCE(SUM(pnl),0) FROM trade WHERE status='closed' AND close_ts >= ?", today0); err != nil {
		return s, err
	}
	if s.PnlTotal, err = q1("SELECT COALESCE(SUM(pnl),0) FROM trade WHERE status='closed'"); err != nil {
		return s, err
	}
	if wins, err2 := d.count("SELECT COUNT(*) FROM trade WHERE status='closed' AND pnl > 0"); err2 == nil && s.TradesTotal > 0 {
		s.WinRate = float64(wins) / float64(s.TradesTotal) * 100
	}
	return s, nil
}

// TableStats 每张表的行数 + 占用空间（客户端「数据库」面板展示）
func (d *DB) TableStats() ([]map[string]any, error) {
	rows, err := d.sql.Query(`
		SELECT table_name,
		       COALESCE(table_rows,0),
		       COALESCE(data_length,0),
		       COALESCE(index_length,0)
		FROM information_schema.tables
		WHERE table_schema = DATABASE() AND table_type='BASE TABLE'
		ORDER BY (COALESCE(data_length,0)+COALESCE(index_length,0)) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var name string
		var nRows, dataLen, idxLen int64
		if err := rows.Scan(&name, &nRows, &dataLen, &idxLen); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"table": name, "rows": nRows,
			"dataMB":  float64(dataLen) / 1048576.0,
			"indexMB": float64(idxLen) / 1048576.0,
		})
	}
	return out, rows.Err()
}

// EquitySnap 最近一条账户权益快照（顶栏「权益 / 可用 / 浮盈」的数据源）
type EquitySnap struct {
	Ts       int64   `json:"ts"`
	TotalEq  float64 `json:"totalEq"`
	Avail    float64 `json:"avail"`
	Upl      float64 `json:"upl"`
	PosCount int     `json:"posCount"`
}

// LatestEquity 取最近一条权益快照。
//
// 引擎每隔几秒就会往 equity 表写一条（见 internal/service/live.go），
// 所以这里读到的永远是几秒内的新鲜值，顶栏可以放心按秒刷新。
// 第二条返回值表示「有没有数据」——没有和「有但是 0」要区分开。
func (d *DB) LatestEquity() (EquitySnap, bool, error) {
	var e EquitySnap
	err := d.sql.QueryRow(
		`SELECT ts,total_eq,COALESCE(avail,0),COALESCE(upl,0),COALESCE(pos_count,0)
		 FROM equity ORDER BY ts DESC LIMIT 1`).Scan(&e.Ts, &e.TotalEq, &e.Avail, &e.Upl, &e.PosCount)
	if err == sql.ErrNoRows {
		return e, false, nil
	}
	if err != nil {
		return e, false, err
	}
	return e, true, nil
}

// ---------------------------------------------------------------------------
// meta 键值表（公告黑名单缓存等）
// ---------------------------------------------------------------------------

// SetMeta 写一条元数据
func (d *DB) SetMeta(k, v string) error {
	_, err := d.sql.Exec(
		"INSERT INTO meta(k, v) VALUES(?, ?) ON DUPLICATE KEY UPDATE v=VALUES(v)", k, v)
	return err
}

// GetMeta 读一条元数据，没有则 ok=false
func (d *DB) GetMeta(k string) (string, bool, error) {
	var v string
	err := d.sql.QueryRow("SELECT v FROM meta WHERE k=?", k).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}
