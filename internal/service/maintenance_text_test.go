package service

// maintenance_text_test.go —— 维护报告渲染的单测。
//
// 为什么值得测：这段代码「一次都跑对了也看不出来，跑错了也没人崩」。
// 缺 daily 分支的后果是**手工预演时什么都看不到** —— 标题掉回「月度维护」、
// 分区清单被整段吞掉。用户正是靠这条命令核对「10 天红线到底删了哪些分区」，
// 打不出来就等于这个功能不存在。
//
// 另一个安静的坑：预演时 FreeGBAfter 没测，直接按两段打会变成
// 「20.5 GB → 0.0 GB」，看着像把 C 盘写爆了。

import (
	"strings"
	"testing"
)

// A. 每日清理的标题必须是「每日 K 线清理」，不能掉回月度；且要列出分区
func TestMaintenanceText_DailyTitle(t *testing.T) {
	// Summary 用 dailySummary 生成，与 RunDailyMaintenance 里的做法**同源** ——
	// 不要手写一个生产里根本不会出现的字符串，否则测的是假管道。
	rep := &MaintenanceReport{
		Kind: "daily", DryRun: true, FreeGBBefore: 20.5,
		KlineDropped: []string{"pd20260910", "pd20260911"},
	}
	rep.Summary = dailySummary(rep, 10, 1790000000000)
	txt := MaintenanceText(rep)

	if !strings.Contains(txt, "每日 K 线清理") {
		t.Fatalf("标题里应出现「每日 K 线清理」，实际：\n%s", txt)
	}
	if strings.Contains(txt, "月度维护") {
		t.Fatalf("每日清理不该出现「月度维护」字样，实际：\n%s", txt)
	}
	// 分区清单必须打出来 —— 这是预演的唯一价值
	if !strings.Contains(txt, "pd20260910") {
		t.Fatalf("预演必须列出会 DROP 的分区名，实际：\n%s", txt)
	}
}

// B. 预演不能打出「→ 0.0 GB」这种吓人的伪变化
func TestMaintenanceText_DailyDryRunNoFakeDiskChange(t *testing.T) {
	rep := &MaintenanceReport{
		Kind: "daily", DryRun: true, FreeGBBefore: 20.5, FreeGBAfter: 0,
		Summary: "每日 K 线清理：没有超期数据",
	}
	txt := MaintenanceText(rep)

	if strings.Contains(txt, "→ 0.0 GB") {
		t.Fatalf("预演没测到清理后的可用空间，不该打出 「→ 0.0 GB」：\n%s", txt)
	}
	if !strings.Contains(txt, "20.5 GB") {
		t.Fatalf("清理前的可用空间要照常打出来：\n%s", txt)
	}
}

// C. 每日清理不该越权汇报日志/记录表 —— 那不是每日任务干的活
func TestMaintenanceText_DailyDoesNotReportMonthlySteps(t *testing.T) {
	rep := &MaintenanceReport{Kind: "daily", DryRun: true, FreeGBBefore: 20.5}
	txt := MaintenanceText(rep)

	for _, unwanted := range []string{"日志清理", "记录表清理", "磁盘守卫", "回收站"} {
		if strings.Contains(txt, unwanted) {
			t.Fatalf("每日清理的报告里不该出现「%s」：\n%s", unwanted, txt)
		}
	}
}

// D. 月度/年度的标题不能被改坏（回归保护）
func TestMaintenanceText_MonthlyAndYearlyTitles(t *testing.T) {
	mon := MaintenanceText(&MaintenanceReport{Kind: "monthly"})
	if !strings.Contains(mon, "月度维护") {
		t.Fatalf("月度报告标题被改坏了：\n%s", mon)
	}
	yr := MaintenanceText(&MaintenanceReport{Kind: "yearly", DryRun: true})
	if !strings.Contains(yr, "年度清理") {
		t.Fatalf("年度报告标题被改坏了：\n%s", yr)
	}
}

// E. 真跑完的每日清理要打出「→ 清理后」，两段都要有
func TestMaintenanceText_DailyRealRunShowsBothDiskValues(t *testing.T) {
	rep := &MaintenanceReport{
		Kind: "daily", DryRun: false,
		FreeGBBefore: 19.56, FreeGBAfter: 20.63,
		KlineDropped: []string{"p202510", "pd20260910"}, KlineRows: 9959984,
		Summary: "每日 K 线清理：红线 10 天；整段 DROP 2 个日分区，约 9959984 行",
	}
	txt := MaintenanceText(rep)

	if !strings.Contains(txt, "19.6 GB → 20.6 GB") {
		t.Fatalf("真跑完应显示清理前后的可用空间变化：\n%s", txt)
	}
	// 真跑没有「预演」提示
	if strings.Contains(txt, "一个都没删") {
		t.Fatalf("真跑的报告不该出现预演提示：\n%s", txt)
	}
}

// F. nil 报告不能 panic（命令行有 err 分支会带着 nil 调进来）
func TestMaintenanceText_NilIsSafe(t *testing.T) {
	if got := MaintenanceText(nil); got == "" {
		t.Fatalf("nil 报告应返回占位串而不是空串")
	}
}

// G. dailySummary 在预演时不报行数（预演不扫行，报「约 0 行」是假的）
func TestDailySummary_DryRunOmitsRowCount(t *testing.T) {
	rep := &MaintenanceReport{
		Kind: "daily", DryRun: true, FreeGBBefore: 20.5,
		KlineDropped: []string{"pd20260910", "pd20260911"},
	}
	got := dailySummary(rep, 10, 1790000000000)

	if strings.Contains(got, "0 行") {
		t.Fatalf("预演不该报行数（实际会报成假的 0 行）：%s", got)
	}
	if !strings.Contains(got, "pd20260910") {
		t.Fatalf("预演摘要必须列出分区名：%s", got)
	}
	if strings.Contains(got, "→") {
		t.Fatalf("预演摘要不该出现磁盘变化箭头：%s", got)
	}

	// 真跑则要带行数与磁盘变化
	rep.DryRun = false
	rep.KlineRows = 9959984
	rep.FreeGBAfter = 20.63
	got = dailySummary(rep, 10, 1790000000000)
	if !strings.Contains(got, "9959984") || !strings.Contains(got, "→") {
		t.Fatalf("真跑摘要要带行数与磁盘变化：%s", got)
	}
}
