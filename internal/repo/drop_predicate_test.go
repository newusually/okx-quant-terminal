package repo

// drop_predicate_test.go —— 「哪些分区可以被整段 DROP」的判据单测。
//
// ★ 为什么值得专门测 ★
//
// 这条判据有两个调用方：真删（DropKlinePartitionsBefore）与预演
// （service 层的 -maint-daily-dry）。**两边必须给出完全一样的答案** ——
// 用户正是靠预演去核对「10 天红线到底会删哪些分区」，
// 预演多报或少报一个名字，整条核对链就废了。
//
// 而且两个错误方向都很贵：
//   · 误报 pmax 可删 → 真跑会把 MAXVALUE 兜底分区删掉，比最大上界还新的数据无处安放，**写入直接报错**；
//   · 误报 p_old 可删 → 真跑一次 DROP 会连带丢掉里面还没过期的行；
//   · 漏报（该删的没删）→ 数据越攒越多，10 天窗口形同虚设。

import (
	"reflect"
	"testing"
)

// 造一份典型分区快照（升序，与 KlinePartitionRanges 的顺序一致）
func sampleRanges() []PartRange {
	return []PartRange{
		{Name: "p_old", LessThan: 1760000000000, IsOld: true},           // 低位垃圾桶
		{Name: "p202510", LessThan: 1761926400000},                      // 月分区
		{Name: "p202609", LessThan: 1790000000000},                      // 月分区
		{Name: "pd20260910", LessThan: 1790100000000},                   // 日分区
		{Name: "pd20260920", LessThan: 1790900000000},                   // 日分区
		{Name: "pd20260925", LessThan: 1791300000000},                   // 日分区（边界）
		{Name: "pmax", LessThan: 1<<62 - 1, IsMax: true},                // MAXVALUE 兜底
	}
}

// A. p_old 与 pmax 永远不能被返回
//
// cutoff 故意给到极大，让「只看上界」的写法必然把它们带进来。
func TestDroppablePartitions_NeverReturnsOldOrMax(t *testing.T) {
	got := DroppablePartitions(sampleRanges(), 1<<62-2) // 比 pmax 小、比谁都大

	for _, bad := range []string{"p_old", "pmax"} {
		for _, n := range got {
			if n == bad {
				t.Fatalf("%s 被误判为可删（这会毁掉分区表）：%v", bad, got)
			}
		}
	}
	// 除这两个以外的都应该在里面
	if len(got) != 5 {
		t.Fatalf("应返回 5 个可删分区，实际 %d 个：%v", len(got), got)
	}
}

// B. 只有「上界 ≤ cutoff」才整段删；跨界的那个必须放过
//
// cutoff 取 pd20260920 与 pd20260925 的上界之间：
// pd20260925 里有一部分行还在窗口内，只能交给 PurgeKlineBefore 逐行删。
func TestDroppablePartitions_RespectsCutoff(t *testing.T) {
	cut := int64(1791200000000) // 落在 pd20260920 之后、pd20260925 之前
	got := DroppablePartitions(sampleRanges(), cut)

	want := []string{"p202510", "p202609", "pd20260910", "pd20260920"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("可删分区不对\n got=%v\nwant=%v", got, want)
	}
	for _, n := range got {
		if n == "pd20260925" {
			t.Fatalf("跨界分区 pd20260925 被整段删了 —— 里面还有窗口内的行")
		}
	}
}

// C. 边界必须用 ≤ 而不是 <
//
// 上界是「不含」，所以上界恰好等于 cutoff 的分区里每一行都 < cutoff，可以整段删。
// 写成 < 会永远慢一个分区，日积月累就漏删。
func TestDroppablePartitions_LessThanIsInclusive(t *testing.T) {
	exact := int64(1790100000000) // 恰好等于 pd20260910 的上界
	got := DroppablePartitions(sampleRanges(), exact)

	found := false
	for _, n := range got {
		if n == "pd20260910" {
			found = true
		}
	}
	if !found {
		t.Fatalf("上界恰好等于 cutoff 的分区应可删（判据要用 <=）：%v", got)
	}
}

// D. cutoff 非法（<=0）→ 一个都不返回
//
// 这是防「配置写 0 或负数时把整张表删光」的最后一道闸。
func TestDroppablePartitions_ZeroCutoffDropsNothing(t *testing.T) {
	for _, cut := range []int64{0, -1, -1790000000000} {
		if got := DroppablePartitions(sampleRanges(), cut); len(got) != 0 {
			t.Fatalf("cutoff=%d 时应一个都不删，实际 %v", cut, got)
		}
	}
}

// E. 空快照不 panic
func TestDroppablePartitions_EmptyRanges(t *testing.T) {
	if got := DroppablePartitions(nil, 1790000000000); len(got) != 0 {
		t.Fatalf("空分区快照应返回空，实际 %v", got)
	}
}

// F. 只有 p_old + pmax 的极简库 → 一个都不删
//
// 全新建库时就是这个样子：p_old 是建表种子，pmax 是兜底，
// 数据还没攒出任何月/日分区。此时干净返回空切片，不能把 p_old 报成可删。
func TestDroppablePartitions_OnlySeedPartitions(t *testing.T) {
	rs := []PartRange{
		{Name: "p_old", LessThan: 1760000000000, IsOld: true},
		{Name: "pmax", LessThan: 1<<62 - 1, IsMax: true},
	}
	if got := DroppablePartitions(rs, 1<<62-2); len(got) != 0 {
		t.Fatalf("只有兜底分区时不该返回任何可删项，实际 %v", got)
	}
}
