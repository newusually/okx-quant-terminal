package repo

// indexes.go —— 全库二级索引的统一维护
//
// ---------------------------------------------------------------------------
// 为什么单开一个文件
// ---------------------------------------------------------------------------
// 老做法是把 KEY 直接写在 CREATE TABLE 里。那只对「新库」有效 ——
// 已经跑起来的库，表早就存在了，CREATE TABLE IF NOT EXISTS 直接跳过，
// 新加的索引一辈子也建不上。于是线上库和新库的索引悄悄分叉，
// 同一个查询在两台机器上耗时差几十倍。
//
// 这里改成「声明式 + 幂等应用」：启动时逐条比对 information_schema.statistics，
// 缺哪个建哪个、列不对就重建。对存量库和新库效果完全一致。
//
// ---------------------------------------------------------------------------
// 索引怎么定的（每条都对应一条真实 SQL，没有拍脑袋加的）
// ---------------------------------------------------------------------------
// 注意 kline 故意**没有**任何二级索引：
//   kline 上所有查询都带 inst_id（或 inst_id+bar）前缀，主键
//   (inst_id, bar, ts) 已经是最优路径。再建一个二级索引只有坏处 ——
//   400 万行要多占 ~70MB 磁盘，写入还要多维护一棵 B+ 树。

import (
	"fmt"
	"strings"
)

// idxDef 一条索引定义
type idxDef struct {
	Table  string
	Name   string
	Unique bool
	Cols   []string
	// Note 记录「为什么需要它」，对应哪条 SQL。给后来的人（和自己）省一次考古。
	Note string
}

// indexPlan 全库索引清单。
//
// 顺序无所谓，应用时会逐条比对。
var indexPlan = []idxDef{
	// ---------------------------------------------------------------- signals
	// SignalsInRange（K 线图上的信号标记，热路径）：
	//   SELECT ... FROM signals WHERE inst_id=? AND ts>=? AND ts<=?
	// 已有的 uk_signal 是 (inst_id, bar, ts)，中间夹着 bar，没法支持
	// 「只按 inst_id + ts 区间」的检索 —— 只能退化成 inst_id 全扫再过滤 ts。
	{Table: "signals", Name: "ix_sig_inst_ts", Cols: []string{"inst_id", "ts"},
		Note: `SELECT ... FROM signals WHERE inst_id=? AND ts BETWEEN ? AND ?`},

	// 全局「最近 N 条信号」列表（/api/signals）走 ts DESC，
	// 已有 ix_signal_ts 够用；但「按合约看历史信号」需要 inst_id 前导。
	{Table: "signals", Name: "ix_sig_inst_bar_ts", Cols: []string{"inst_id", "bar", "ts"},
		Note: `SELECT ... FROM signals WHERE inst_id=? AND bar=? ORDER BY ts`},

	// 二十一期：按周期的全局信号统计 / 回补前后审计（用户「可以做索引 这样快」）。
	//   SELECT ... FROM signals WHERE bar=? AND ts BETWEEN ? AND ?
	{Table: "signals", Name: "ix_sig_bar_ts", Cols: []string{"bar", "ts"},
		Note: `WHERE bar=? AND ts BETWEEN ? AND ?（按周期跨合约统计）`},

	// ------------------------------------------------------------------ trade
	// LoadOpenPositions：SELECT ... FROM trade WHERE status='open' ORDER BY open_ts
	// 已有 ix_trade_status 是 (status, inst_id)，第二列不是排序列，
	// 排序还得回表 filesort。把排序列放进索引就能直接顺着索引读。
	{Table: "trade", Name: "ix_trade_status_open_ts", Cols: []string{"status", "open_ts"},
		Note: `WHERE status='open' ORDER BY open_ts`},

	// 历史仓位列表 / 今日盈亏：
	//   WHERE status='closed' ORDER BY close_ts DESC
	//   WHERE status='closed' AND close_ts >= ?
	{Table: "trade", Name: "ix_trade_status_close_ts", Cols: []string{"status", "close_ts"},
		Note: `WHERE status='closed' ORDER BY close_ts DESC / AND close_ts >= ?`},

	// 冷却时间计算：SELECT inst_id, MAX(open_ts) FROM trade GROUP BY inst_id
	// 有了 (inst_id, open_ts) 就能走松散索引扫描，不用全表 + 临时表。
	{Table: "trade", Name: "ix_trade_inst_open_ts", Cols: []string{"inst_id", "open_ts"},
		Note: `SELECT inst_id, MAX(open_ts) FROM trade GROUP BY inst_id`},

	// OKX 历史仓位同步（okxpositions.go）的幂等查找：
	//   SELECT id FROM trade WHERE pos_id=? AND close_ts=?
	// **必须带上 close_ts**：OKX 的 posId 在 (合约,方向) 维度上是复用的，
	// 一个 posId 底下会有几十上百笔独立交易，只按 pos_id 建索引区分不开。
	{Table: "trade", Name: "ix_trade_pos_ts", Cols: []string{"pos_id", "close_ts"},
		Note: `SELECT id FROM trade WHERE pos_id=? AND close_ts=?（历史仓位同步去重）`},

	// ------------------------------------------------------------ trade_event
	// 「最近 N 天买入 / 加仓 / 平仓各多少笔」这类按动作的统计。
	// 已有 ix_event_ts 只能按时间扫全量再过滤 kind。
	{Table: "trade_event", Name: "ix_event_kind_ts", Cols: []string{"kind", "ts"},
		Note: `WHERE kind=? AND ts >= ?（按动作统计）`},

	// ------------------------------------------------------------------- inst
	// 合约下拉框 / 准入筛选：SELECT ... FROM inst WHERE tradeable=1 ORDER BY quote_vol24h DESC
	{Table: "inst", Name: "ix_inst_tradeable_vol", Cols: []string{"tradeable", "quote_vol24h"},
		Note: `WHERE tradeable=? ORDER BY quote_vol24h DESC`},

	// 品种分类筛选（加密 / 美股 / 商品）
	{Table: "inst", Name: "ix_inst_category_vol", Cols: []string{"inst_category", "quote_vol24h"},
		Note: `WHERE inst_category=? ORDER BY quote_vol24h DESC`},

	// ------------------------------------------------------------ backfill_job
	// 回补进度面板按状态 + 更新时间排
	{Table: "backfill_job", Name: "ix_job_status_updated", Cols: []string{"status", "updated_at"},
		Note: `WHERE status=? ORDER BY updated_at DESC`},
}

// applyIndexPlan 把 indexPlan 同步到数据库（幂等）。
//
// 返回实际新建/重建的索引数量，以及每条的说明，启动日志里打出来。
func (d *DB) applyIndexPlan() (int, []string, error) {
	changed := 0
	msgs := make([]string, 0, len(indexPlan))
	for _, ix := range indexPlan {
		action, err := d.syncIndex(ix)
		if err != nil {
			// 索引建不上不该拖垮整个服务：记下来继续走，
			// 大不了那条查询慢一点，总比开不了机强。
			msgs = append(msgs, fmt.Sprintf("✗ %s.%s 失败：%v", ix.Table, ix.Name, err))
			continue
		}
		if action != "" {
			changed++
			msgs = append(msgs, fmt.Sprintf("%s %s.%s(%s)",
				action, ix.Table, ix.Name, strings.Join(ix.Cols, ",")))
		}
	}
	return changed, msgs, nil
}

// syncIndex 让 table 上名为 ix.Name 的索引与定义一致。
//
// 返回 "" 表示本来就一致（没动）；"+" 新建；"~" 重建。
func (d *DB) syncIndex(ix idxDef) (string, error) {
	if !safeIdent(ix.Table) || !safeIdent(ix.Name) {
		return "", fmt.Errorf("非法标识符：%s.%s", ix.Table, ix.Name)
	}
	for _, c := range ix.Cols {
		if !safeIdent(c) {
			return "", fmt.Errorf("非法列名：%s.%s", ix.Table, c)
		}
	}
	var tn int
	if err := d.sql.QueryRow(
		`SELECT COUNT(*) FROM information_schema.tables
		 WHERE table_schema=DATABASE() AND table_name=?`, ix.Table).Scan(&tn); err != nil {
		return "", err
	}
	if tn == 0 {
		return "", nil // 表还没建（理论上不会，Init 里先跑 schemaStmts）
	}

	// 读现有索引的列顺序
	rows, err := d.sql.Query(
		`SELECT column_name FROM information_schema.statistics
		 WHERE table_schema=DATABASE() AND table_name=? AND index_name=?
		 ORDER BY seq_in_index`, ix.Table, ix.Name)
	if err != nil {
		return "", err
	}
	got := make([]string, 0, len(ix.Cols))
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			return "", err
		}
		got = append(got, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}

	if sameStrSlice(got, ix.Cols) {
		return "", nil
	}

	action := "+"
	if len(got) > 0 {
		action = "~"
		if _, err := d.sql.Exec("ALTER TABLE `" + ix.Table + "` DROP INDEX `" + ix.Name + "`"); err != nil {
			return "", err
		}
	}

	quoted := make([]string, len(ix.Cols))
	for i, c := range ix.Cols {
		quoted[i] = "`" + c + "`"
	}
	kind := "KEY"
	if ix.Unique {
		kind = "UNIQUE KEY"
	}
	_, err = d.sql.Exec("ALTER TABLE `" + ix.Table + "` ADD " + kind + " `" + ix.Name +
		"` (" + strings.Join(quoted, ",") + ")")
	if err != nil {
		return "", err
	}
	return action, nil
}

// sameStrSlice 两个字符串切片是否完全一致（顺序敏感：索引列顺序有意义）
func sameStrSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// IndexPlanSummary 给 -index 开关 / 诊断接口用：列出计划里的所有索引。
func IndexPlanSummary() []map[string]any {
	out := make([]map[string]any, 0, len(indexPlan))
	for _, ix := range indexPlan {
		out = append(out, map[string]any{
			"table": ix.Table, "name": ix.Name,
			"unique": ix.Unique, "cols": ix.Cols, "note": ix.Note,
		})
	}
	return out
}
