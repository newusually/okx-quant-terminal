package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"finally-main/internal/model"
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

		// ★ 四周期归档的分片名（bar 段里带数字，最容易解析错）★
		{"kline-1m-2026-09.part01.csv.gz", "2026-09"},
		{"kline-3m-2026-09.part02.csv.gz", "2026-09"},
		{"kline-5m-2026-10.part01.csv.gz", "2026-10"},

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

// writeArchiveFile 在测试目录里落一个文件
func writeArchiveFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestArchiveExists 覆盖「某月是否已有完整归档」的全部判据。
//
// ★ 这个函数从「只看文件在不在」升级成「逐周期核对」★
// 一期只归档 15m，老清单里没有 1m/3m/5m。若仍只看文件存在，
// 那三个周期永远不会被归档 —— 静默漏归档比重复导出危险得多。
func TestArchiveExists(t *testing.T) {
	dir := t.TempDir()
	bars := exportBars()
	if len(bars) == 0 {
		t.Fatal("exportBars() 不该为空")
	}

	// (a) 什么都没有
	if ArchiveExists(dir, "2026-07") {
		t.Error("2026-07 什么都没有，应为 false")
	}

	// (b) 只有分片、没有 manifest = 上次导出中途挂了 → 要重导
	writeArchiveFile(t, dir, "kline-15m-2026-08.part01.csv.gz", "x")
	if ArchiveExists(dir, "2026-08") {
		t.Error("2026-08 只有分片没有 manifest，应判定为未完成")
	}

	// (c) 一期老清单（只有 bar 单值 "15m"）：缺 1m/3m/5m → 不算齐
	writeArchiveFile(t, dir, "manifest-2026-09.json",
		`{"month":"2026-09","bar":"15m","totalRows":100}`)
	if ArchiveExists(dir, "2026-09") {
		t.Error("老清单只覆盖 15m，缺 1m/3m/5m，应判定为未完成（要重导覆盖）")
	}

	// (d) 四周期齐全 → true（新格式：bars 数组）
	full, _ := json.Marshal(map[string]any{
		"month": "2026-09", "bar": strings.Join(bars, ","), "bars": bars, "totalRows": 100,
	})
	writeArchiveFile(t, dir, "manifest-2026-09.json", string(full))
	if !ArchiveExists(dir, "2026-09") {
		t.Errorf("四周期齐全（%v），应判定为已归档", bars)
	}

	// (e) 只有 bar 逗号串（没有 bars 数组）也要能认出齐全 —— 兼容中间版本
	writeArchiveFile(t, dir, "manifest-2026-12.json",
		`{"month":"2026-12","bar":"`+strings.Join(bars, ",")+`","totalRows":1}`)
	if !ArchiveExists(dir, "2026-12") {
		t.Error("bar 逗号串已覆盖全部周期，应判定为已归档")
	}

	// (f) 空 JSON / 畸形 JSON → 不能误判为已归档
	writeArchiveFile(t, dir, "manifest-2026-10.json", "{}")
	if ArchiveExists(dir, "2026-10") {
		t.Error("空清单没有周期信息，应判定为未完成")
	}
	writeArchiveFile(t, dir, "manifest-2026-11.json", "这不是 JSON")
	if ArchiveExists(dir, "2026-11") {
		t.Error("畸形清单应判定为未完成")
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

// TestExportBarsFollowsEnabledBars 归档周期必须跟 model.EnabledBars 走。
//
// 「归档覆盖哪些周期」不能有第二份名单：model.EnabledBars 是周期白名单的
// 唯一权威（repo 的 K 线清理、service 的扫描/回补都读它）。
// 一旦归档自己写死一个周期列表，就会出现「某周期在库里有数据、
// 但永久不被归档」的静默漏洞 —— 而 K 线只有 10 天窗口，删了就没了。
func TestExportBarsFollowsEnabledBars(t *testing.T) {
	got := exportBars()
	if len(got) != len(model.EnabledBars) {
		t.Fatalf("exportBars() = %v，期望与 model.EnabledBars(%v) 等长",
			got, model.EnabledBars)
	}
	for i, b := range model.EnabledBars {
		if got[i] != b {
			t.Errorf("第 %d 个周期 = %q，期望 %q", i, got[i], b)
		}
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
