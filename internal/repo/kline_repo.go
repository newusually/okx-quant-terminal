package repo

// kline_repo.go —— K 线仓储（MySQL 版）
//
// 这是全项目写入量最大的表：400+ 合约 × 6 个周期 × 30 天 ≈ 3000 万行。
// 并发写靠三件事扛住：
//   1. 多行 INSERT ... ON DUPLICATE KEY UPDATE（一次 500 行）
//   2. InnoDB 行级锁（不同合约之间完全并行，不互相阻塞）
//   3. innodb_autoinc_lock_mode=2 + 无自增主键（kline 用业务复合主键）

import (
	"database/sql"
	"strings"
)

// klineCols kline 表列序（批量写入用）
var klineCols = []string{"inst_id", "bar", "ts", "o", "h", "l", "c", "v"}

// klineUpdateCols 冲突（同一合约+周期+时间戳）时覆盖的字段
var klineUpdateCols = []string{"o", "h", "l", "c", "v"}

// ---------------------------------------------------------------------------
// 写
// ---------------------------------------------------------------------------

// UpsertKlines 批量写入 K 线
//
// 同一批里不同合约的数据混在一起也没关系，一次网络往返全部落库。
func (d *DB) UpsertKlines(rows []Kline) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	args := make([][]any, 0, len(rows))
	for _, r := range rows {
		args = append(args, []any{r.InstID, r.Bar, r.Ts, r.O, r.H, r.L, r.C, r.V})
	}
	return d.bulkUpsert("kline", klineCols, args, klineUpdateCols)
}

// ---------------------------------------------------------------------------
// 读
// ---------------------------------------------------------------------------

// QueryKlines 查 K 线，返回按时间升序（老 → 新），直接喂 TradingView。
func (d *DB) QueryKlines(q KlineQuery) ([]Kline, error) {
	sb := strings.Builder{}
	sb.WriteString("SELECT inst_id,bar,ts,o,h,l,c,v FROM kline WHERE 1=1")
	args := []any{}
	if q.InstID != "" {
		sb.WriteString(" AND inst_id=?")
		args = append(args, q.InstID)
	}
	if q.Bar != "" {
		sb.WriteString(" AND bar=?")
		args = append(args, q.Bar)
	}
	if q.FromTs > 0 {
		sb.WriteString(" AND ts>=?")
		args = append(args, q.FromTs)
	}
	if q.ToTs > 0 {
		sb.WriteString(" AND ts<=?")
		args = append(args, q.ToTs)
	}
	// 复合主键 (inst_id, bar, ts) 天然按 ts 有序，DESC 走反向索引扫描，很快
	sb.WriteString(" ORDER BY ts DESC")
	if q.Limit > 0 {
		sb.WriteString(" LIMIT ?")
		args = append(args, q.Limit)
	}
	rows, err := d.sql.Query(sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Kline, 0, 512)
	for rows.Next() {
		var k Kline
		if err := rows.Scan(&k.InstID, &k.Bar, &k.Ts, &k.O, &k.H, &k.L, &k.C, &k.V); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 反转成升序
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// LastKlineTs 某合约某周期最新一根的时间戳（回补时判断要不要接着往前拉）
func (d *DB) LastKlineTs(instID, bar string) (int64, error) {
	var ts sql.NullInt64
	err := d.sql.QueryRow(
		`SELECT MAX(ts) FROM kline WHERE inst_id=? AND bar=?`, instID, bar).Scan(&ts)
	return ts.Int64, err
}

// FirstKlineTs 某合约某周期最老一根的时间戳
func (d *DB) FirstKlineTs(instID, bar string) (int64, error) {
	var ts sql.NullInt64
	err := d.sql.QueryRow(
		`SELECT MIN(ts) FROM kline WHERE inst_id=? AND bar=?`, instID, bar).Scan(&ts)
	return ts.Int64, err
}

// ---------------------------------------------------------------------------
// 覆盖情况
// ---------------------------------------------------------------------------

// Coverage 查单对 (合约,周期) 的覆盖范围
func (d *DB) Coverage(instID, bar string) (KlineCoverage, error) {
	c := KlineCoverage{InstID: instID, Bar: bar}
	var minTs, maxTs sql.NullInt64
	err := d.sql.QueryRow(
		`SELECT COUNT(*), MIN(ts), MAX(ts) FROM kline WHERE inst_id=? AND bar=?`,
		instID, bar).Scan(&c.Count, &minTs, &maxTs)
	if err != nil {
		return c, err
	}
	c.MinTs, c.MaxTs = minTs.Int64, maxTs.Int64
	if c.Count > 1 && c.MaxTs > c.MinTs {
		c.Days = float64(c.MaxTs-c.MinTs) / 86400000.0
	}
	return c, nil
}

// CoverageAll 所有 (合约,周期) 的覆盖列表
//
// 直接在 3000 万行上 GROUP BY 太慢（十几秒），
// 所以先拿「有数据的合约」（几百行），再按合约逐个聚合 ——
// 每次都命中 (inst_id, bar) 索引前缀，整体快一个数量级。
func (d *DB) CoverageAll() ([]KlineCoverage, error) {
	insts, err := d.ListInstrumentIDs()
	if err != nil {
		return nil, err
	}
	out := make([]KlineCoverage, 0, len(insts)*6)
	for _, inst := range insts {
		rows, err := d.sql.Query(
			`SELECT bar, COUNT(*), MIN(ts), MAX(ts) FROM kline
			 WHERE inst_id=? GROUP BY bar ORDER BY bar`, inst)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var c KlineCoverage
			var minTs, maxTs sql.NullInt64
			c.InstID = inst
			if err := rows.Scan(&c.Bar, &c.Count, &minTs, &maxTs); err != nil {
				rows.Close()
				return nil, err
			}
			c.MinTs, c.MaxTs = minTs.Int64, maxTs.Int64
			if c.Count > 1 && c.MaxTs > c.MinTs {
				c.Days = float64(c.MaxTs-c.MinTs) / 86400000.0
			}
			out = append(out, c)
		}
		rows.Close()
	}
	return out, nil
}

// ListInstrumentIDs 只取合约 ID 列表
func (d *DB) ListInstrumentIDs() ([]string, error) {
	rows, err := d.sql.Query(`SELECT inst_id FROM inst ORDER BY inst_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 滚动清理 —— 防止 3000 万行继续膨胀
// ---------------------------------------------------------------------------

// CleanupKlines 每个 (合约,周期) 只保留最新 keep 根
//
// 先定位第 keep 根的时间戳（主键倒序 + OFFSET，毫秒级），
// 再按 ts < 该值 批量删。规避 MySQL
// 「不能在 DELETE 的子查询中引用目标表」的限制。
func (d *DB) CleanupKlines(keep int) (int64, error) {
	if keep <= 0 {
		keep = 20000
	}
	insts, err := d.ListInstrumentIDs()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, inst := range insts {
		bars, err := d.barsOf(inst)
		if err != nil {
			return total, err
		}
		for _, bar := range bars {
			var cut sql.NullInt64
			err := d.sql.QueryRow(
				`SELECT ts FROM kline WHERE inst_id=? AND bar=? ORDER BY ts DESC LIMIT 1 OFFSET ?`,
				inst, bar, keep-1).Scan(&cut)
			if err == sql.ErrNoRows {
				continue // 还不到 keep 根，不清理
			}
			if err != nil {
				return total, err
			}
			res, err := d.sql.Exec(
				`DELETE FROM kline WHERE inst_id=? AND bar=? AND ts<?`, inst, bar, cut.Int64)
			if err != nil {
				return total, err
			}
			n, _ := res.RowsAffected()
			total += n
		}
	}
	return total, nil
}

// barsOf 某合约在库里有哪些周期（主键前缀查询，很快）
func (d *DB) barsOf(instID string) ([]string, error) {
	rows, err := d.sql.Query(`SELECT DISTINCT bar FROM kline WHERE inst_id=?`, instID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// CleanupBench 压测清理：删掉早于 cutTs 的 K 线（cmd/okxbench 用）
func (d *DB) CleanupBench(cutTs int64) (int64, error) {
	res, err := d.sql.Exec(`DELETE FROM kline WHERE ts < ?`, cutTs)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
