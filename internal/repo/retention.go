package repo

// retention.go —— 30 天数据保留红线（自动删除程序的数据层）
//
// ---------------------------------------------------------------------------
// 用户口径（2026-10-01）
// ---------------------------------------------------------------------------
// 「只保留最近 30 天的数据，包括不限于历史仓位、交易记录、交易信号、盈利记录等，
//   和 30 天的日志记录。超过 30 天的数据、交易信号和 30 天的日志记录将有
//   自动数据删除程序删除这些东西。」
//
// 于是这里把「哪些表按哪一列裁」写成一张显式的清单（retentionSpecs），
// 每一条都对应一句人话，不做「按表名猜时间列」这种会在半夜删错东西的事。
//
// ---------------------------------------------------------------------------
// 为什么必须分批 DELETE ... LIMIT
// ---------------------------------------------------------------------------
// equity 每 3 秒写一条，一天 2.88 万行；signals 一天几千行。
// 一次 `DELETE WHERE ts < ?` 要开一个长事务、持大量行锁、binlog 写爆，
// 在 2 核 / 2GB 的机器上足以把 MySQl 卡住好几分钟。
// 改成每批 2000 行、批间让出 20ms，单批事务秒级完成，
// 期间新数据照常写入，对线上完全无感。
//
// ---------------------------------------------------------------------------
// 安全边界（刻意不做的事）
// ---------------------------------------------------------------------------
//   · trade 只删 status='closed' —— 持仓中的仓位哪怕开了 60 天也不能删，
//     删了引擎就找不到它，永远平不掉。
//   · inst / ticker / meta / backfill_job / signal_scan_state / kline_bench
//     不进清单：这些是「当前状态」，不是「历史记录」，按时间裁会把系统裁坏。
//   · kline 不在这里做（另有 CleanupKlines 按 (合约,周期) 换算根数）。
//   · 表不存在时跳过而不是报错 —— 老库可能还没有 ai_call / pnl_point。

import (
	"strconv"
	"time"

	"finally-main/internal/conf"
)

// RetainDays **记录表**保留窗口（天）。配置里 0 / 负数一律按 30 天兜底。
func RetainDays() int {
	if c := conf.LoadConfig(); c != nil && c.Store != nil && c.Store.RetainDays > 0 {
		return c.Store.RetainDays
	}
	return 30
}

// KlineRetainDays **K 线**保留窗口（天）。
//
// ★ 2026-10-01 用户口径：「15 分钟一年数据保留」★
// 默认 365，不是 30 —— K 线是唯一值得留一年的东西（回测要用）。
// 单独一个函数是因为它和 RetainDays 是两条独立红线：
// trade/equity 那类记录表 30 天就够，K 线要一年。
func KlineRetainDays() int {
	if c := conf.LoadConfig(); c != nil && c.Store != nil && c.Store.KlineRetainDays > 0 {
		return c.Store.KlineRetainDays
	}
	return 365
}

// LogRetainDays 日志文件保留窗口（天）。
//
// 用户口径：「每个月要清除所有超过一个月的日志记录，包括数据库、
// 客户端、网页等日志」。默认 30。
func LogRetainDays() int {
	if c := conf.LoadConfig(); c != nil && c.Store != nil && c.Store.LogRetainDays > 0 {
		return c.Store.LogRetainDays
	}
	return 30
}

// ArchiveMinFreeGB 磁盘守卫阈值（GB）。可用空间低于它就把 K 线收缩到当月。
func ArchiveMinFreeGB() int {
	if c := conf.LoadConfig(); c != nil && c.Store != nil && c.Store.ArchiveMinFreeGB > 0 {
		return c.Store.ArchiveMinFreeGB
	}
	return 10
}

// RetentionStep 一张表（或一类文件）的清理结果
type RetentionStep struct {
	Table   string `json:"table"`   // 表名 / "logs" / "kline"
	Column  string `json:"column"`  // 依据的时间列
	Days    int    `json:"days"`    // 保留天数
	Before  int64  `json:"before"`  // 清理前行数
	Deleted int64  `json:"deleted"` // 实际删除行数
	Note    string `json:"note,omitempty"`
	Err     string `json:"err,omitempty"`
}

// retentionSpec 一条清理规则：表 + 时间列 +（可选的）常量附加条件
type retentionSpec struct {
	Table  string
	Column string
	Extra  string // 追加的 WHERE 片段；只在代码里写死，绝不接外部输入
	Note   string
}

// retentionSpecs 保留窗口到期的历史数据清单。
//
// 顺序即执行顺序（大的先删，磁盘早点还回来）。
var retentionSpecs = []retentionSpec{
	// ★ trade 是「一个仓位一行」的历史仓位表。
	//   只删已平仓的：status='open' 的仓位无论多老都必须留着，
	//   否则引擎再也找不到它，永远平不掉（会一直占着保证金）。
	{"trade", "close_ts", " AND status='closed'", "历史仓位（仅已平仓）"},

	// 逐笔成交流水（开仓 / 加仓 / 平仓）。OKX 历史同步会往里灌，
	// 是增长最快的一张「记录类」表。
	{"trade_event", "ts", "", "交易记录（逐笔流水）"},

	// 八因子信号。kline_signals 的 bitmask 版存在 signals 表里。
	{"signals", "ts", "", "交易信号"},

	// 权益 / 浮盈曲线点。每 3 秒一条 → 一天 2.88 万行，是行数增长冠军。
	{"equity", "ts", "", "盈利记录（权益曲线）"},

	// 结构化运行日志（logx 同时写文件 + 写这张表）。
	{"runlog", "ts", "", "运行日志（数据库）"},

	// AI 解读的调用记录（只记元数据，不记正文）。
	{"ai_call", "ts", "", "AI 调用记录"},

	// 盈亏点（历史遗留表，目前为空，一并纳入免得以后漏）。
	{"pnl_point", "ts", "", "盈亏点"},
}

// retentionBatch 单批删除行数。2000 行 / 批是实测下来的平衡点：
// 再大 token 事务变长，再小批次数太多、每批的索引定位开销占比过高。
const retentionBatch = 2000

// retentionPause 批间让出时间，给实时写入和同步复制留出 IO。
const retentionPause = 20 * time.Millisecond

// RetentionPreview 只统计「超期数据有多少」，一行都不删。
// 给 -cleanup-dry 和网页自查用。
func (d *DB) RetentionPreview(retainDays int) ([]RetentionStep, error) {
	days, cut := normRetain(retainDays)
	out := make([]RetentionStep, 0, len(retentionSpecs))
	for _, sp := range retentionSpecs {
		if !d.tableExists(sp.Table) {
			continue
		}
		st := RetentionStep{Table: sp.Table, Column: sp.Column, Days: days, Note: sp.Note}
		q := "SELECT COUNT(*) FROM `" + sp.Table + "` WHERE `" + sp.Column + "` > 0 AND `" + sp.Column + "` < ?" + sp.Extra
		if err := d.sql.QueryRow(q, cut).Scan(&st.Before); err != nil {
			st.Err = err.Error()
		}
		out = append(out, st)
	}
	return out, nil
}

// PurgeExpired 删除所有清单表里超过 retainDays 天的行，返回逐表结果。
//
// 幂等：随时可以重复跑，删过一次之后第二次就是 0 行。
// 单表失败不中断其它表 —— 一张表删不掉不该让整个清理停摆。
func (d *DB) PurgeExpired(retainDays int) ([]RetentionStep, error) {
	days, cut := normRetain(retainDays)
	out := make([]RetentionStep, 0, len(retentionSpecs))
	var firstErr error

	for _, sp := range retentionSpecs {
		if !d.tableExists(sp.Table) {
			continue
		}
		st := RetentionStep{Table: sp.Table, Column: sp.Column, Days: days, Note: sp.Note}

		// 先数一遍（报告里能看出「本来有多少超期」）
		q := "SELECT COUNT(*) FROM `" + sp.Table + "` WHERE `" + sp.Column + "` > 0 AND `" + sp.Column + "` < ?" + sp.Extra
		_ = d.sql.QueryRow(q, cut).Scan(&st.Before)

		n, err := d.purgeBatched(sp, cut)
		st.Deleted = n
		if err != nil {
			st.Err = err.Error()
			if firstErr == nil {
				firstErr = err
			}
		}
		out = append(out, st)
	}
	return out, firstErr
}

// purgeBatched 分批删一张表，返回累计删除行数。
func (d *DB) purgeBatched(sp retentionSpec, cut int64) (int64, error) {
	q := "DELETE FROM `" + sp.Table + "` WHERE `" + sp.Column + "` > 0 AND `" +
		sp.Column + "` < ?" + sp.Extra + " LIMIT " + strconv.Itoa(retentionBatch)

	var total int64
	for {
		res, err := d.sql.Exec(q, cut)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n < retentionBatch {
			// 最后一批没满 → 已经删干净
			return total, nil
		}
		time.Sleep(retentionPause)
	}
}

// normRetain 归一化保留天数，并算出「早于此刻就算超期」的时间戳
func normRetain(days int) (int, int64) {
	if days <= 0 {
		days = 30
	}
	cut := time.Now().AddDate(0, 0, -days).UnixMilli()
	return days, cut
}

// tableExists 表是否存在（老库可能还没有 ai_call / pnl_point）。
// 一次调用会缓存，避免每张表都查一次 information_schema。
func (d *DB) tableExists(table string) bool {
	if !safeIdent(table) {
		return false
	}
	d.tblMu.Lock()
	if d.tblCache == nil {
		d.tblCache = map[string]bool{}
	}
	if v, ok := d.tblCache[table]; ok {
		d.tblMu.Unlock()
		return v
	}
	d.tblMu.Unlock()

	var n int
	err := d.sql.QueryRow(
		`SELECT COUNT(*) FROM information_schema.tables
		 WHERE table_schema=DATABASE() AND table_name=?`, table).Scan(&n)
	ok := err == nil && n > 0

	d.tblMu.Lock()
	d.tblCache[table] = ok
	d.tblMu.Unlock()
	return ok
}

// RetentionSummary 把结果压成一行人话，给日志和控制台用。
func RetentionSummary(steps []RetentionStep) (deleted int64, text string) {
	for _, s := range steps {
		deleted += s.Deleted
	}
	if deleted == 0 {
		return 0, "全部数据都在保留窗口内，无需清理"
	}
	text = "已删除 " + strconv.FormatInt(deleted, 10) + " 行超期数据："
	first := true
	for _, s := range steps {
		if s.Deleted == 0 {
			continue
		}
		if !first {
			text += " · "
		}
		first = false
		text += s.Table + " " + strconv.FormatInt(s.Deleted, 10)
	}
	return deleted, text
}
