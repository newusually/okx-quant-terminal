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
// 为什么「按月 + 按天」两层分区（关键设计，2026-10-01 二期由按周改为按天）
// ---------------------------------------------------------------------------
// 纯按月的问题：整月数据远超 buffer pool，一个月都装不下，
// 查「最近一小时」照样要读盘。
//
// 纯按天的问题：历史拉长之后分区数暴涨（一年 365 个），管理成本高。
// 对当前口径（K 线只留 10 天）来说一年根本不存在，但 p_old 之前的历史
// 还是要按月聚拢，否则 DROP 的单位太碎、information_schema 也吃不消。
//
// 所以分两层：
//   · 热区（最近 21 天 + 未来 2 天）：**按天**切。
//     单天约 100MB（四个周期合计），最近几天的热点数据能常在 buffer pool 里；
//     更重要的是**每日清理能整段 DROP**（见下面「为什么必须按天」）。
//   · 冷区（更早的历史）：**按月**切。正常应该是空的（10 天保留窗口会把它删光），
//     只在「清理任务停了很久」或「保留窗口被临时调大」时才有数据。
//
// ---------------------------------------------------------------------------
// 为什么必须按天（这是二期改动力度最大的一处）
// ---------------------------------------------------------------------------
// 每日清理的 cutoff = now − 10 天，落点在一周中的任意一天。
// 分区粒度必须**细于**删除粒度，否则每次清理都会有一个「跨在 cutoff 上的分区」
// 既不能 DROP、又必须逐行 DELETE，而 kline 上**没有独立的 ts 索引**
// （主键是 inst_id,bar,ts，见 indexes.go 的说明），`WHERE ts < ?` 只能全索引扫。
// 按周切时那个边界分区有 7 天的数据、约 90MB，每批 LIMIT 5000 都要重扫一遍 ——
// 几十批下来就是几十分钟。按天切之后，边界分区只有 1 天数据，
// 而凌晨 00:0x 跑任务时 cutoff 恰好落在某个日边界附近，需要逐行删的只有几分钟的量。
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
	// PartitionHotDays 热区（按天切）覆盖最近多少天。
	//
	// ★ 2026-10-01 二期：从「按周」改成「按天」★
	//
	// 起因：K 线保留窗口从 365 天砍到 **10 天**（用户口径「只能查询保存最近 10 天
	// 数据，多出来就删除」+「自动每天凌晨删除数据一次」），而每日清理靠
	// `DROP PARTITION`。分区粒度必须**细于**删除粒度，否则每天都在跨分区边界：
	//
	//	按周切 + 每天删 → cut 落在某个 7 天分区中间，那个分区永远 DROP 不掉，
	//	                     只能逐行 DELETE 扫一个 90MB 分区（每批 LIMIT 5000 都要重扫一遍）
	//	按天切 + 每天删 → 整天的分区直接 DROP（毫秒级），残余只有边界那一小段
	//
	// 热区天数取 21：够覆盖 10 天保留窗口 + 两周缓冲，
	// 万一某天清理任务没跑（服务停了、机器重启），数据仍落在可 DROP 的日分区里，
	// 不会掉进永不删除的 p_old。
	PartitionHotDays = 21

	// PartitionDaysAhead 预建未来几天。
	// 只铺 2 天：跨时区/时钟漂移时下一根 K 线不会落到 pmax，
	// 再多铺出来的分区长期是空的，纯属给 information_schema 添负担。
	PartitionDaysAhead = 2

	// PartitionColdMonths 冷区（按月切）最多回溯多少个月；
	// 更早的数据统一落进第一个 p_old 兜底分区，避免分区数失控
	PartitionColdMonths = 18
	// partitionOldName 兜底分区名（覆盖 base 之前的一切数据）
	partitionOldName = "p_old"
	// partitionMaxName 上界兜底分区名（VALUES LESS THAN MAXVALUE）。
	// 必须是最后一个分区，且**永不 DROP** —— 删了之后比最大上界还新的数据无处安放。
	partitionMaxName = "pmax"
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

// dayStart 返回 t 所在自然日的 00:00:00（本地时区）。
// 热区日分区的边界锚在这里 —— 与「每天凌晨清理」的时间口径对齐。
func dayStart(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
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
	Daily    bool      // 是否是热区按天切出来的
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
//	+ 热区按天           hotStart → 未来 PartitionDaysAhead 天，每 1 天一个
//	+ pmax               兜底（调用方补）
//
// ★ 2026-10-01 二期：热区从「按周」改成「按天」★
//
//	热区起点 = 「今天往前 PartitionHotDays 天」所在自然日的 00:00（本地时区）。
//	日边界刻意对齐本地零点，与每日清理任务的运行时间口径一致
//	—— 凌晨 00:0x 跑清理时，cutoff 恰好落在某个日边界附近，
//	所以「整段过期的日分区」能全部 DROP，只剩边界那一小段需要逐行删。
func planBounds(firstData, now time.Time) []partBound {
	loc := now.Location()
	coldFloor := monthStart(now).AddDate(0, -PartitionColdMonths, 0)
	dataFrom := monthStart(firstData.In(loc))
	if dataFrom.Before(coldFloor) {
		dataFrom = coldFloor
	}

	// 热区起点：今天往前 HotDays 天的自然日 00:00。
	hotStart := dayStart(now.In(loc).AddDate(0, 0, -PartitionHotDays))
	if hotStart.Before(dataFrom) {
		hotStart = dataFrom
	}
	base := dataFrom
	if hotStart.Before(base) {
		base = hotStart
	}

	// 热区要铺到「今天 + Ahead」那天的 00:00（不含），再往后由 pmax 接住
	end := dayStart(now.In(loc)).AddDate(0, 0, PartitionDaysAhead+1)

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
	// 热区：按 1 天一段。锚点是「今天往前 HotDays 天」的零点，保证边界可复现。
	for c := hotStart; c.Before(end); c = c.AddDate(0, 0, 1) {
		next := c.AddDate(0, 0, 1)
		bs = append(bs, partBound{
			Name: "pd" + dayTag(c), LessThan: next.UnixMilli(), From: c, Daily: true,
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
	daily, monthly := 0, 0
	for _, b := range bs {
		if b.Daily {
			daily++
		} else if b.Name != partitionOldName {
			monthly++
		}
	}
	start := time.Now()
	logx.Logf("INFO", "[PART] 开始重建 %s 的分区方案（数据起点 %s，%d 个月分区 + %d 个日分区，会重建整表）...",
		table, since.Format("2006-01-02"), monthly, daily)

	if _, err := d.sql.Exec("ALTER TABLE `" + table + "` " + clause); err != nil {
		return fmt.Errorf("分区 %s 失败：%w", table, err)
	}
	logx.Logf("INFO", "[PART] ✔ %s 分区完成，用时 %.1fs（冷区按月 + 热区按天）",
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
//   · 只判上界：老的纯按月方案（p202609 < 2026-10-01）会让计划的日分区
//     pd20260928（< 2026-09-29）被判为「已覆盖」而漏建 —— 虽然它确实不该建，
//     因为方案变了；但反过来如果表已按新方案建好，缺失的日分区必须补上。
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

	var parts []partBound
	for _, b := range planBounds(since, now) {
		if existing[b.Name] {
			continue
		}
		// 必须严格大于现有最大数据上界，否则 ADD 出来是非递增分区，MySQL 直接报错
		if b.LessThan <= pi.MaxDataLessThan {
			continue
		}
		parts = append(parts, b)
	}
	if len(parts) == 0 {
		return nil
	}

	ddl := buildAppendPartitionDDL(table, parts, existing[partitionMaxName])
	if _, err := d.sql.Exec(ddl); err != nil {
		// ★ 把完整 DDL 一起打出来 ★ —— 只在日志里写「补分区失败：Error 1064」，
		// 排查时根本看不出是哪条语句坏在哪，这个坑已经吃过一次。
		logx.Logf("WARN", "[PART] %s 补分区失败（%d 个）：%v\n  DDL: %s",
			table, len(parts), err, ddl)
		return err
	}
	names := make([]string, 0, len(parts))
	for _, b := range parts {
		names = append(names, b.Name)
	}
	logx.Logf("INFO", "[PART] %s 新增分区：%s", table, strings.Join(names, ", "))
	return nil
}

// buildAppendPartitionDDL 生成「把新分区插到 pmax 之前」的 DDL。
//
// ★★ 这里必须用 REORGANIZE，不能用 DROP pmax + ADD ★★
//
// 原来的写法是一条语句里同时 DROP pmax、ADD 新分区、再把 pmax ADD 回去：
//
//	ALTER TABLE t DROP PARTITION pmax, ADD PARTITION (…), ADD PARTITION (PARTITION pmax …)
//
// MySQL **不允许在同一条 ALTER 里既 DROP 又 ADD 分区**（也确实不允许 ADD 出
// 低于现有最大上界的非递增分区），实测恒定报 1064。也就是说这段代码
// **从写下来那天起就没成功过**：新分区永远补不上，数据一路堆进 pmax ——
// 而 pmax 恰恰是「永不 DROP」的兜底分区，于是容量只增不减。
//
// 正确形态是 REORGANIZE：把 pmax 覆盖的那一段区间原子地拆成
// 「若干新分区 + 新的 pmax」。一条语句、原子完成，中途不存在
// 「没有兜底分区可写」的窗口。pmax 正常情况下是空的（热区已经铺到
// 今天 + PartitionDaysAhead），所以这次 reorganize 不搬任何数据，代价≈0。
//
// 表里没有 pmax 时（更老的方案、或压根还没分区）退回纯 ADD ——
// 那是唯一合法的形态：没有 MAXVALUE 兜底分区时，ADD 是允许的。
//
// 抽成纯函数是为了能单测：这条 SQL 坏过一次，不能再靠「跑起来试试」。
func buildAppendPartitionDDL(table string, parts []partBound, hasMax bool) string {
	var sb strings.Builder
	sb.WriteString("ALTER TABLE `" + table + "` ")
	if hasMax {
		sb.WriteString("REORGANIZE PARTITION " + partitionMaxName + " INTO (")
	} else {
		sb.WriteString("ADD PARTITION (")
	}
	for i, b := range parts {
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "PARTITION %s VALUES LESS THAN (%d)", b.Name, b.LessThan)
	}
	if hasMax {
		sb.WriteString(", PARTITION " + partitionMaxName + " VALUES LESS THAN (MAXVALUE))")
	} else {
		sb.WriteString(")")
	}
	return sb.String()
}

// PartRange 一个分区区间的**只读快照**（导出）。
//
// 内部用的是小写的 partRange；这里单独导出一份是给 service 层的
// 「每日清理 dry-run 预告」用的 —— service 不能（也不该）依赖 repo 的内部结构。
type PartRange struct {
	Name     string // 分区名，形如 p_old / p202609 / pd20261001 / pmax
	LessThan int64  // 上界（不含）；pmax 记 1<<62-1
	IsMax    bool   // pmax 兜底分区
	IsOld    bool   // p_old 垃圾桶分区（永不 DROP）
}

// DroppablePartitions 从分区快照里挑出「上界 ≤ cutMs、且可以整段丢弃」的分区名。
//
// ★★ 预演与真跑必须共用这一个判据 ★★
//
// 原来两处各写了一遍：真跑（DropKlinePartitionsBefore）正确地跳过了
// p_old / pmax，而每日清理的 dry-run 预告只判了「上界 ≤ cutoff」，
// 于是预报出「将整段 DROP 1 个日分区 [p_old]」—— **预演在说谎**。
// 用户恰恰是靠这条预演核对「10 天红线到底会删哪些分区」，报错一个名字整条核对链就废了。
//
// 两条永不返回：
//   - pmax：MAXVALUE 兜底分区，删了之后比最大上界还新的数据无处安放，写入直接报错；
//   - p_old：低位垃圾桶，装的是低于第一个分区下界的碎片，
//     交给 PurgeKlineBefore 分批逐行删（一次 DROP 会把里面还没过期的行一起带走）。
func DroppablePartitions(ranges []PartRange, cutMs int64) []string {
	if cutMs <= 0 {
		return nil
	}
	out := make([]string, 0, len(ranges))
	for _, r := range ranges {
		if r.IsMax || r.IsOld {
			continue
		}
		if r.LessThan > 0 && r.LessThan <= cutMs {
			out = append(out, r.Name)
		}
	}
	return out
}

// toPartRanges 内部 partRange → 导出的 PartRange（补上 IsOld 标记）
func toPartRanges(rs []partRange) []PartRange {
	out := make([]PartRange, 0, len(rs))
	for _, r := range rs {
		out = append(out, PartRange{
			Name: r.Name, LessThan: r.LessThan, IsMax: r.IsMax,
			IsOld: r.Name == partitionOldName,
		})
	}
	return out
}

// KlinePartitionRanges 取 kline 现有分区区间（升序，按 ordinal_position）。
//
// 只在自检 / dry-run 预告 / 面板里调用，不在任何轮询路径上。
func (d *DB) KlinePartitionRanges() ([]PartRange, error) {
	pi, err := d.inspectPartition("kline")
	if err != nil {
		return nil, err
	}
	return toPartRanges(pi.Ranges), nil
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
		daily, monthly, legacyWeekly, old := 0, 0, 0, 0
		for _, r := range pi.Ranges {
			switch {
			case r.IsMax:
			case r.Name == partitionOldName:
				old++
			case strings.HasPrefix(r.Name, "pd"):
				daily++
			case strings.HasPrefix(r.Name, "pw"):
				// 老方案（按周）留下的分区：跑到 -repartition 之后就没了。
				// 单独计数而不是并进「月分区」，否则运维会以为新方案没生效。
				legacyWeekly++
			default:
				monthly++
			}
		}
		out = append(out, map[string]any{
			"table":       t,
			"partitioned": pi.Partitioned,
			"count":       len(pi.Names),
			"daily":       daily,
			"monthly":     monthly,
			"legacyWeeks": legacyWeekly,
			"old":         old,
			"names":       pi.Names,
		})
	}
	return out
}
