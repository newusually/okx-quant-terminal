package service

// archive.go —— 月度 K 线归档导出（1m/3m/5m/15m，按周期分组、按月分片 gzip）
//
// ---------------------------------------------------------------------------
// 它解决什么问题
// ---------------------------------------------------------------------------
// 用户口径（2026-10-01）：
//
//	「15 分钟一年数据保留，其余的分文件上传到 github 中，
//	  并且同步每个月月底按月份上传到 github 中，自动上传。」
//
// 也就是：本地 K 线留一段窗口 → 每个月月底把**上个月**整月导出成归档文件 →
// 推到 GitHub 的独立数据仓库。
//
// ★ 二期口径变更（2026-10-01 第二轮）★
//   本地保留窗口从「15m 留一年」改成「四个周期都只留 10 天」，
//   四周期（1m/3m/5m/15m）都参与开仓 ⇒ 归档也必须覆盖四个周期。
//   否则 1m/3m/5m 会「被每日任务删掉且从未留底」= 永久损失。
//   代价是归档内容变薄：月度任务每月 1 号跑，而此时上月的数据
//   只剩最后约 10 天（更早的已被每日任务 DROP）—— 这是 10 天窗口的
//   必然结果，不是 bug。若要完整月度归档，须加大 kline_retain_days。
//
// 为什么不能直接 push 数据库文件：MySQL 的 .ibd 对 git 毫无意义（几十 MB 的
// 二进制块，diff 不可读、增量传输也没优势）。导出成「按行、按时间排序、
// 每行一根 K 线」的 CSV 再 gzip，才能做到：
//   · 每个月一个文件，一眼看出对应哪段时间
//   · 纯文本，将来 git diff 能看出「哪一天的行情变了」（数据修正时很有用）
//
// ---------------------------------------------------------------------------
// 为什么要分片
// ---------------------------------------------------------------------------
// GitHub 单文件硬上限 100MB。所以这里按**压缩后字节数**滚动分片
// （默认 40MB 一片）—— 直接盯住 GitHub 的限制，而不是拍一个
// 「每片多少行」的魔数。分片号**按周期各自独立编号**
// （kline-1m-2026-09.part01.csv.gz / kline-15m-2026-09.part01.csv.gz），
// 这样一个周期写崩了不影响其它周期已完成的分片。

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"finally-main/internal/conf"
	"finally-main/internal/logx"
	"finally-main/internal/model"
	"finally-main/internal/repo"
)

// archiveChunkBytes 单片的压缩后字节上限。40MB 是留给 GitHub 100MB 硬上限的
// 安全余量（分片边界只在一个 row 之后检查，最多多出几 KB）。
const archiveChunkBytes = 40 << 20

// archiveFlushRows 每写多少行 flush 一次 csv 缓冲。
// 不 flush 的话 countingWriter 读到的一直是空，分片永远不触发。
const archiveFlushRows = 20000

// ★ 归档为什么不做「按天切片」★（2026-10-01 二期，踩过又回退）
//
// 一开始以为「一次查一个月太慢」，于是按 24 小时切成 30 段。**实测更慢**
// （2025-10：整月一次 4m58s → 切片后 >5m 没跑完）。
//
// 原因在 EXPLAIN 里看得很清楚：`WHERE ts >= ? AND ts < ?` 在 kline 上
// **没有任何 ts 前缀索引**（主键是 inst_id,bar,ts，前缀是 inst_id），
// 所以这个条件只能靠「全分区扫描 + 过滤」实现。
// 切 30 段 = 把同一个分区扫 30 遍。
//
// 真正让归档变快的是**分区本身**：
//   · 冷区按月切 ⇒ 查某个月只命中它自己的 p<YYYY_MM> 分区（约 85 万行），
//     而不是掉进 p_old 兜底分区（实测 945 万行 —— 这才是 5 分钟的元凶）；
//   · 热区按天切 ⇒ 查最近几天更是只命中一个日分区。
// 所以归档前必须确保跑过 -repartition（把历史数据从 p_old 摊回各月分区）。

// ArchivePart 一个分片文件
type ArchivePart struct {
	Name   string `json:"name"`
	Bar    string `json:"bar,omitempty"` // 该分片属于哪个周期（旧清单无此字段）
	Rows   int64  `json:"rows"`
	Bytes  int64  `json:"bytes"`
	Sha256 string `json:"sha256"`
}

// ArchiveManifest 一次月度归档的清单（和分片放一起，跟着推上 GitHub）
type ArchiveManifest struct {
	Month string `json:"month"` // 2026-09
	// Bar 历史字段：最初只归档 15m，这里就是 "15m"。
	// 四周期归档后填逗号串（"1m,3m,5m,15m"），仅供人看；
	// 程序判据请用 Bars —— 它才是「本次覆盖了哪些周期」的权威。
	Bar         string        `json:"bar"`
	Bars        []string      `json:"bars,omitempty"` // 本次覆盖的全部周期
	Source      string        `json:"source"`         // 数据来源说明
	GeneratedAt string        `json:"generatedAt"`
	From        string        `json:"from"`       // 该月第一根 K 线时间
	To          string        `json:"to"`         // 该月最后一根 K 线时间
	Insts       int64         `json:"insts"`      // 涉及多少个合约
	TotalRows   int64         `json:"totalRows"`  //
	TotalBytes  int64         `json:"totalBytes"` //
	Columns     []string      `json:"columns"`    // CSV 列序
	TsUnit      string        `json:"tsUnit"`     // ts 的单位说明
	Parts       []ArchivePart `json:"parts"`      //
	Note        string        `json:"note,omitempty"`
}

// csvColumns 归档 CSV 的列序（与 kline 表一致，不做任何加工）
var csvColumns = []string{"inst_id", "bar", "ts", "o", "h", "l", "c", "v"}

// ArchiveDir 归档根目录（绝对路径）
func ArchiveDir() string {
	c := conf.LoadConfig()
	if c == nil || c.Store == nil || c.Store.ArchiveDir == "" {
		return "archive"
	}
	return c.Resolve(c.Store.ArchiveDir)
}

// exportBars 归档要覆盖的周期 = model.EnabledBars（唯一权威）。
//
// 这里刻意不读配置文件里的 bars_enabled —— 周期名单的唯一真源是
// model.EnabledBars（repo 层也要用它做 K 线清理），多一处副本就会走岔。
func exportBars() []string {
	out := make([]string, 0, len(model.EnabledBars))
	for _, b := range model.EnabledBars {
		if s := strings.TrimSpace(b); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// countingWriter 统计已写入的字节数（用来判断该不该分片）
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// archiveBarWriter 一个周期的分片写出器。
//
// ★ 一个周期一组文件、各自独立编号 ★
// 为什么不让四个周期共用一个 partNo 序列：分片是按「压缩后 40MB」滚的，
// 四个周期的数据量差好几个数量级（1m 是 15m 的 15 倍），
// 共用编号会出现「part07 是 1m、part08 是 15m」这种无法预测的对应关系，
// 将来想单独恢复 1m 就得先读 manifest 才能知道拿哪几个文件。
type archiveBarWriter struct {
	bar     string
	ym      string
	outDir  string
	man     *ArchiveManifest
	partNo  int
	curName string
	partRows int64

	file *os.File
	gz   *gzip.Writer
	csvw *csv.Writer
	cw   *countingWriter
	hash hash.Hash
}

// openPart 开一个新分片（含表头）
func (w *archiveBarWriter) openPart() error {
	w.partNo++
	w.partRows = 0
	w.hash = sha256.New()
	w.curName = fmt.Sprintf("kline-%s-%s.part%02d.csv.gz", w.bar, w.ym, w.partNo)

	f, err := os.Create(filepath.Join(w.outDir, w.curName))
	if err != nil {
		return err
	}
	w.file = f
	w.cw = &countingWriter{w: f}
	w.gz = gzip.NewWriter(io.MultiWriter(w.cw, w.hash))
	w.csvw = csv.NewWriter(w.gz)
	return w.csvw.Write(csvColumns)
}

// seal 收尾当前分片：**先 flush + close，再统计行数/字节/哈希**。
//
// ★ 顺序绝对不能反 ★
// csvw.Flush() 会把缓冲里最后不到 2 万行吐出来，
// gz.Close() 会补写 gzip 尾部 8 字节（CRC32 + 原始长度）。
// 早一步统计就会漏掉这两部分 —— 实测漏了 22,649 字节，
// 清单里的 sha256 和实际文件对不上，校验会直接失败。
func (w *archiveBarWriter) seal() error {
	if w.file == nil {
		return nil
	}
	w.csvw.Flush()
	if err := w.csvw.Error(); err != nil {
		w.file.Close()
		return err
	}
	if err := w.gz.Close(); err != nil {
		w.file.Close()
		return err
	}
	if err := w.file.Close(); err != nil {
		return err
	}
	w.man.Parts = append(w.man.Parts, ArchivePart{
		Name:   w.curName,
		Bar:    w.bar,
		Rows:   w.partRows,
		Bytes:  w.cw.n,
		Sha256: hex.EncodeToString(w.hash.Sum(nil)),
	})
	w.file, w.gz, w.csvw, w.cw, w.hash = nil, nil, nil, nil, nil
	return nil
}

// abort 出错时收掉当前分片，不留半截文件，也不进清单
func (w *archiveBarWriter) abort() {
	if w.file != nil {
		w.file.Close()
		w.file = nil
	}
	if w.curName != "" {
		os.Remove(filepath.Join(w.outDir, w.curName))
	}
}

// dropEmpty 该周期整月一行都没有：收掉只有表头的空文件
func (w *archiveBarWriter) dropEmpty() {
	if w.file != nil {
		w.file.Close()
	}
	os.Remove(filepath.Join(w.outDir, w.curName))
	w.file = nil
}

// ExportMonth 把某个月（`2026-09`）的 K 线导出到 outDir，**每个周期一组分片**。
//
// 返回清单；调用方负责把清单写到 `manifest-<月>.json` 并提交推送。
// 该月无数据时返回 rows=0 的空清单（不算错误）。
func ExportMonth(ym, outDir string) (*ArchiveManifest, error) {
	from, to, err := repo.MonthRange(ym)
	if err != nil {
		return nil, err
	}

	bars := exportBars()
	if len(bars) == 0 {
		bars = []string{"15m"} // EnabledBars 被人为清空时的兜底
	}

	cfg := conf.LoadConfig()

	// ★ 归档必须用「不超时」的连接 ★
	// 月度归档是**长时间只读顺序扫描**：单月 15m 就 85 万行，
	// 而 `ORDER BY inst_id, ts` 在 kline 上没有对应索引（主键是
	// inst_id,bar,ts，见 indexes.go），只能 filesort —— 实测量级是分钟。
	// 日常 DSN 里 readTimeout=60s，时间一到驱动直接断开，
	// 报出来是 `read tcp …: i/o timeout` + `invalid connection`，
	// 而 ExportMonth 自己完全看不出问题在哪（踩过一次）。
	// 所以这里跟 -partition / -repartition 走同一条路：LongDDL 关掉读写超时。
	mcfg := repo.DefaultMySQLConfig()
	mcfg.LongDDL = true
	db, err := repo.OpenMySQL(mcfg)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}

	man := &ArchiveManifest{
		Month:       ym,
		Bar:         strings.Join(bars, ","),
		Bars:        append([]string{}, bars...),
		Source:      "okx-quant-terminal · MySQL okx.kline（" + strings.Join(bars, "/") + "）",
		GeneratedAt: time.Now().Format("2006-01-02 15:04:05"),
		Columns:     csvColumns,
		TsUnit:      "毫秒 Unix 时间戳（UTC+8 本地自然月切分）",
	}
	if cfg != nil && cfg.Store != nil && cfg.Store.KlineRetainDays > 0 {
		man.Note = fmt.Sprintf("本地 K 线保留窗口 %d 天；本文件为该周期在该月的全部本地数据，"+
			"与保留策略无关（更早的已由每日任务清理）。", cfg.Store.KlineRetainDays)
	}

	var (
		first int64
		last  int64
		// 同一合约出现次数（用来统计涉及多少合约，避免再查一次库）
		insts = map[string]struct{}{}
	)

	for _, bar := range bars {
		w := &archiveBarWriter{bar: bar, ym: ym, outDir: outDir, man: man}
		if err := w.openPart(); err != nil {
			return nil, err
		}

		werr := db.StreamKlines(from, to, bar, func(k repo.Kline) error {
			rec := []string{
				k.InstID, k.Bar, strconv.FormatInt(k.Ts, 10),
				f2s(k.O), f2s(k.H), f2s(k.L), f2s(k.C), f2s(k.V),
			}
			if err := w.csvw.Write(rec); err != nil {
				return err
			}
			man.TotalRows++
			w.partRows++
			insts[k.InstID] = struct{}{}
			if first == 0 || k.Ts < first {
				first = k.Ts
			}
			if k.Ts > last {
				last = k.Ts
			}

			if w.partRows%archiveFlushRows == 0 {
				w.csvw.Flush()
				if err := w.csvw.Error(); err != nil {
					return err
				}
				if w.cw.n >= archiveChunkBytes {
					// 到上限了：封掉本片，滚到下一片继续写
					if err := w.seal(); err != nil {
						return err
					}
					if err := w.openPart(); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if werr != nil {
			w.abort()
			return nil, werr
		}

		if w.partRows == 0 {
			// 该周期在这个月没数据：收掉空文件，别留一个只有表头的垃圾
			w.dropEmpty()
			continue
		}
		if err := w.seal(); err != nil {
			return nil, err
		}
	}

	if man.TotalRows == 0 {
		man.Parts = nil
		return man, nil
	}

	man.Insts = int64(len(insts))
	if first > 0 {
		man.From = time.UnixMilli(first).Format("2006-01-02 15:04:05")
	}
	if last > 0 {
		man.To = time.UnixMilli(last).Format("2006-01-02 15:04:05")
	}
	for _, p := range man.Parts {
		man.TotalBytes += p.Bytes
	}
	return man, nil
}

// WriteManifest 把清单落盘成 manifest-<月>.json（缩进过，方便人看与 git diff）
func WriteManifest(outDir string, man *ArchiveManifest) (string, error) {
	b, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return "", err
	}
	p := filepath.Join(outDir, "manifest-"+man.Month+".json")
	if err := os.WriteFile(p, append(b, '\n'), 0o644); err != nil {
		return "", err
	}
	return p, nil
}

// f2s float → 字符串。K 线价格位数不定，用 -1 精度（最短往返表示），
// 避免把 0.00001234 这类小币价写成 0.00。
func f2s(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// ArchiveText 把清单渲染成人看的文本（-archive 控制台输出）
func ArchiveText(man *ArchiveManifest) string {
	if man == nil {
		return "（无结果）"
	}
	var b []byte
	add := func(s string) { b = append(b, s...) }
	add(fmt.Sprintf("== K 线月度归档 · %s（%s）==\n", man.Month, man.Bar))
	add(fmt.Sprintf("数据来源：%s\n", man.Source))
	add(fmt.Sprintf("时间范围：%s ~ %s\n", man.From, man.To))
	add(fmt.Sprintf("规模    ：%d 行 / %d 个合约 / %.1f MB（压缩后）\n",
		man.TotalRows, man.Insts, float64(man.TotalBytes)/1048576))
	add(fmt.Sprintf("生成时间：%s\n", man.GeneratedAt))
	add("----------------------------------------------\n")
	for _, p := range man.Parts {
		name := p.Name
		sha := p.Sha256
		if len(sha) > 12 {
			sha = sha[:12]
		}
		add(fmt.Sprintf("  %-44s %9d 行  %8.1f MB  sha256:%s…\n",
			name, p.Rows, float64(p.Bytes)/1048576, sha))
	}
	add("----------------------------------------------\n")
	return string(b)
}

// logArchive 归档完成后打一行日志（失败不影响主流程）
func logArchive(man *ArchiveManifest, dir string, err error) {
	if err != nil {
		logx.Logf("WARN", "[ARCHIVE] 月度归档失败：%v", err)
		return
	}
	if man.TotalRows == 0 {
		logx.Logf("INFO", "[ARCHIVE] %s 无数据，跳过", man.Month)
		return
	}
	logx.Logf("INFO", "[ARCHIVE] %s 已导出：%d 行 / %d 合约 / %d 个分片 / %.1f MB → %s",
		man.Month, man.TotalRows, man.Insts, len(man.Parts),
		float64(man.TotalBytes)/1048576, dir)
}

// archiveMonthOf 从归档产物文件名里取出它属于哪个月（"YYYY-MM"）。
// 只认 ExportMonth / WriteManifest 生成的那两种命名：
//
//	kline-15m-2026-09.part01.csv.gz   （bar 名里可能带数字：1m/3m/5m/15m）
//	manifest-2026-09.json
//
// 返回 "" 表示「这不是我能识别的归档文件」—— 一律不动。宁可漏删，不能误删：
// 这个目录将来可能被塞进 README、校验脚本或别人手工放的说明文件。
func archiveMonthOf(name string) string {
	var s string
	switch {
	case strings.HasPrefix(name, "manifest-"):
		s = strings.TrimSuffix(strings.TrimPrefix(name, "manifest-"), ".json")
	case strings.HasPrefix(name, "kline-"):
		// kline-<bar>-<YYYY>-<MM>.partNN.csv.gz → 去掉前缀/后缀/分片号
		s = strings.TrimSuffix(strings.TrimPrefix(name, "kline-"), ".csv.gz")
		if i := strings.Index(s, ".part"); i >= 0 {
			s = s[:i]
		}
		parts := strings.Split(s, "-")
		if len(parts) < 3 {
			return ""
		}
		s = parts[len(parts)-2] + "-" + parts[len(parts)-1] // 末两段就是年、月
	default:
		return ""
	}
	// 校验恰好是 YYYY-MM：7 个字符、第 5 位是 '-'、其余全数字。
	// bar 名里可能带数字（15m/1m），所以不能只看长度就信。
	if len(s) != 7 || s[4] != '-' {
		return ""
	}
	for i, c := range s {
		if i == 4 {
			continue
		}
		if c < '0' || c > '9' {
			return ""
		}
	}
	return s
}

// archiveBarsOf 从清单里取出「本次覆盖了哪些周期」。
//
// 优先读 Bars（新格式）；只有 Bar（旧清单，单周期）时按逗号拆开兼容。
func archiveBarsOf(man *ArchiveManifest) map[string]bool {
	have := map[string]bool{}
	for _, b := range man.Bars {
		if s := strings.TrimSpace(b); s != "" {
			have[s] = true
		}
	}
	if len(have) == 0 && strings.TrimSpace(man.Bar) != "" {
		for _, b := range strings.Split(man.Bar, ",") {
			if s := strings.TrimSpace(b); s != "" {
				have[s] = true
			}
		}
	}
	return have
}

// ArchiveExists 判断某个月（"YYYY-MM"）是否已经有「覆盖当前全部在册周期」的
// 完整归档落地。
//
// 为什么不能只看 manifest 在不在：一期只归档 15m，老清单里没有 1m/3m/5m。
// 若只判「文件存在」，那三个周期的数据就永远不会被归档
// （任务以为「这个月已经做过了」）—— 静默漏归档比重复导出危险得多。
// 所以这里要把 manifest 读出来，逐周期核对：
//
//	缺任何一个在册周期 → 返回 false → 本轮重新导出（同名文件覆盖，幂等）
//
// 反过来，只看到 .gz 而没有 manifest，说明上次导出中途挂了，也返回 false。
func ArchiveExists(dir, ym string) bool {
	if archiveMonthOf("manifest-"+ym+".json") != ym {
		return false // 参数本身不是合法 YYYY-MM
	}
	p := filepath.Join(dir, "manifest-"+ym+".json")
	b, err := os.ReadFile(p)
	if err != nil || len(b) == 0 {
		return false
	}
	var man ArchiveManifest
	if err := json.Unmarshal(b, &man); err != nil {
		return false
	}
	want := exportBars()
	if len(want) == 0 {
		return true // 没有在册周期（异常态），别无限重导
	}
	have := archiveBarsOf(&man)
	for _, bar := range want {
		if !have[bar] {
			return false
		}
	}
	return true
}

// PruneArchivesBefore 删掉严格早于 keepYM（"YYYY-MM"）的归档产物，
// 返回删除的文件数与字节数。这是「磁盘守卫」的归档侧动作 —— 只保留当月。
//
// ★ 调用前必须确认这些归档**已经成功推到远端**（见 RunMonthlyMaintenance
// 里的顺序与门禁）。本地 archive/ 删掉之后就只剩远端那一份了。
//
// 判据用文件名做字符串比较：YYYY-MM 是定宽零填充，字典序 == 时间序，
// 所以 `ym >= keepYM` 就是「当月或未来」，直接跳过。
func PruneArchivesBefore(dir, keepYM string, dryRun bool) (files int, bytes int64, err error) {
	ents, rerr := os.ReadDir(dir)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return 0, 0, nil // 还没归档过，不是错误
		}
		return 0, 0, rerr
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		ym := archiveMonthOf(e.Name())
		if ym == "" || ym >= keepYM {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue // 拿不到大小就别删，宁可留着
		}
		if !dryRun {
			if derr := os.Remove(filepath.Join(dir, e.Name())); derr != nil {
				err = derr // 记最后一个错误，继续删其余的
				continue
			}
		}
		files++
		bytes += info.Size()
	}
	return files, bytes, err
}
