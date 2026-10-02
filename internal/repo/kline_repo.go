package repo

// kline_repo.go —— K 线仓储（MySQL 版）
//
// 这是全项目写入量最大的表：479 合约 × 1 个周期（15m）× 365 天 ≈ 1700 万行。
// 并发写靠三件事扛住：
//   1. 多行 INSERT ... ON DUPLICATE KEY UPDATE（一次 500 行）
//   2. InnoDB 行级锁（不同合约之间完全并行，不互相阻塞）
//   3. innodb_autoinc_lock_mode=2 + 无自增主键（kline 用业务复合主键）
//
// 表是 RANGE COLUMNS(ts) 分区表（冷区按月 + 热区按周 + p_old/pmax 兜底），
// 所以过期数据走 DROP PARTITION 秒删 —— 见本文件末尾「分区级删除」。

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"finally-main/internal/logx"
	"finally-main/internal/model"
)

// klineCols kline 表列序（批量写入用）
var klineCols = []string{"inst_id", "bar", "ts", "o", "h", "l", "c", "v"}

// klineUpdateCols 冲突（同一合约+周期+时间戳）时覆盖的字段
var klineUpdateCols = []string{"o", "h", "l", "c", "v"}

// KlineRejected 累计被合法性校验拦下的行数。
//
// 背景：曾经有个压测工具把 28.68 万根「合成 K 线」（价格 ~740、时间戳带毫秒尾巴）
// 直接写进了生产 kline 表，把每个合约的 MA25/MA99/布林带全部算歪。
// 现在所有写入都必须先过 IsValidKline，拦下的行数在这里累计，接口层可以报出来。
var KlineRejected atomic.Int64

// IsValidKline 一根 K 线是否可信。
//
// 不变量（OKX 的数据天然满足）：
//   - bar 开盘时间戳必定对齐到整秒（毫秒位为 0）—— 合成/压测数据必带毫秒尾巴
//   - 开高低收都必须 > 0，且 high >= low
func IsValidKline(k Kline) bool {
	if k.InstID == "" || k.Bar == "" {
		return false
	}
	if k.Ts <= 0 || k.Ts%1000 != 0 {
		return false
	}
	if k.O <= 0 || k.H <= 0 || k.L <= 0 || k.C <= 0 {
		return false
	}
	if k.H < k.L {
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// 写
// ---------------------------------------------------------------------------

// UpsertKlines 批量写入 K 线
//
// 同一批里不同合约的数据混在一起也没关系，一次网络往返全部落库。
func (d *DB) UpsertKlines(rows []Kline) (int, error) {
	return d.UpsertKlinesInto("kline", rows)
}

// UpsertKlinesInto 写进指定表（压测走 kline_bench，永远碰不到生产数据）
func (d *DB) UpsertKlinesInto(table string, rows []Kline) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	args := make([][]any, 0, len(rows))
	bad := int64(0)
	for _, r := range rows {
		if !IsValidKline(r) {
			bad++
			continue
		}
		args = append(args, []any{r.InstID, r.Bar, r.Ts, r.O, r.H, r.L, r.C, r.V})
	}
	if bad > 0 {
		KlineRejected.Add(bad)
	}
	if len(args) == 0 {
		return 0, nil
	}
	return d.bulkUpsert(table, klineCols, args, klineUpdateCols)
}

// PurgeBadKlines 清掉不符合不变量的脏行，返回删除行数。
//
// 启动时跑一次，属于「自愈」：就算有别的工具又塞了脏数据进来，重启就能清干净。
//
// ★ 这个 DELETE 用不上任何索引（ts % 1000 是函数表达式），每次都是全表扫描。
// kline 涨到 400 万行后一次要跑几十秒，还是长事务 + 大量 undo/binlog。所以：
//   - 分批删（每批 2000 行，最多 20 批），把长事务切成小事务；
//   - 调用方必须放后台 goroutine，绝不能挡在 HTTP 监听前面 ——
//     以前是同步调，服务要等它跑完才 ListenAndServe，看起来就像「启动失败」。
func (d *DB) PurgeBadKlines() (int64, error) {
	const batch, maxRounds = 2000, 20
	var total int64
	for i := 0; i < maxRounds; i++ {
		res, err := d.sql.Exec(
			`DELETE FROM kline
			 WHERE ts <= 0 OR ts % 1000 <> 0
			    OR o <= 0 OR h <= 0 OR l <= 0 OR c <= 0
			 LIMIT ?`, batch)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < batch {
			break // 没删满一批，说明已经干净了
		}
	}
	return total, nil
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
	// 复合主键 (inst_id, bar, ts) 天然按 ts 有序：
	//   DESC（默认）走反向索引扫描，取「最新的 N 根」很快；
	//   Asc（七期，右移翻页用）走正向索引扫描，LIMIT 直接落在窗口里最老的 N 根。
	// 两种排序最后都反转成升序返回 —— 调用方永远拿到升序。
	if q.Asc {
		sb.WriteString(" ORDER BY ts ASC")
	} else {
		sb.WriteString(" ORDER BY ts DESC")
	}
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
	// 反转成升序。
	// ★ 只对 DESC 查询反转（DESC 取「最新的 N 根」需要倒过来）；
	//   Asc=true 查询本来就是升序，再反转就变成降序 —— 七期右移翻页
	//   的 Rows 会被弄成倒序，FirstTs/LastTs 全部反义。
	if !q.Asc {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
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

// CleanupKlines 每个 (合约,周期) 只保留最近 keepDays 天。
//
// 为什么按「天」而不是按「根数」：
//
//	5m 一天 288 根、4H 一天只有 6 根，同一个根数对这两个周期是完全不同的时间跨度。
//	按固定根数裁（原来的 30000）会得到畸形结果，所以这里按周期把天换算成根数。
//
// 实现：先定位第 keep 根的时间戳（主键倒序 + OFFSET），再按 ts < 该值 批量删。
// 规避 MySQL「不能在 DELETE 的子查询中引用目标表」的限制。
//
// ★ 2026-10-01 两处调整 ★
//
//  1. 余量从「×6/5 + 2 天（= 38 天）」收到「+1 天」。
//     原来留 20%+2 天是为了「每个周期至少覆盖一个月」，但用户的口径已经
//     明确成「只保留最近 30 天的数据」——38 天等于白存 27% 的行。
//     回补侧同步改成写入前 trimToWindow(cfg.Days=30)，两边对齐后保留量 ≈30 天。
//
//  2. 顺手清掉「当前不支持的周期」。库里若还躺着历史遗留的整段周期
//     （例如旧版本回补进去的 5m），`barsOf` 会把它列出来，按上面那套
//     「保留 N 根」的逻辑它会被一直留着 —— 既不展示又白占磁盘。
//     这里先按白名单整体删一次，再走逐合约裁剪。
func (d *DB) CleanupKlines(keepDays int) (int64, error) {
	if keepDays <= 0 {
		keepDays = 30
	}
	keepDays = keepDays + 1

	var total int64

	// ---- 0) 先干掉不在白名单里的周期（整段删掉，不留尾巴）----
	{
		rows, err := d.sql.Query(`SELECT DISTINCT bar FROM kline`)
		if err == nil {
			var stale []string
			for rows.Next() {
				var b string
				if err := rows.Scan(&b); err != nil {
					break
				}
				if !model.BarEnabled(b) {
					stale = append(stale, b)
				}
			}
			rows.Close()
			for _, b := range stale {
				// 分批删：一次性 DELETE 十几万行会开一个长事务，
				// 把 undo/binlog 再撑一遍，也容易卡住 2 核机器上的交易循环。
				var n int64
				for {
					res, err := d.sql.Exec(`DELETE FROM kline WHERE bar=? LIMIT 2000`, b)
					if err != nil {
						break
					}
					aff, _ := res.RowsAffected()
					n += aff
					if aff < 2000 {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				if n > 0 {
					total += n
					logx.Logf("INFO", "[CLEAN] 已下线周期 %s：删除残留 K 线 %d 行", b, n)
				}
			}
		}
	}

	insts, err := d.ListInstrumentIDs()
	if err != nil {
		return total, err
	}
	for _, inst := range insts {
		bars, err := d.barsOf(inst)
		if err != nil {
			return total, err
		}
		for _, bar := range bars {
			keep := barsPerDay(bar) * keepDays
			if keep <= 0 {
				continue // 不认识的周期，不动它
			}
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

// ---------------------------------------------------------------------------
// 分区级删除（2026-10-01 新增）
// ---------------------------------------------------------------------------
//
// 为什么需要它：K 线保留期从 30 天扩到 1 年后，`DELETE FROM kline WHERE ts < ?`
// 要扫掉上千万行 —— 长事务、undo/binlog 暴涨，在 2 核 2G 的机器上足以把
// 实时写入卡住好几分钟。
//
// 而 kline 是 RANGE COLUMNS(ts) 分区表，**整段过期的分区可以直接
// `ALTER TABLE ... DROP PARTITION`** —— 走的是「删数据文件」而不是「逐行删」，
// 上百万行也是毫秒级，且不产生 undo/binlog。
//
// 分两步走：
//  1. DropKlinePartitionsBefore —— 把上界 ≤ cutoff 的分区整体扔掉（快路径）
//  2. PurgeKlineBefore          —— 边界分区 + p_old 里的残余行，分批 DELETE 兜底
//
// 为什么不连 p_old 一起 DROP：p_old 是「早于第一个正经分区」的兜底垃圾桶，
// 结构上必须留着（否则将来回补更早的数据会报 "no partition for value"）。
// 所以它里面的数据只能 DELETE，不能连桶扔掉。

// PartitionDropResult 一次分区删除的结果
type PartitionDropResult struct {
	Dropped []string `json:"dropped"` // 被 DROP 的分区名
	Rows    int64    `json:"rows"`    // 这些分区的行数（information_schema 估算值）
	MB      float64  `json:"mb"`      // 释放的磁盘（估算）
}

// DropKlinePartitionsBefore 把「所有数据都早于 cutMs」的分区整套删掉。
//
// 只认「分区上界 ≤ cutMs」的分区，所以**永远不会误删保留窗口内的数据** ——
// 上界 100% 落在 cutoff 之前，才说明这个分区里的每一行都过期了。
func (d *DB) DropKlinePartitionsBefore(cutMs int64) (*PartitionDropResult, error) {
	res := &PartitionDropResult{}
	if cutMs <= 0 {
		return res, nil
	}
	pi, err := d.inspectPartition("kline")
	if err != nil {
		return res, err
	}
	if !pi.Partitioned {
		return res, nil // 没分区，交给 PurgeKlineBefore 分批删
	}

	// ★ 判据与 service 层 dry-run 预告**共用** DroppablePartitions ★
	//   原来这里和预告各写了一遍，结果预告多报了一个 p_old（见该函数注释）。
	res.Dropped = DroppablePartitions(toPartRanges(pi.Ranges), cutMs)
	if len(res.Dropped) == 0 {
		return res, nil
	}

	// 先统计（估算值）—— 要在 DROP 之前查，之后分区就没了。
	// SUM() 返回 DECIMAL，驱动给的是 []byte，所以显式 CAST AS SIGNED，
	// 否则 Scan 到 int64 会报 "converting driver.Value type []uint8 to int64"。
	q := `SELECT CAST(COALESCE(SUM(table_rows),0) AS SIGNED),
	             CAST(COALESCE(SUM(data_length+index_length),0) AS SIGNED)
	      FROM information_schema.partitions
	      WHERE table_schema=DATABASE() AND table_name='kline' AND partition_name IN (?` +
		strings.Repeat(",?", len(res.Dropped)-1) + `)`
	args := make([]any, len(res.Dropped))
	for i, n := range res.Dropped {
		args[i] = n
	}
	var mbBytes int64
	_ = d.sql.QueryRow(q, args...).Scan(&res.Rows, &mbBytes)
	res.MB = float64(mbBytes) / 1048576

	stmt := "ALTER TABLE kline DROP PARTITION " + strings.Join(res.Dropped, ", ")
	if _, err := d.sql.Exec(stmt); err != nil {
		return res, err
	}
	logx.Logf("INFO", "[KLINE] 分区级删除：DROP %s（约 %.0f 万行 / %.0f MB）",
		strings.Join(res.Dropped, ", "), float64(res.Rows)/10000, res.MB)
	return res, nil
}

// PurgeKlineBefore 分批删掉 ts < cutMs 的残余行（p_old 与边界分区里的）。
//
// 和分区删除配合使用：分区能整段扔掉的部分走 DROP PARTITION，
// 剩下「一半在窗口内、一半在窗口外」的边界分区只能逐行删。
// 每批 5000 行、批间让 20ms，长事务被切成小事务，实时写入不受影响。
func (d *DB) PurgeKlineBefore(cutMs int64) (int64, error) {
	if cutMs <= 0 {
		return 0, nil
	}
	const batch = 5000
	var total int64
	for round := 0; round < 4000; round++ { // 上限 2000 万行/轮，防死循环
		r, err := d.sql.Exec(`DELETE FROM kline WHERE ts < ? LIMIT `+strconv.Itoa(batch), cutMs)
		if err != nil {
			return total, err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n < batch {
			return total, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return total, nil
}

// KlineMonth 一个月的数据概况（归档脚本与面板都用）
type KlineMonth struct {
	Month   string `json:"month"`   // 2026-09
	Rows    int64  `json:"rows"`    //
	Insts   int64  `json:"insts"`   // 涉及多少个合约
	FirstTs int64  `json:"firstTs"` //
	LastTs  int64  `json:"lastTs"`  //
}

// KlineMonths 按月统计 K 线。GROUP BY 走的是分区裁剪 + ts 上的索引，
// 在本机 137 万行上约 1 秒；一年 1700 万行约 10 秒。
// 只在归档/月度任务里调用，不在任何轮询路径上。
func (d *DB) KlineMonths() ([]KlineMonth, error) {
	rows, err := d.sql.Query(`
		SELECT DATE_FORMAT(FROM_UNIXTIME(ts/1000), '%Y-%m') AS ym,
		       COUNT(*), COUNT(DISTINCT inst_id), MIN(ts), MAX(ts)
		FROM kline GROUP BY ym ORDER BY ym`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KlineMonth{}
	for rows.Next() {
		var m KlineMonth
		if err := rows.Scan(&m.Month, &m.Rows, &m.Insts, &m.FirstTs, &m.LastTs); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// KlineSpan 全表时间跨度与行数（/api/state 展示保留窗口实际生效情况）
func (d *DB) KlineSpan() (rowsN, minTs, maxTs int64, err error) {
	err = d.sql.QueryRow(`SELECT COUNT(*), COALESCE(MIN(ts),0), COALESCE(MAX(ts),0) FROM kline`).
		Scan(&rowsN, &minTs, &maxTs)
	return
}

// StreamKlines 把 [fromMs, toMs) 区间内、**指定周期**的 K 线流式逐行喂给 fn。
//
// bar 传空串 = 不过滤（一次遍历所有周期）。
//
// 为什么要带 bar 过滤：归档要**按周期分文件**（kline-<bar>-<ym>.partNN.csv.gz）。
// 一次遍历把四个周期混在一个文件里虽然 CSV 合法（有 bar 列），
// 但文件名就没法用周期命名，将来想单独拿 1m 也要先全量解压再筛。
// 分区表上 `AND bar = ?` 不额外增加扫描代价：分区裁剪仍由 ts 条件决定，
// bar 只在命中的分区内部过滤。
//
// 为什么不用 QueryKlines：那个接口会把整个结果集读进内存再返回。
// 归档一个月是几十万到上百万行（未来会是单次上千万行的窗口），
// 一次性装进内存再序列化，在 2GB 内存的机器上会直接把进程顶死。
//
// 这个版本底层是 `sql.Rows` 游标，一次只在内存里放一行。
// fn 返回非 nil 错误会立刻中止遍历并把错误透传出去。
func (d *DB) StreamKlines(fromMs, toMs int64, bar string, fn func(Kline) error) error {
	q := `SELECT inst_id, bar, ts, o, h, l, c, v FROM kline
		 WHERE ts >= ? AND ts < ?`
	args := []any{fromMs, toMs}
	if b := strings.TrimSpace(bar); b != "" {
		q += ` AND bar = ?`
		args = append(args, b)
	}
	q += ` ORDER BY inst_id, ts`

	rows, err := d.sql.Query(q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k Kline
		if err := rows.Scan(&k.InstID, &k.Bar, &k.Ts, &k.O, &k.H, &k.L, &k.C, &k.V); err != nil {
			return err
		}
		if err := fn(k); err != nil {
			return err
		}
	}
	return rows.Err()
}

// StreamMonth 便捷包装：流式遍历某个自然月（`2026-09`）指定周期的 K 线。
// bar 传空串 = 不过滤。
func (d *DB) StreamMonth(ym, bar string, fn func(Kline) error) error {
	from, to, err := MonthRange(ym)
	if err != nil {
		return err
	}
	return d.StreamKlines(from, to, bar, fn)
}

// MonthRange 把 `2026-09` 解析成 [月初, 下月初) 的毫秒时间戳区间。
func MonthRange(ym string) (from, to int64, err error) {
	t, err := time.ParseInLocation("2006-01", strings.TrimSpace(ym), time.Local)
	if err != nil {
		return 0, 0, fmt.Errorf("月份格式应为 YYYY-MM，收到 %q", ym)
	}
	return t.UnixMilli(), t.AddDate(0, 1, 0).UnixMilli(), nil
}

// barsPerDay 一个周期一天有多少根 K 线（OKX 口径，7×24 小时不停地开盘）
func barsPerDay(bar string) int {
	switch strings.ToLower(strings.TrimSpace(bar)) {
	case "1m":
		return 1440
	case "3m":
		return 480
	case "5m":
		return 288
	case "15m":
		return 96
	case "30m":
		return 48
	case "1h":
		return 24
	case "2h":
		return 12
	case "4h":
		return 6
	case "6h":
		return 4
	case "12h":
		return 2
	case "1d":
		return 1
	}
	return 0
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
// CleanupBench 清空压测表。压测只能写 kline_bench，这里整表删掉。
//
// 老实现的坑：它按 `ts < now-(bars+10)*60000` 删生产表 kline，而压测写进去的
// 时间戳恰好都落在这个区间「之后」，条件永远不成立 —— 一行都没删掉，
// 28.68 万行合成数据就那样留在生产库里污染了所有均线和布林带。
func (d *DB) CleanupBench() (int64, error) {
	res, err := d.sql.Exec(`DELETE FROM kline_bench`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
