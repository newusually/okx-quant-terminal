package repo

// partition.go —— 大表分区：冷区按月、热区按周
//
// ---------------------------------------------------------------------------
// 为什么要分区
// ---------------------------------------------------------------------------
// kline 现在 460 万行 / 788MB，而 innodb_buffer_pool_size 只有 384MB ——
// 整表装不进内存，每次查询都要回磁盘捞冷页。分区之后：
//
//   1. 分区裁剪：`WHERE ts >= X` 只扫涉及的那几个分区，其余直接跳过，
//      扫描量降一个数量级（回补、K 线翻页、Coverage 统计都吃这个红利）
//   2. 单分区变小 → 能整个装进 384MB 的 buffer pool，热点数据全内存命中
//   3. 过期数据用 `ALTER TABLE ... DROP PARTITION` 秒级删掉，
//      不用 `DELETE FROM kline WHERE ts < ?` 扫全表（也不产生 undo/binlog 膨胀）
//   4. 统计信息按分区维护，ANALYZE 的粒度更细
//
// ---------------------------------------------------------------------------
// 为什么「按月 + 按周」两层（关键设计）
// ---------------------------------------------------------------------------
// 纯按月的问题：整月数据 402MB，**比 384MB 的 buffer pool 还大** ——
// 一个月都装不下，查「最近一小时」照样要读盘。
//
// 纯按周的问题：历史拉长之后分区数暴涨（一年 52 个）。kline 项目要求
// 「每个周期至少覆盖一个月」，但历史区间会长到一两年，52×N 个分区管理起来累。
//
// 所以分两层：
//   · 热区（最近 8 周 + 未来 3 个月）：**按周**切。单分区约 90MB，
//     384MB 的 buffer pool 能同时装下最近 4 周 —— 查最新行情基本零磁盘 IO。
//   · 冷区（更早的历史）：**按月**切。一年 12 个，管理成本可控，
//     而且冷数据本来也很少查。
//
// 两层用同一句 `PARTITION BY RANGE COLUMNS(ts)` 表达 —— RANGE 分区**不要求
// 各分区等宽**，所以「先几个月的、再一串 7 天的、最后 pmax」完全合法。
//
// ---------------------------------------------------------------------------
// 分区键选择
// ---------------------------------------------------------------------------
// 用 ts（毫秒时间戳）。MySQL 8 要求「分区列必须出现在表的每一个唯一键里」：
//   kline   PRIMARY KEY (inst_id, bar, ts)                —— 含 ts ✓
//   signals UNIQUE KEY uk_signal (inst_id, bar, ts)       —— 含 ts ✓
// 但 signals 的主键是 (id) 不含 ts，要分区就得改主键，而它才 1 万行，不划算。
// 所以目前只有 kline 分区（见 PartitionedTables）。
//
// 应用层 SQL **一行都不用改** —— MySQL 自己裁剪。
//
// ---------------------------------------------------------------------------
// 两条执行路径（重要）
// ---------------------------------------------------------------------------
// · 首次分区 / 改分区方案（无分区 → 有分区，或改分片粒度）：MySQL 只能
//   ALGORITHM=COPY，会 LOCK=SHARED 禁写几分钟。所以**不在启动路径上自动跑**，
//   走 `okxweb.exe -partition`（缺省跳过已分区表）或 `-repartition`（强制重建）。
// · 之后补新分区：`ALTER TABLE ... ADD PARTITION` 在 MySQL 8 是
//   INPLACE / LOCK=NONE，不阻塞读写，可以在启动时自动补。

import (
	"fmt"
	"strings"
	"time"

	"finally-main/internal/logx"
)

const (
	// PartitionHotWeeks 热区（按周切）覆盖最近多少周
	PartitionHotWeeks = 8
	// PartitionMonthsAhead 预建未来几个自然月（热区按周一直铺到那个月 1 号）。
	// 只铺 1 个月：kline 的保留期约 30 天，再多铺出来的分区长期是空的，
	// 纯属给 information_schema 添负担。真正需要时 EnsurePartitions 会自动补。
	PartitionMonthsAhead = 1
	// PartitionColdMonths 冷区（按月切）最多回溯多少个月；
	// 更早的数据统一落进第一个 p_old 兜底分区，避免分区数失控
	PartitionColdMonths = 18
	// partitionOldName 兜底分区名（覆盖 base 之前的一切数据）
	partitionOldName = "p_old"
)

// PartitionedTables 需要分区的表。
//
// 目前只有 kline：它是唯一「行数会长到千万级」的表。
//
// 为什么 signals 不一起分区：MySQL 8 要求「分区列必须出现在表的每一个唯一键里」。
//   kline    PRIMARY KEY (inst_id, bar, ts)   —— ts 在里面 ✓ 能分区
//   signals  PRIMARY KEY (id) + uk_signal(inst_id, bar, ts)
//            —— 主键 (id) 不含 ts ✗，要分区就得把主键改成 (id, ts)，
//               那是更大的改动，而 signals 才 1 万行，收益不划算。
//  等 signals 真的涨到百万级，再把主键改成 (id, ts) 一并分区。
var PartitionedTables = []string{"kline"}

// ---------------------------------------------------------------------------
// 时间工具
// ---------------------------------------------------------------------------

// monthStart 返回 t 所在自然月的 1 号 00:00:00（本地时区）
func monthStart(t time.Time) time.Time {
	y, m, _ := t.Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, t.Location())
}

// monthStartMs t 所在月的下一个月 1 号毫秒时间戳（即「本月上界」）
func monthStartMs(t time.Time) int64 {
	return monthStart(t).AddDate(0, 1, 0).UnixMilli()
}

// monthTag 2026-10 → "202610"
func monthTag(t time.Time) string {
	y, m, _ := t.Date()
	return fmt.Sprintf("%04d%02d", y, int(m))
}

// dayTag 2026-10-05 → "20261005"
func dayTag(t time.Time) string {
	y, m, d := t.Date()
	return fmt.Sprintf("%04d%02d%02d", y, int(m), d)
}

// ---------------------------------------------------------------------------
// 分区现状探查
// ---------------------------------------------------------------------------

// partRange 一个已存在分区的区间
type partRange struct {
	Name     string
	LessThan int64 // 上界（不含）；pmax 记 1<<62-1
	IsMax    bool  // 是否是 pmax 兜底分区
}

// partitionInfo 一张表的分区现状
type partitionInfo struct {
	Table       string
	Partitioned bool
	Ranges      []partRange // 按 ordinal_position 顺序
	Names       []string    // 只含名字（兼容旧调用）
	// MaxDataLessThan 非 pmax 分区的最大上界。补新分区时用它判「已经铺到哪了」。
	MaxDataLessThan int64
}

// inspectPartition 查 information_schema 看某表的分区情况
func (d *DB) inspectPartition(table string) (*partitionInfo, error) {
	if !safeIdent(table) {
		return nil, fmt.Errorf("非法表名：%s", table)
	}
	rows, err := d.sql.Query(`
		SELECT partition_name, COALESCE(partition_description,'')
		FROM information_schema.partitions
		WHERE table_schema=DATABASE() AND table_name=? AND partition_name IS NOT NULL
		ORDER BY partition_ordinal_position`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	pi := &partitionInfo{Table: table}
	for rows.Next() {
		var name, desc string
		if err := rows.Scan(&name, &desc); err != nil {
			return nil, err
		}
		pr := partRange{Name: name}
		desc = strings.TrimSpace(desc)
		if strings.EqualFold(desc, "MAXVALUE") {
			pr.IsMax = true
			pr.LessThan = 1<<62 - 1
		} else {
			var v int64
			if _, err := fmt.Sscanf(desc, "%d", &v); err == nil {
				pr.LessThan = v
			}
		}
		pi.Ranges = append(pi.Ranges, pr)
		pi.Names = append(pi.Names, name)
		if !pr.IsMax && pr.LessThan > pi.MaxDataLessThan {
			pi.MaxDataLessThan = pr.LessThan
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	pi.Partitioned = len(pi.Names) > 0
	return pi, nil
}

// PartitionDetail 每个分区的行数与磁盘占用（自检 / 面板展示用）。
//
// 这是唯一允许对分区表跑 `COUNT(*)` 的地方 —— 按分区统计会走分区裁剪，
// 每个分区各自扫一遍，代价可控；而且它只在自检/接口里按需调用，
// 不在任何轮询路径上。
func (d *DB) PartitionDetail(table string) ([]map[string]any, error) {
	if !safeIdent(table) {
		return nil, fmt.Errorf("非法表名：%s", table)
	}
	rows, err := d.sql.Query(`
		SELECT partition_name, COALESCE(table_rows,0),
		       COALESCE(data_length,0), COALESCE(index_length,0),
		       COALESCE(partition_description,'')
		FROM information_schema.partitions
		WHERE table_schema=DATABASE() AND table_name=? AND partition_name IS NOT NULL
		ORDER BY partition_ordinal_position`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var name, desc string
		var n, dataLen, idxLen int64
		if err := rows.Scan(&name, &n, &dataLen, &idxLen, &desc); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"name": name, "rows": n,
			"dataMB":  float64(dataLen) / 1048576,
			"indexMB": float64(idxLen) / 1048576,
			"bound":   strings.TrimSpace(desc),
			"isMax":   strings.EqualFold(strings.TrimSpace(desc), "MAXVALUE"),
		})
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 分区方案生成
// ---------------------------------------------------------------------------

// partBound 计划中的一个分区
type partBound struct {
	Name     string
	LessThan int64
	From     time.Time // 区间起点（只用于注释与日志）
	Weekly   bool      // 是否是按周切出来的
}

// planBounds 生成分区方案（上界列表，升序）。
//
// firstData：表里最早的一条数据时间（决定冷区从哪个月开始）
// now：当前时间
//
// 结构：
//
//	p_old                覆盖 base 之前的一切（正常是空的，纯兜底）
//	+ 冷区按月           base 月 1 号 → hotStart，每月一个
//	+ 热区按周           hotStart → 未来 PartitionMonthsAhead 个月，每 7 天一个
//	+ pmax               兜底（调用方补）
func planBounds(firstData, now time.Time) []partBound {
	loc := now.Location()
	coldFloor := monthStart(now).AddDate(0, -PartitionColdMonths, 0)
	dataFrom := monthStart(firstData.In(loc))
	if dataFrom.Before(coldFloor) {
		dataFrom = coldFloor
	}

	// 热区起点：把「今天往前 HotWeeks 周」所在的**自然月 1 号**当作分界。
	// 取整月是为了让热/冷边界落在月 1 号上 —— 否则会出现一个只覆盖
	// 两三天的碎片分区（既没意义，又让 REORGANIZE 的边界推导变麻烦）。
	hotStart := monthStart(now.AddDate(0, 0, -7*PartitionHotWeeks))
	if hotStart.Before(dataFrom) {
		hotStart = dataFrom
	}
	base := dataFrom
	if hotStart.Before(base) {
		base = hotStart
	}

	// 热区要铺到「当前月 + Ahead」的 1 号
	end := monthStart(now).AddDate(0, PartitionMonthsAhead+1, 0)

	var bs []partBound
	// 兜底老分区：上界 = base
	bs = append(bs, partBound{Name: partitionOldName, LessThan: base.UnixMilli()})

	// 冷区：按月（base → hotStart）
	for c := base; c.Before(hotStart); c = c.AddDate(0, 1, 0) {
		next := c.AddDate(0, 1, 0)
		if next.After(hotStart) {
			next = hotStart
		}
		bs = append(bs, partBound{
			Name: "p" + monthTag(c), LessThan: next.UnixMilli(), From: c,
		})
	}
	// 热区：按 7 天一段。锚点是「热区起点所在月的 1 号」，保证边界可复现。
	for c := hotStart; c.Before(end); c = c.AddDate(0, 0, 7) {
		next := c.AddDate(0, 0, 7)
		bs = append(bs, partBound{
			Name: "pw" + dayTag(c), LessThan: next.UnixMilli(), From: c, Weekly: true,
		})
	}
	return bs
}

// buildPartitionClause 把方案渲染成 ALTER TABLE 用的分区子句。
func buildPartitionClause(bs []partBound) string {
	var sb strings.Builder
	sb.WriteString("PARTITION BY RANGE COLUMNS(ts) (\n")
	for i, b := range bs {
		if i > 0 {
			sb.WriteString(",\n")
		}
		// 用 `/* */` 块注释而不是 `--` 行注释：行注释会把行尾的逗号一起吃掉，
		// 生成 "... LESS THAN (123)  -- <2026-10-01," 这种把分隔符注释掉的非法 SQL。
		label := time.UnixMilli(b.LessThan).Format("2006-01-02")
		fmt.Fprintf(&sb, "  PARTITION %s VALUES LESS THAN (%d) /* < %s */",
			b.Name, b.LessThan, label)
	}
	sb.WriteString(",\n  PARTITION pmax VALUES LESS THAN (MAXVALUE)\n)")
	return sb.String()
}

// ---------------------------------------------------------------------------
// 一次性：把表改造成新分区方案（-partition / -repartition，需要停机窗口）
// ---------------------------------------------------------------------------

// MigrateToPartition 把一张表的分区方案同步成 planBounds。
//
// force=false：表已经是分区表就跳过（-partition 的缺省行为）
// force=true ：无论当前什么状态都重建（-repartition，用于把老的纯按月方案
//              升级成「冷区月 + 热区周」）
//
// ⚠ 这是整表重建（ALGORITHM=COPY），kline 788MB 在本机约需数分钟，期间禁写。
// 只应由 `okxweb.exe -partition / -repartition` 触发，且**必须先停掉引擎**。
func (d *DB) MigrateToPartition(table string, force bool) error {
	pi, err := d.inspectPartition(table)
	if err != nil {
		return err
	}
	if pi.Partitioned && !force {
		logx.Logf("INFO", "[PART] %s 已是分区表（%d 个分区），跳过重建（要改方案用 -repartition）",
			table, len(pi.Names))
		return nil
	}

	var minTs int64
	if err := d.sql.QueryRow("SELECT COALESCE(MIN(ts),0) FROM `" + table + "`").Scan(&minTs); err != nil {
		return fmt.Errorf("读 %s 最早时间失败：%w", table, err)
	}
	now := time.Now()
	since := now
	if minTs > 0 {
		since = time.UnixMilli(minTs)
	}

	bs := planBounds(since, now)
	clause := buildPartitionClause(bs)
	weekly, monthly := 0, 0
	for _, b := range bs {
		if b.Weekly {
			weekly++
		} else if b.Name != partitionOldName {
			monthly++
		}
	}
	start := time.Now()
	logx.Logf("INFO", "[PART] 开始重建 %s 的分区方案（数据起点 %s，%d 个月分区 + %d 个周分区，会重建整表）...",
		table, since.Format("2006-01-02"), monthly, weekly)

	if _, err := d.sql.Exec("ALTER TABLE `" + table + "` " + clause); err != nil {
		return fmt.Errorf("分区 %s 失败：%w", table, err)
	}
	logx.Logf("INFO", "[PART] ✔ %s 分区完成，用时 %.1fs（冷区按月 + 热区按周）",
		table, time.Since(start).Seconds())
	return nil
}

// ---------------------------------------------------------------------------
// 增量：补齐未来分区（启动时自动跑，不阻塞）
// ---------------------------------------------------------------------------

// EnsurePartitions 确保每张分区表都铺到「当前月 + PartitionMonthsAhead」。
//
// 只做 ADD PARTITION（INPLACE / LOCK=NONE，不阻塞读写）。
// 表还没分区的话这里不会自作主张去重建（那要几分钟的写锁），
// 只会打一条提示，让运维用 -partition 开关在停机窗口里做。
func (d *DB) EnsurePartitions() {
	now := time.Now()
	for _, t := range PartitionedTables {
		pi, err := d.inspectPartition(t)
		if err != nil {
			logx.Logf("WARN", "[PART] 检查 %s 分区失败：%v", t, err)
			continue
		}
		if !pi.Partitioned {
			logx.Logf("WARN", "[PART] %s 还不是分区表 —— 执行 `okxweb.exe -partition`（建议先停引擎）可一次性改造", t)
			continue
		}
		if err := d.addMissingPartitions(t, pi, now); err != nil {
			logx.Logf("WARN", "[PART] %s 补分区失败：%v", t, err)
		}
	}
}

// addMissingPartitions 把计划里「比现有最大上界还大、且名字没出现过」的分区补上。
//
// 为什么同时要判名字和上界：
//   · 只判上界：老的纯按月方案（p202609 < 2026-10-01）会让计划的周分区
//     pw20260801（< 2026-08-08）被判为「已覆盖」而漏建 —— 虽然它确实不该建，
//     因为方案变了；但反过来如果表已按新方案建好，缺失的周分区必须补上。
//   · 只判名字：容易被「同名不同界」骗过去。
//
// 两个条件同时满足才 ADD，既不会插出非递增的非法分区，也不会漏建。
func (d *DB) addMissingPartitions(table string, pi *partitionInfo, now time.Time) error {
	existing := make(map[string]bool, len(pi.Names))
	for _, n := range pi.Names {
		existing[n] = true
	}

	var minTs int64
	_ = d.sql.QueryRow("SELECT COALESCE(MIN(ts),0) FROM `" + table + "`").Scan(&minTs)
	since := now
	if minTs > 0 {
		since = time.UnixMilli(minTs)
	}

	var parts, added []string
	for _, b := range planBounds(since, now) {
		if existing[b.Name] {
			continue
		}
		// 必须严格大于现有最大数据上界，否则 ADD 出来是非递增分区，MySQL 直接报错
		if b.LessThan <= pi.MaxDataLessThan {
			continue
		}
		parts = append(parts, fmt.Sprintf("PARTITION %s VALUES LESS THAN (%d)", b.Name, b.LessThan))
		added = append(added, b.Name)
	}
	if len(parts) == 0 {
		return nil
	}

	// 先把 pmax 拆掉，再加新分区，最后把 pmax 放回去。
	// （MySQL 不允许在 MAXVALUE 之后再加分区，必须先 DROP 再 ADD）
	sql := "ALTER TABLE `" + table + "` DROP PARTITION pmax, ADD PARTITION (" +
		strings.Join(parts, ", ") + "), ADD PARTITION (PARTITION pmax VALUES LESS THAN (MAXVALUE))"
	if _, err := d.sql.Exec(sql); err != nil {
		return err
	}
	logx.Logf("INFO", "[PART] %s 新增分区：%s", table, strings.Join(added, ", "))
	return nil
}

// PartitionSummary 分区概况（自检/接口展示用）
func (d *DB) PartitionSummary() []map[string]any {
	out := []map[string]any{}
	for _, t := range PartitionedTables {
		pi, err := d.inspectPartition(t)
		if err != nil {
			out = append(out, map[string]any{"table": t, "error": err.Error()})
			continue
		}
		weekly, monthly := 0, 0
		for _, r := range pi.Ranges {
			switch {
			case r.IsMax:
			case strings.HasPrefix(r.Name, "pw"):
				weekly++
			default:
				monthly++
			}
		}
		out = append(out, map[string]any{
			"table":       t,
			"partitioned": pi.Partitioned,
			"count":       len(pi.Names),
			"weekly":      weekly,
			"monthly":     monthly,
			"names":       pi.Names,
		})
	}
	return out
}
