package service

import (
	"os"
	"path/filepath"
	"testing"
)

// 归档侧「只留当月」的判据 —— 这一段在磁盘充足时永远不会在线上跑到，
// 而它一旦判错就是**删掉唯一的本地副本**，所以必须用单测钉死。
//
// 两个关键点：
//  1. 文件名解析必须只认自己生成的那两种命名，别的一律返回 ""（不动它）；
//  2. YYYY-MM 是定宽零填充，字符串比较 == 时间比较，所以 `ym >= keepYM`
//     就是「当月或未来」，绝对不删。

func TestArchiveMonthOf(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		// 自己生成的两种 —— 必须认出来
		{"kline-15m-2026-09.part01.csv.gz", "2026-09"},
		{"kline-15m-2026-09.part12.csv.gz", "2026-09"},
		{"kline-1H-2026-12.part01.csv.gz", "2026-12"},
		{"manifest-2026-09.json", "2026-09"},

		// 不像归档产物的 —— 一律不动（宁可漏删，不能误删）
		{"README.md", ""},
		{"manifest-.json", ""},
		{"manifest-2026-9.json", ""},   // 月份没补零，不是我们的格式
		{"manifest-20260-09.json", ""}, // 年是 5 位
		{"manifest-2026-09.txt", ""},   // 后缀不对
		{"kline-15m-2026-09.txt", ""},  // 后缀不对
		{"kline-15m.csv.gz", ""},       // 没有年月
		{"kline-2026-09.part01.csv.gz", ""}, // 缺 bar 段
		{".gitignore", ""},
		{"kline-15m-2026-AB.part01.csv.gz", ""}, // 月份不是数字
		{"manifest-2026-0a.json", ""},
	}
	for _, c := range cases {
		if got := archiveMonthOf(c.name); got != c.want {
			t.Errorf("archiveMonthOf(%q) = %q, 期望 %q", c.name, got, c.want)
		}
	}
}

func TestArchiveExists(t *testing.T) {
	dir := t.TempDir()
	// 有 manifest 才算「完整归档」——它是最后写出的
	if err := os.WriteFile(filepath.Join(dir, "manifest-2026-09.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 只有分片没有 manifest = 上次导出中途挂了，不算存在（要重导）
	if err := os.WriteFile(filepath.Join(dir, "kline-15m-2026-08.part01.csv.gz"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !ArchiveExists(dir, "2026-09") {
		t.Error("2026-09 有 manifest，应判定为已归档")
	}
	if ArchiveExists(dir, "2026-08") {
		t.Error("2026-08 只有分片没有 manifest，应判定为未完成")
	}
	if ArchiveExists(dir, "2026-07") {
		t.Error("2026-07 什么都没有，应为 false")
	}
	// 参数非法时不能把畸形文件名当命中
	if ArchiveExists(dir, "2026-9") || ArchiveExists(dir, "") {
		t.Error("非法月份参数应返回 false")
	}
}

// TestPruneArchivesBefore 覆盖三件事：
//   - 只删严格早于 keepYM 的
//   - 当月与未来一个都不动
//   - 无法识别的文件（README 等）一个都不动
//   - dryRun 只数不删
func TestPruneArchivesBefore(t *testing.T) {
	dir := t.TempDir()
	files := []string{
		"kline-15m-2026-07.part01.csv.gz",
		"manifest-2026-07.json",
		"kline-15m-2026-08.part01.csv.gz",
		"manifest-2026-08.json",
		"kline-15m-2026-09.part01.csv.gz",
		"manifest-2026-09.json",
		"kline-15m-2026-10.part01.csv.gz", // 当月 —— 不许删
		"manifest-2026-10.json",
		"README.md", // 不是归档产物 —— 不许删
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("dummy"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 判据是「**严格早于** keepYM」，所以 keepYM=2026-10 时
	// 07 / 08 / 09 三个月的归档都要删（各 2 个文件 = 6 个），
	// 只留下当月的 2026-10 和不是归档产物的 README.md。
	const wantDel = 6

	// --- 预演：只数不删 ---
	n, bytes, err := PruneArchivesBefore(dir, "2026-10", true)
	if err != nil {
		t.Fatalf("预演报错：%v", err)
	}
	if n != wantDel {
		t.Errorf("预演应数到 %d 个（07/08/09 各 2 个文件），实际 %d", wantDel, n)
	}
	if bytes != int64(wantDel*len("dummy")) {
		t.Errorf("预演字节数 = %d，期望 %d", bytes, wantDel*len("dummy"))
	}
	if got := len(mustReadDir(t, dir)); got != len(files) {
		t.Fatalf("预演不应删文件：原 %d 个，现 %d 个", len(files), got)
	}

	// --- 真删 ---
	n, bytes, err = PruneArchivesBefore(dir, "2026-10", false)
	if err != nil {
		t.Fatalf("真删报错：%v", err)
	}
	if n != wantDel || bytes != int64(wantDel*len("dummy")) {
		t.Errorf("真删结果 = %d 个 / %d 字节，期望 %d / %d",
			n, bytes, wantDel, wantDel*len("dummy"))
	}

	left := mustReadDir(t, dir)
	want := map[string]bool{
		"kline-15m-2026-10.part01.csv.gz": true, // 当月，保留
		"manifest-2026-10.json":           true, // 当月，保留
		"README.md":                       true, // 非归档产物，永远不动
	}
	if len(left) != len(want) {
		t.Fatalf("剩余文件数 %d，期望 %d；实际：%v", len(left), len(want), left)
	}
	for _, f := range left {
		if !want[f] {
			t.Errorf("不该保留/不该删的文件：%s", f)
		}
	}
}

// TestPruneArchivesBeforeMissingDir 目录还不存在（从没归档过）不是错误。
func TestPruneArchivesBeforeMissingDir(t *testing.T) {
	n, b, err := PruneArchivesBefore(filepath.Join(t.TempDir(), "nope"), "2026-10", false)
	if err != nil {
		t.Fatalf("目录不存在不应报错，实际：%v", err)
	}
	if n != 0 || b != 0 {
		t.Errorf("空目录应返回 0/0，实际 %d/%d", n, b)
	}
}

func mustReadDir(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}
