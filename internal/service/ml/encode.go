package ml

// attn_encode.go —— 原 okx/AttnEncoder.py 的 Go 版（按真实日志格式重写）
//
// ⚠️ 重要更正
// ---------------------------------------------------------------------------
// AttnEncoder.py / AttnDecoder.py / Trainer.py 这三个脚本里写的解析逻辑，是假设
// 日志每行形如：
//
//	"xxx,time--->>2025-1-2 15:04:05,a--->>0.1,b--->>0.2,e--->>0.3,a-b--->>0.1,
//	 b-e--->>0.1,eth_close--->>3200.5"
//
// 但 datas/log/buylog_*.txt 里真正躺着的数据是【两行一条】的：
//
//	2025-7-19 13:02:19,symbol--->>AVAAI-USDT-SWAP
//	,c1--->>1.00116,c2--->>1.00724,minute--->>1H
//
// 所以原 Python 脚本一跑就 IndexError（items[7] 根本不存在）。这也是它从来
// 没被跑通过的原因之一。
//
// 本 Go 版同时支持两种格式（自动识别），并且以真实格式为主：
//   · 真实格式 → 特征列 [c1, c2, spread, ratio, count]，标签 = 下一条的 c2
//   · 旧格式   → 特征列 [eth_close, a, b, e, a-b, b-e, macd, signal, hist]，
//                标签 = 下一根的 eth_close（保留原脚本口径，万一以后有这种日志）
//
// 依赖：只用 Go 标准库（Python 版的 numpy/talib/pandas 全部自研替代）。

import (
	"encoding/csv"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// LogFormat 日志格式
type LogFormat int

const (
	FormatUnknown LogFormat = iota
	// FormatRatio 真实格式：两行一条，c1/c2/count/minute
	FormatRatio
	// FormatLegacy 旧格式：单行，a/b/e/a-b/b-e/eth_close
	FormatLegacy
)

func (f LogFormat) String() string {
	switch f {
	case FormatRatio:
		return "ratio(c1/c2)"
	case FormatLegacy:
		return "legacy(a/b/e)"
	}
	return "unknown"
}

// BuyLog 一条买点记录（两种格式共用，用不到的字段留 0）
type BuyLog struct {
	Format LogFormat
	Time   time.Time
	Symbol string
	Minute string

	// 真实格式字段
	C1    float64 // 上一根 K 线的 收/开（mvc.GetKline 的 co1）
	C2    float64 // 最新一根 K 线的 收/开（co2）
	Count float64 // 日志里的 count 字段

	// 旧格式字段（AttnEncoder.py 期待的那套）
	EthClose float64
	A, B, E  float64
	AB, BE   float64
}

// ---------------------------------------------------------------------------
// 解析
// ---------------------------------------------------------------------------

// ParseBuyLog 解析日志文本，自动识别两种格式。
//
// 真实格式（两行一条）：
//
//	第 1 行  "2025-7-19 13:02:19,symbol--->>AVAAI-USDT-SWAP"
//	第 2 行  ",c1--->>1.00116,c2--->>1.00724,minute--->>1H"
//
// 旧格式（一行一条）：items[1]=时间 items[2..6]=a/b/e/a-b/b-e items[7]=eth_close
func ParseBuyLog(content string) []BuyLog {
	out := []BuyLog{}
	var pending *BuyLog

	flush := func() {
		if pending != nil {
			if pending.Symbol != "" || pending.C1 != 0 || pending.C2 != 0 {
				out = append(out, *pending)
			}
			pending = nil
		}
	}

	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		items := strings.Split(line, ",")

		// ---- 真实格式：含 symbol--->> 是记录头 ----
		if strings.Contains(line, "symbol--->>") {
			flush()
			rec := BuyLog{Format: FormatRatio}
			if len(items) > 0 {
				rec.Time = parseTimeLoose(strings.TrimSpace(items[0]))
			}
			rec.Symbol = strings.TrimSpace(afterArrow(items[1]))
			pending = &rec
			continue
		}

		// ---- 真实格式：以逗号开头、含 c1/c2 的是记录体 ----
		if pending != nil && (strings.Contains(line, "c1--->>") || strings.Contains(line, "c2--->>")) {
			for _, it := range items {
				if !strings.Contains(it, "--->>") {
					continue
				}
				key := strings.TrimSpace(strings.SplitN(it, "--->>", 2)[0])
				val := strings.TrimSpace(afterArrow(it))
				switch key {
				case "c1":
					pending.C1, _ = strconv.ParseFloat(val, 64)
				case "c2":
					pending.C2, _ = strconv.ParseFloat(val, 64)
				case "count":
					pending.Count, _ = strconv.ParseFloat(val, 64)
				case "minute":
					pending.Minute = val
				}
			}
			continue
		}

		// ---- 旧格式：一行 ≥8 段，且第 7 段含 eth_close 之类 ----
		if len(items) >= 8 && pending == nil {
			legacy := BuyLog{Format: FormatLegacy}
			legacy.Time = parseTimeLoose(strings.TrimSpace(afterArrow(items[1])))
			legacy.A = numAfterArrow(items[2])
			legacy.B = numAfterArrow(items[3])
			legacy.E = numAfterArrow(items[4])
			legacy.AB = numAfterArrow(items[5])
			legacy.BE = numAfterArrow(items[6])
			legacy.EthClose = numAfterArrow(items[7])
			if !math.IsNaN(legacy.EthClose) && legacy.EthClose > 0 {
				out = append(out, legacy)
			}
			continue
		}
	}
	flush()
	return out
}

func numAfterArrow(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(afterArrow(s)), 64)
	if err != nil {
		return math.NaN()
	}
	return v
}

func parseTimeLoose(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{
		"2006-01-02 15:04:05", "2006-1-2 15:04:05",
		"2006-01-02 15:04", "2006-1-2 15:04",
		time.RFC3339,
	} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
}

func afterArrow(s string) string {
	if i := strings.Index(s, ">>>"); i >= 0 {
		return strings.TrimSpace(s[i+3:])
	}
	return s
}

// DetectFormat 看一批记录是什么格式（取多数）
func DetectFormat(logs []BuyLog) LogFormat {
	if len(logs) == 0 {
		return FormatUnknown
	}
	nRatio, nLegacy := 0, 0
	for _, l := range logs {
		switch l.Format {
		case FormatRatio:
			nRatio++
		case FormatLegacy:
			nLegacy++
		}
	}
	if nRatio >= nLegacy {
		return FormatRatio
	}
	return FormatLegacy
}

// ---------------------------------------------------------------------------
// MACD（talib 口径，旧格式才用得上，但保留）
// ---------------------------------------------------------------------------

// EmaTalib talib 的 EMA：第 period-1 个用前 period 个的 SMA 做种子，之后递推。
//
// 和 runtime/indicators.go 里的 ema()（用 x[0] 做种子）不同，
// 这里必须用 SMA 种子才能和 Python 的 talib.MACD 对上。
func EmaTalib(x []float64, period int) []float64 {
	n := len(x)
	out := make([]float64, n)
	for i := range out {
		out[i] = math.NaN()
	}
	if period <= 0 || n < period {
		return out
	}
	sum := 0.0
	for i := 0; i < period; i++ {
		sum += x[i]
	}
	out[period-1] = sum / float64(period)
	k := 2.0 / float64(period+1)
	for i := period; i < n; i++ {
		out[i] = (x[i]-out[i-1])*k + out[i-1]
	}
	return out
}

// MacdTalib 对应 talib.MACD(close, 12, 26, 9)，返回 (macd, signal, hist)
func MacdTalib(close []float64, fast, slow, signal int) (macd, sig, hist []float64) {
	n := len(close)
	macd, sig, hist = make([]float64, n), make([]float64, n), make([]float64, n)
	ef := EmaTalib(close, fast)
	es := EmaTalib(close, slow)
	for i := 0; i < n; i++ {
		if math.IsNaN(ef[i]) || math.IsNaN(es[i]) {
			macd[i] = math.NaN()
			continue
		}
		macd[i] = ef[i] - es[i]
	}
	sig = EmaTalib(fillNaN(macd), signal)
	for i := 0; i < n; i++ {
		if math.IsNaN(macd[i]) || math.IsNaN(sig[i]) {
			hist[i] = math.NaN()
			continue
		}
		hist[i] = macd[i] - sig[i]
	}
	return
}

// fillNaN 把前导 NaN 用 0 回填，保证 EMA 长度对齐
func fillNaN(x []float64) []float64 {
	out := make([]float64, len(x))
	first := -1
	for i, v := range x {
		if !math.IsNaN(v) {
			first = i
			break
		}
	}
	if first < 0 {
		return out
	}
	copy(out[first:], x[first:])
	return out
}

// ---------------------------------------------------------------------------
// 特征表
// ---------------------------------------------------------------------------

// AttnFeatures 特征表
type AttnFeatures struct {
	Format    LogFormat
	Rows      []BuyLog
	Names     []string    // 列名
	Matrix    [][]float64 // [样本][特征]
	Target    []float64   // 预测目标（下一条的第 TargetCol 列）
	TargetCol int         // 目标列下标

	Times   []time.Time
	Symbols []string
}

// BuildAttnFeatures 组特征矩阵。
//
// 真实格式列：c1, c2, spread(c1-c2), ratio(c1/c2), count
// 旧格式列  ：eth_close, a, b, e, a-b, b-e, macd, signal, hist
//
// Target 一律取「下一条记录的第 TargetCol 列」，对应原脚本「预测下一根收盘价」。
func BuildAttnFeatures(rows []BuyLog) *AttnFeatures {
	f := &AttnFeatures{Rows: rows, Format: DetectFormat(rows)}
	n := len(rows)
	if n == 0 {
		f.Names = []string{}
		return f
	}

	switch f.Format {
	case FormatLegacy:
		close := make([]float64, n)
		a := make([]float64, n)
		b := make([]float64, n)
		e := make([]float64, n)
		ab := make([]float64, n)
		be := make([]float64, n)
		for i, r := range rows {
			close[i], a[i], b[i], e[i], ab[i], be[i] = r.EthClose, r.A, r.B, r.E, r.AB, r.BE
		}
		macd, sig, hist := MacdTalib(close, 12, 26, 9)
		f.Names = []string{"eth_close", "a", "b", "e", "a-b", "b-e", "macd", "signal", "hist"}
		f.TargetCol = 0
		f.Matrix = make([][]float64, 0, n)
		for i := range rows {
			row := []float64{close[i], a[i], b[i], e[i], ab[i], be[i], macd[i], sig[i], hist[i]}
			if hasNaN(row) {
				continue // 对齐 Python 的 df.dropna()
			}
			f.Matrix = append(f.Matrix, row)
			f.Times = append(f.Times, rows[i].Time)
			f.Symbols = append(f.Symbols, rows[i].Symbol)
		}

	default: // FormatRatio
		f.Names = []string{"c1", "c2", "spread", "ratio", "count"}
		f.TargetCol = 1 // 预测下一条的 c2
		f.Matrix = make([][]float64, 0, n)
		for _, r := range rows {
			ratio := 0.0
			if r.C2 != 0 {
				ratio = r.C1 / r.C2
			}
			f.Matrix = append(f.Matrix, []float64{r.C1, r.C2, r.C1 - r.C2, ratio, r.Count})
			f.Times = append(f.Times, r.Time)
			f.Symbols = append(f.Symbols, r.Symbol)
		}
	}

	// Target[i] = Matrix[i+1][TargetCol]，最后一条没有目标
	m := len(f.Matrix)
	f.Target = make([]float64, m)
	for i := 0; i < m; i++ {
		if i+1 < m {
			f.Target[i] = f.Matrix[i+1][f.TargetCol]
		} else {
			f.Target[i] = math.NaN()
		}
	}
	return f
}

func hasNaN(v []float64) bool {
	for _, x := range v {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 相关系数
// ---------------------------------------------------------------------------

// Corr 皮尔逊相关系数，忽略 NaN / Inf。样本不足返回 NaN。
func Corr(x, y []float64) float64 {
	n := len(x)
	if len(y) < n {
		n = len(y)
	}
	xs, ys := make([]float64, 0, n), make([]float64, 0, n)
	for i := 0; i < n; i++ {
		if math.IsNaN(x[i]) || math.IsNaN(y[i]) || math.IsInf(x[i], 0) || math.IsInf(y[i], 0) {
			continue
		}
		xs = append(xs, x[i])
		ys = append(ys, y[i])
	}
	if len(xs) < 2 {
		return math.NaN()
	}
	var sx, sy float64
	for i := range xs {
		sx += xs[i]
		sy += ys[i]
	}
	mx, my := sx/float64(len(xs)), sy/float64(len(ys))
	var num, dx, dy float64
	for i := range xs {
		a, b := xs[i]-mx, ys[i]-my
		num += a * b
		dx += a * a
		dy += b * b
	}
	if dx == 0 || dy == 0 {
		return math.NaN()
	}
	return num / math.Sqrt(dx*dy)
}

// AttnCorrelations 算「各特征列 vs 目标列」的相关系数并打印（对应原脚本那 6 行打印）
func AttnCorrelations(f *AttnFeatures) map[string]float64 {
	res := map[string]float64{}
	if f == nil || len(f.Matrix) == 0 {
		return res
	}
	names := make([]string, 0, len(f.Names))
	for i, nm := range f.Names {
		if i == f.TargetCol {
			continue
		}
		col := make([]float64, len(f.Matrix))
		for r := range f.Matrix {
			col[r] = f.Matrix[r][i]
		}
		res[nm] = Corr(col, f.Target)
		names = append(names, nm)
	}
	sort.Strings(names)
	for _, nm := range names {
		fmt.Printf("Correlation between target(%s) and %s: %.6f\n", f.Names[f.TargetCol], nm, res[nm])
	}
	return res
}

// ---------------------------------------------------------------------------
// CSV / 读取
// ---------------------------------------------------------------------------

// WriteAttnCSV 落 ethdata.csv
func WriteAttnCSV(path string, f *AttnFeatures) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && filepath.Dir(path) != "." {
		return err
	}
	fh, err := os.Create(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	w := csv.NewWriter(fh)
	defer w.Flush()

	header := append([]string{"time", "symbol"}, f.Names...)
	header = append(header, "target")
	if err := w.Write(header); err != nil {
		return err
	}
	num := func(v float64) string {
		if math.IsNaN(v) {
			return ""
		}
		return strconv.FormatFloat(v, 'f', 8, 64)
	}
	for i, row := range f.Matrix {
		rec := []string{
			f.Times[i].Format("2006-01-02 15:04:05"),
			f.Symbols[i],
		}
		for _, v := range row {
			rec = append(rec, num(v))
		}
		rec = append(rec, num(f.Target[i]))
		if err := w.Write(rec); err != nil {
			return err
		}
	}
	return nil
}

// ReadData 读日志文件 → 特征表（对应原脚本的 read_data(file_path)）
func ReadData(filePath string) (*AttnFeatures, error) {
	b, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	rows := ParseBuyLog(string(b))
	if len(rows) == 0 {
		return nil, fmt.Errorf("日志里没解析出任何有效记录：%s", filePath)
	}
	return BuildAttnFeatures(rows), nil
}
