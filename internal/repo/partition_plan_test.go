package repo

// partition_plan_test.go —— 分区方案的纯函数验证（不连数据库）
//
// ★ 2026-10-01 二期为什么值得测 ★
//
// 分区粒度是「每日凌晨删 K 线」能不能跑得动的**唯一前提**：
//
//	kline 上没有任何以 ts 为前导的索引（主键是 inst_id,bar,ts，见 indexes.go），
//	所以 `DELETE FROM kline WHERE ts < ? LIMIT 5000` 只能全索引扫。
//	跨在 cutoff 上的那个分区有多大，决定这条 DELETE 要重扫多少遍。
//
// 按周切时，边界分区有 7 天数据（约 90MB），凌晨任务要重扫几十遍；
// 按天切之后，边界分区只有 1 天数据，而且任务在 00:0x 跑，
// cutoff 恰好落在日边界上，需要逐行删的只剩几分钟的量。
//
// 这个文件把「日边界对齐本地零点」「整段可 DROP 的天数 ≥ 保留天数」
// 两件事钉死，免得以后有人把粒度改回去。

import (
	"testing"
	"time"
)

// planAt 用固定时刻生成方案，方便断言
func planAt(now, firstData time.Time) []partBound {
	return planBounds(firstData, now)
}

// TestPlanBoundsStrictlyIncreasing MySQL 的 RANGE 分区要求上界严格递增，
// 否则 ALTER TABLE 直接报错。这条最便宜，但一旦违反整个改建就跑不起来。
func TestPlanBoundsStrictlyIncreasing(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 5, 0, 0, time.Local)
	first := now.AddDate(0, 0, -400)
	bs := planAt(now, first)
	if len(bs) < 30 {
		t.Fatalf("方案太短（%d 个分区），冷区+热区应该都有", len(bs))
	}
	seen := map[string]bool{}
	prev := int64(0)
	for i, b := range bs {
		if b.LessThan <= prev {
			t.Fatalf("第 %d 个分区 %s 的上界 %d 未严格大于前一个 %d",
				i, b.Name, b.LessThan, prev)
		}
		if seen[b.Name] {
			t.Fatalf("分区名重复：%s", b.Name)
		}
		seen[b.Name] = true
		prev = b.LessThan
	}
	if bs[0].Name != partitionOldName {
		t.Fatalf("第一个分区必须是 %s（垃圾桶），实际 %s", partitionOldName, bs[0].Name)
	}
}

// TestPlanBoundsDailyBoundariesAlignToLocalMidnight 日分区边界必须对齐本地零点。
//
// 对齐的意义：每日清理在 00:0x 跑，cutoff 是「此刻 − 10 天」，
// 只有边界对齐零点，cutoff 才落在某个日分区边界附近，
// 从而让「整数天的分区」被整段 DROP。
// 如果锚在「现在往前 21 天」这种带时分秒的时刻上，每天多出来的那几分钟
// 会让边界分区逐日漂移，DROP 掉的天数会一天比一天少。
func TestPlanBoundsDailyBoundariesAlignToLocalMidnight(t *testing.T) {
	// 故意用一个「带时分秒」的 now，看边界有没有被抹平
	now := time.Date(2026, 10, 1, 13, 47, 23, 0, time.Local)
	bs := planAt(now, now.AddDate(0, 0, -90))

	daily := 0
	for _, b := range bs {
		if !b.Daily {
			continue
		}
		daily++
		lt := time.UnixMilli(b.LessThan).In(time.Local)
		if lt.Hour() != 0 || lt.Minute() != 0 || lt.Second() != 0 || lt.Nanosecond() != 0 {
			t.Fatalf("日分区 %s 的上界 %s 没有对齐本地零点", b.Name, lt.Format(time.RFC3339))
		}
		if !b.From.Equal(time.Date(b.From.Year(), b.From.Month(), b.From.Day(), 0, 0, 0, 0, time.Local)) {
			t.Fatalf("日分区 %s 的起点 %s 没有对齐本地零点", b.Name, b.From)
		}
	}
	if daily < PartitionHotDays+PartitionDaysAhead {
		t.Fatalf("日分区只有 %d 个，应至少覆盖最近 %d 天 + 未来 %d 天",
			daily, PartitionHotDays, PartitionDaysAhead)
	}
}

// TestPlanBoundsDropsWholeRetentionWindow 这是本文件的核心断言：
//
//	**每日清理时，能被整段 DROP 的天数，必须 ≥ K 线保留天数。**
//
// 只有满足它，「只留 10 天」才不会退化成「每天逐行删 10 天里最老的 1 天」。
// 口径：cutoff = now − retainDays，统计所有 LessThan ≤ cutoff 的日分区，
//       这些分区的最早上界（最后被 DROP 的那一天）到 cutoff 之间不能超过 1 天。
func TestPlanBoundsDropsWholeRetentionWindow(t *testing.T) {
	const retainDays = 10
	t0 := time.Date(2026, 10, 1, 0, 5, 0, 0, time.Local) // 凌晨 00:05 跑任务

	for _, now := range []time.Time{
		t0,
		t0.Add(24 * time.Hour),  // 第二天凌晨
		t0.Add(3 * 24 * time.Hour),
		time.Date(2026, 11, 1, 0, 7, 0, 0, time.Local), // 跨月
		time.Date(2027, 3, 15, 0, 1, 0, 0, time.Local), // 跨年 + 夏令时无关（本地时区）
	} {
		cut := now.AddDate(0, 0, -retainDays)
		bs := planAt(now, now.AddDate(0, 0, -400))

		lastDropped := int64(0)
		droppedDays := 0
		for _, b := range bs {
			if b.Name == partitionOldName {
				continue // p_old 永不 DROP
			}
			if b.LessThan > 0 && b.LessThan <= cut.UnixMilli() {
				lastDropped = b.LessThan
				if b.Daily {
					droppedDays++
				}
			}
		}
		if droppedDays < retainDays {
			t.Fatalf("now=%s：只能整段 DROP %d 个日分区，少于保留窗口 %d 天 —— 剩下要靠逐行删，凌晨任务会超时",
				now.Format("2006-01-02 15:04"), droppedDays, retainDays)
		}
		// 残余（跨在 cutoff 上的那一段）必须 ≤ 1 天
		residual := cut.UnixMilli() - lastDropped
		if residual > int64(24*time.Hour) {
			t.Fatalf("now=%s：边界残余 %v 超过 1 天，逐行 DELETE 会重扫一个过大的分区",
				now.Format("2006-01-02 15:04"), time.Duration(residual))
		}
	}
}

// TestPlanBoundsAlwaysLeavesOldAndMax p_old 与 pmax 是兜底，永远不能被方案「吃掉」：
// p_old 必须还在方案里（调用方会补 pmax）。少一个，早期数据就会无处可落。
func TestPlanBoundsAlwaysLeavesOldAndMax(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 5, 0, 0, time.Local)

	// 极端一：数据起点就在今天（全新库）
	bs := planAt(now, now)
	if bs[0].Name != partitionOldName {
		t.Fatalf("全新库的方案第一个也必须是 %s", partitionOldName)
	}
	if bs[0].LessThan != now.UnixMilli() &&
		bs[0].LessThan != time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local).UnixMilli() {
		t.Logf("提示：全新库时 p_old 上界 = %s", time.UnixMilli(bs[0].LessThan).Format("2006-01-02 15:04"))
	}

	// 极端二：数据起点远超冷区下限（老库），冷区会被夹到 18 个月
	bs = planAt(now, now.AddDate(-5, 0, 0))
	if bs[0].Name != partitionOldName {
		t.Fatalf("老库的方案第一个也必须是 %s", partitionOldName)
	}
	months := 0
	for _, b := range bs {
		if b.Name != partitionOldName && !b.Daily {
			months++
		}
	}
	if months > PartitionColdMonths+1 {
		t.Fatalf("冷区月分区 %d 个，超过上限 %d —— 分区数会失控", months, PartitionColdMonths)
	}
}
