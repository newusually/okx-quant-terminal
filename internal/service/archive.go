package service

// archive.go —— 月度 K 线归档导出（15m，按月分片 gzip）
//
// ---------------------------------------------------------------------------
// 它解决什么问题
// ---------------------------------------------------------------------------
// 用户口径（2026-10-01）：
//
//	「15 分钟一年数据保留，其余的分文件上传到 github 中，
//	  并且同步每个月月底按月份上传到 github 中，自动上传。」
//
// 也就是：本地 K 线留一年 → 每个月月底把**上个月**整月导出成一个归档文件 →
// 推到 GitHub 的独立数据仓库。
//
// 为什么不能直接 push 数据库文件：MySQL 的 .ibd 对 git 毫无意义（几十 MB 的
// 二进制块，diff 不可读、增量传输也没优势）。导出成「按行、按时间排序、
// 每行一根 K 线」的 CSV 再 gzip，才能做到：
//   · 每个月一个文件，一眼看出对应哪段时间
//   · gzip 后约 25MB/月，远低于 GitHub 的 100MB 单文件硬上限
//   · 纯文本，将来 git diff 能看出「哪一天的行情变了」（数据修正时很有用）
//
// ---------------------------------------------------------------------------
// 为什么要分片
// ---------------------------------------------------------------------------
// GitHub 单文件硬上限 100MB。一个月 134 万行压完约 25MB 是安全的，
// 但**不能假设未来永远是这个量**（合约数会涨、以后可能连 5m 一起归档）。
// 所以这里按**压缩后字节数**滚动分片（默认 40MB 一片）——
// 直接盯住 GitHub 的限制，而不是拍一个「每片多少行」的魔数。

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"finally-main/internal/conf"
	"finally-main/internal/logx"
	"finally-main/internal/repo"
)

// archiveChunkBytes 单片的压缩后字节上限。40MB 是留给 GitHub 100MB 硬上限的
// 安全余量（分片边界只在一个 row 之后检查，最多多出几 KB）。
const archiveChunkBytes = 40 << 20

// archiveFlushRows 每写多少行 flush 一次 csv 缓冲。
// 不 flush 的话 countingWriter 读到的一直是空，分片永远不触发。
const archiveFlushRows = 20000

// ArchivePart 一个分片文件
type ArchivePart struct {
	Name   string `json:"name"`
	Rows   int64  `json:"rows"`
	Bytes  int64  `json:"bytes"`
	Sha256 string `json:"sha256"`
}

// ArchiveManifest 一次月度归档的清单（和分片放一起，跟着推上 GitHub）
type ArchiveManifest struct {
	Month       string        `json:"month"`     // 2026-09
	Bar         string        `json:"bar"`       // 15m
	Source      string        `json:"source"`    // 数据来源说明
	GeneratedAt string        `json:"generatedAt"`
	From        string        `json:"from"`        // 该月第一根 K 线时间
	To          string        `json:"to"`          // 该月最后一根 K 线时间
	Insts       int64         `json:"insts"`       // 涉及多少个合约
	TotalRows   int64         `json:"totalRows"`   //
	TotalBytes  int64         `json:"totalBytes"`  //
	Columns     []string      `json:"columns"`     // CSV 列序
	TsUnit      string        `json:"tsUnit"`      // ts 的单位说明
	Parts       []ArchivePart `json:"parts"`       //
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

// ExportMonth 把某个月（`2026-09`）的 K 线导出到 outDir，按需分片。
//
// 返回清单；调用方负责把清单写到 `manifest-<月>.json` 并提交推送。
// 该月无数据时返回 rows=0 的空清单（不算错误）。
func ExportMonth(ym, outDir string) (*ArchiveManifest, error) {
	from, to, err := repo.MonthRange(ym)
	if err != nil {
		return nil, err
	}

	cfg := conf.LoadConfig()
	store := repo.NewStore(cfg)
	db, err := store.DB()
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}

	man := &ArchiveManifest{
		Month:       ym,
		Bar:         "15m",
		Source:      "okx-quant-terminal · MySQL okx.kline（只存 15m）",
		GeneratedAt: time.Now().Format("2006-01-02 15:04:05"),
		Columns:     csvColumns,
		TsUnit:      "毫秒 Unix 时间戳（UTC+8 本地自然月切分）",
	}
	if cfg != nil && cfg.Store != nil && cfg.Store.KlineRetainDays > 0 {
		man.Note = fmt.Sprintf("本地 K 线保留窗口 %d 天；本文件为该月全量快照，与保留策略无关。",
			cfg.Store.KlineRetainDays)
	}

	var (
		gz       *gzip.Writer
		csvw     *csv.Writer
		file     *os.File
		cw       *countingWriter
		hasher   io.Writer
		hash     = sha256.New()
		partNo   int
		part     ArchivePart
		partRows int64
		first    int64
		last     int64
		// 同一合约出现次数（用来统计涉及多少合约，避免再查一次库）
		insts = map[string]struct{}{}
	)

	openPart := func() error {
		partNo++
		part = ArchivePart{
			Name: fmt.Sprintf("kline-15m-%s.part%02d.csv.gz", ym, partNo),
		}
		partRows = 0
		hash = sha256.New()
		p := filepath.Join(outDir, part.Name)
		f, err := os.Create(p)
		if err != nil {
			return err
		}
		file = f
		cw = &countingWriter{w: f}
		hasher = io.MultiWriter(cw, hash)
		gz = gzip.NewWriter(hasher)
		csvw = csv.NewWriter(gz)
		return csvw.Write(csvColumns)
	}

	// sealPart 收尾当前分片：**先 flush + close，再统计行数/字节/哈希**。
	//
	// ★ 顺序绝对不能反 ★
	// csvw.Flush() 会把缓冲里最后不到 2 万行吐出来，
	// gz.Close() 会补写 gzip 尾部 8 字节（CRC32 + 原始长度）。
	// 早一步统计就会漏掉这两部分 —— 实测漏了 22,649 字节，
	// 清单里的 sha256 和实际文件对不上，校验会直接失败。
	sealPart := func() error {
		if file == nil {
			return nil
		}
		csvw.Flush()
		if err := csvw.Error(); err != nil {
			file.Close()
			return err
		}
		if err := gz.Close(); err != nil {
			file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		part.Rows = partRows
		part.Bytes = cw.n
		part.Sha256 = hex.EncodeToString(hash.Sum(nil))
		man.Parts = append(man.Parts, part)
		file, gz, csvw, cw = nil, nil, nil, nil
		return nil
	}

	// 先开第一片
	if err := openPart(); err != nil {
		return nil, err
	}

	werr := db.StreamKlines(from, to, func(k repo.Kline) error {
		rec := []string{
			k.InstID, k.Bar, strconv.FormatInt(k.Ts, 10),
			f2s(k.O), f2s(k.H), f2s(k.L), f2s(k.C), f2s(k.V),
		}
		if err := csvw.Write(rec); err != nil {
			return err
		}
		man.TotalRows++
		partRows++
		insts[k.InstID] = struct{}{}
		if first == 0 || k.Ts < first {
			first = k.Ts
		}
		if k.Ts > last {
			last = k.Ts
		}

		if partRows%archiveFlushRows == 0 {
			csvw.Flush()
			if err := csvw.Error(); err != nil {
				return err
			}
			if cw.n >= archiveChunkBytes {
				// 到上限了：封掉本片，滚到下一片继续写
				if err := sealPart(); err != nil {
					return err
				}
				if err := openPart(); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if werr != nil {
		if file != nil {
			file.Close()
		}
		return nil, werr
	}

	if man.TotalRows == 0 {
		// 该月没数据：收掉空文件，别留一个只有表头的垃圾
		if file != nil {
			file.Close()
			os.Remove(filepath.Join(outDir, part.Name))
		}
		man.Parts = nil
		return man, nil
	}

	// 收尾最后一片
	if err := sealPart(); err != nil {
		return nil, err
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
		add(fmt.Sprintf("  %-44s %9d 行  %8.1f MB  sha256:%s…\n",
			p.Name, p.Rows, float64(p.Bytes)/1048576, p.Sha256[:12]))
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
//	kline-15m-2026-09.part01.csv.gz
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
	// bar 名里可能带数字（15m/1H），所以不能只看长度就信。
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

// ArchiveExists 判断某个月（"YYYY-MM"）是否已经有完整归档落地。
//
// 只看 `manifest-<ym>.json` —— 它是 ExportMonth 跑完之后由 WriteManifest
// 最后写出的，所以「manifest 在」就等于「那个月的分片都写完了」。
// 反过来，如果只看到 .gz 而没有 manifest，说明上次导出是中途挂掉的，
// 这里返回 false，下一轮会重新导出（同名分片会被覆盖，幂等）。
func ArchiveExists(dir, ym string) bool {
	if archiveMonthOf("manifest-"+ym+".json") != ym {
		return false // 参数本身不是合法 YYYY-MM
	}
	st, err := os.Stat(filepath.Join(dir, "manifest-"+ym+".json"))
	return err == nil && !st.IsDir()
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
