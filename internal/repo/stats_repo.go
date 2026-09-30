package repo

// stats_repo.go —— 前端顶部条用的汇总统计（MySQL 版）
//
// 顶部条 10 个数字，/api/account 每 2 秒拉一次、/api/state 每 10 秒拉一次。
//
// ⚠ 性能红线（2026-10-01 事故）：
//   除 kline 之外的 SUM / COUNT 都走索引，实际耗时都在毫秒级；
//   唯独 `COUNT(*) FROM kline` 是 400 万行的索引全扫，实测 7.76 秒。
//   它曾经被写在这条每 2 秒执行一次的热路径上 → 请求堆积 → 全站雪崩。
//   现在 kline 行数只读 rowcount.go 里的进程内缓存，这里永不出现全表 COUNT。
//
//   所以：**在这个函数里新增任何统计前，先确认它能走索引。**

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
	// kline 行数：只读进程内缓存（见 rowcount.go 顶部的事故复盘）。
	//
	// 绝对不要在这里退化成 `SELECT COUNT(*) FROM kline`：
	//   · information_schema 的 table_rows 在本机恒为 0（统计信息失效），
	//     老代码正是靠「估算为 0 就精确数一次」把全表扫描引进了热路径；
	//   · 400 万行全扫 7.76 秒，而这条路径每 2 秒被调一次。
	// 缓存没预热好就先显示 0，前端那格是「展示用」的，不影响任何决策。
	if n, ok := d.RowCount("kline"); ok {
		s.KlineRows = n
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
//
// 走元数据缓存：information_schema 要打开 data dictionary，实测 2.4 秒，
// 而表结构几乎不变。大表行数用 RowCount 的真实计数覆盖估算值。
func (d *DB) TableStats() ([]map[string]any, error) {
	metas, err := d.tableMeta()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(metas))
	for _, m := range metas {
		nRows := m.Rows
		// 估算值不准（本机 kline 恒为 0），能用真实缓存就用
		if real, ok := d.RowCount(m.Name); ok {
			nRows = real
		}
		out = append(out, map[string]any{
			"table":   m.Name,
			"rows":    nRows,
			"dataMB":  float64(m.DataLen) / 1048576.0,
			"indexMB": float64(m.IndexLen) / 1048576.0,
		})
	}
	return out, nil
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
