package repo

// partition_append_test.go —— 「补未来分区」那条 DDL 的纯函数验证（不连数据库）
//
// ★ 2026-10-02 四期为什么必须测 ★
//
// addMissingPartitions 原本生成的是一条**永远跑不通**的语句：
//
//	ALTER TABLE t DROP PARTITION pmax, ADD PARTITION (…), ADD PARTITION (PARTITION pmax …)
//
// MySQL 不允许在同一条 ALTER 里既 DROP 又 ADD 分区，恒定报 1064。
// 而这段代码的失败是**静默的**：只在日志里留一行 WARN「补分区失败」，
// 对外表现是「服务一切正常、K 线照常入库」，真到容量出问题才会被发现 ——
// 实测它从写下来那天起就没成功过，数据一路堆进 pmax，而 pmax 是永不 DROP 的。
//
// 所以这条 SQL 不能再靠「跑起来试试」。这里把它钉成字符串断言：
// 必须 REORGANIZE、必须带回新的 pmax、**绝不允许出现 DROP PARTITION**。

import (
	"strings"
	"testing"
)

func partsFixture() []partBound {
	return []partBound{
		{Name: "pd20261004", LessThan: 1790000000000},
		{Name: "pd20261005", LessThan: 1790086400000},
	}
}

// TestAppendDDLUsesReorganizeWhenMaxExists
// 有 pmax 时唯一合法形态 = REORGANIZE PARTITION pmax INTO (新分区…, 新 pmax)。
func TestAppendDDLUsesReorganizeWhenMaxExists(t *testing.T) {
	ddl := buildAppendPartitionDDL("kline", partsFixture(), true)

	if !strings.HasPrefix(ddl, "ALTER TABLE `kline` REORGANIZE PARTITION pmax INTO (") {
		t.Fatalf("有 pmax 时必须走 REORGANIZE，实际：%s", ddl)
	}
	// 新的 pmax 必须被放回去，否则比最大上界还新的数据没有落点，写入直接报错
	if !strings.HasSuffix(ddl, ", PARTITION pmax VALUES LESS THAN (MAXVALUE))") {
		t.Fatalf("REORGANIZE 必须把 pmax 放回末尾，实际：%s", ddl)
	}
	// ★ 回归断言：这条是历史上从来没成功过的写法，不能再出现
	if strings.Contains(ddl, "DROP PARTITION") {
		t.Fatalf("不许再出现 DROP PARTITION（MySQL 禁止同语句 DROP+ADD）：%s", ddl)
	}
	// 新分区必须按升序原样出现
	i1 := strings.Index(ddl, "PARTITION pd20261004 VALUES LESS THAN (1790000000000)")
	i2 := strings.Index(ddl, "PARTITION pd20261005 VALUES LESS THAN (1790086400000)")
	if i1 < 0 || i2 < 0 {
		t.Fatalf("新分区子句缺失：%s", ddl)
	}
	if i1 > i2 {
		t.Fatalf("新分区顺序被打乱（MySQL 要求上界严格递增）：%s", ddl)
	}
}

// TestAppendDDLFallsBackToAddWithoutMax
// 表里没有 pmax（老方案 / 未分区）时退回纯 ADD —— 那是唯一合法的形态。
func TestAppendDDLFallsBackToAddWithoutMax(t *testing.T) {
	ddl := buildAppendPartitionDDL("kline", partsFixture(), false)

	if !strings.HasPrefix(ddl, "ALTER TABLE `kline` ADD PARTITION (") {
		t.Fatalf("无 pmax 时应退回 ADD PARTITION，实际：%s", ddl)
	}
	if strings.Contains(ddl, "REORGANIZE") {
		t.Fatalf("无 pmax 时不能 REORGANIZE：%s", ddl)
	}
	if strings.Contains(ddl, partitionMaxName) {
		t.Fatalf("无 pmax 时不应凭空造出 pmax（会破坏原有分区序）：%s", ddl)
	}
	if strings.Contains(ddl, "DROP PARTITION") {
		t.Fatalf("不许出现 DROP PARTITION：%s", ddl)
	}
}

// TestAppendDDLSinglePartition 只有一个新分区时不能多出逗号。
func TestAppendDDLSinglePartition(t *testing.T) {
	one := []partBound{{Name: "pd20261004", LessThan: 1790000000000}}

	withMax := buildAppendPartitionDDL("kline", one, true)
	want := "ALTER TABLE `kline` REORGANIZE PARTITION pmax INTO (" +
		"PARTITION pd20261004 VALUES LESS THAN (1790000000000), " +
		"PARTITION pmax VALUES LESS THAN (MAXVALUE))"
	if withMax != want {
		t.Fatalf("单分区 REORGANIZE 形态不符：\n want %s\n got  %s", want, withMax)
	}

	noMax := buildAppendPartitionDDL("kline", one, false)
	want2 := "ALTER TABLE `kline` ADD PARTITION (PARTITION pd20261004 VALUES LESS THAN (1790000000000))"
	if noMax != want2 {
		t.Fatalf("单分区 ADD 形态不符：\n want %s\n got  %s", want2, noMax)
	}
}
