package service

// ops.go —— 原 py 那 5 个入口脚本的 Go 版支撑件。
//
// 原 Python 的调用链是：
//
//	run.go(cron) → exec.Command("python", "cash.py" / "sells.py" / "gorun.py" ...)
//	                └─ 脚本里 from userinfo import User / from mvc import MVC
//
// 现在整条链子都留在同一个进程里：
//
//	run.go(cron) → Getcashbal() / Buy() / ... → DefaultOps() → OKX REST
//
// 关键点：datas/ 目录在当前项目（finally）的上一层，这是 Python 版里
// 满屏的 "../datas/..." 造成的，这里用 ProjectRoot/DataRoot 把它算清楚，
// 不管 cron 从哪个工作目录拉起来都能找对地方。

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

var (
	opsOnce sync.Once
	opsInst *AccountOps
	opsErr  error
)

// ProjectRoot 从当前目录往上找 go.mod，找到就是项目根（.../finally）。
func ProjectRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	start := dir
	for i := 0; i < 6; i++ {
		if _, e := os.Stat(filepath.Join(dir, "go.mod")); e == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return start
}

// DataRoot 数据根目录 —— datas/ 的父目录，也就是项目根的上一层。
// 对应 Python 里的 "../datas/api.json"、"../datas/log/buylog_*.txt"。
func DataRoot() string { return filepath.Dir(ProjectRoot()) }

// DefaultOps 全局账户操作器（懒加载 + sync.Once，cron 并发调也安全）。
func DefaultOps() (*AccountOps, error) {
	opsOnce.Do(func() {
		opsInst, opsErr = NewAccountOps(DataRoot(), nil)
	})
	return opsInst, opsErr
}

// normalizeMinute 照抄 gorun.py：1m→1、3m→3、5m→5、15m→15；
// low / imr 这两个补仓专用档位原样透传。
func normalizeMinute(minute string) string {
	switch minute {
	case "1m":
		return "1"
	case "3m":
		return "3"
	case "5m":
		return "5"
	case "15m":
		return "15"
	}
	return minute
}

// ---------------------------------------------------------------------------
// Savecsv —— 把 buylog_<minute>.txt 的两行式记录汇总成 CSV
// ---------------------------------------------------------------------------

// BuyLogRecord 一条买点记录。真实日志格式（两行一条）：
//
//	2025-10-21 01:20:21,symbol--->>SPX-USDT-SWAP
//	,c1--->>0.99862,c2--->>1.00437,count--->>29,minute--->>5m
type BuyLogRecord struct {
	Time   string
	Symbol string
	C1     float64
	C2     float64
	Spread float64 // c1 - c2
	Ratio  float64 // c1 / c2
	Count  int
	Minute string
}

// LogPath 拼出 datas/log/buylog_<minute>.txt 的绝对路径。
func LogPath(minute string) string {
	return filepath.Join(DataRoot(), "datas", "log", "buylog_"+minute+".txt")
}

// ParseBuyLogRecords 解析两行式 buylog（兼容老式单行 a/b/e 格式）。
// 解析逻辑与 okx/attn_encode.go 保持一致，避免两处口径漂移。
func ParseBuyLogRecords(content string) []BuyLogRecord {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	var out []BuyLogRecord
	var cur *BuyLogRecord
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		low := strings.ToLower(line)
		switch {
		case strings.Contains(low, "symbol--->>"):
			// 新记录开始，先把上一条收掉
			if cur != nil {
				out = append(out, *cur)
			}
			cur = &BuyLogRecord{}
			if i := strings.Index(line, ","); i > 0 {
				cur.Time = strings.TrimSpace(line[:i])
			}
			cur.Symbol = pickValue(line, "symbol")
		case strings.Contains(low, "c1--->>"):
			if cur == nil {
				cur = &BuyLogRecord{}
			}
			cur.C1 = parseNum(pickValue(line, "c1"))
			cur.C2 = parseNum(pickValue(line, "c2"))
			cur.Count = int(parseNum(pickValue(line, "count")))
			cur.Minute = pickValue(line, "minute")
		default:
			// 老式单行：a/b/e/a-b/b-e/eth_close
			parts := strings.Split(line, "/")
			if len(parts) >= 6 {
				r := BuyLogRecord{
					Time:   strings.TrimSpace(parts[0]),
					Symbol: strings.TrimSpace(parts[1]),
					C1:     parseNum(parts[2]),
					C2:     parseNum(parts[3]),
				}
				out = append(out, r)
			}
		}
	}
	if cur != nil {
		out = append(out, *cur)
	}
	for i := range out {
		out[i].Spread = out[i].C1 - out[i].C2
		if out[i].C2 != 0 {
			out[i].Ratio = out[i].C1 / out[i].C2
		}
	}
	return out
}

// ExportBuyLogCSV 把某个周期的买点日志导出成 CSV，返回落地路径。
func ExportBuyLogCSV(minute string) (string, error) {
	src := LogPath(minute)
	raw, err := os.ReadFile(src)
	if err != nil {
		return "", fmt.Errorf("读不到 %s：%w", src, err)
	}
	recs := ParseBuyLogRecords(string(raw))
	if len(recs) == 0 {
		return "", fmt.Errorf("%s 里没解析出记录", src)
	}
	outDir := filepath.Join(DataRoot(), "datas", "csv")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(outDir, "buylog_"+minute+".csv")
	var sb strings.Builder
	sb.WriteString("time,symbol,c1,c2,spread,ratio,count,minute\n")
	for _, r := range recs {
		fmt.Fprintf(&sb, "%s,%s,%.5f,%.5f,%.6f,%.6f,%d,%s\n",
			r.Time, r.Symbol, r.C1, r.C2, r.Spread, r.Ratio, r.Count, r.Minute)
	}
	if err := os.WriteFile(dst, []byte(sb.String()), 0o644); err != nil {
		return "", err
	}
	return dst, nil
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// pickValue 从 "...,c1--->>0.99862,..." 这种行里抠出 c1 后面的值。
func pickValue(line, key string) string {
	idx := strings.Index(strings.ToLower(line), strings.ToLower(key)+"--->>")
	if idx < 0 {
		return ""
	}
	rest := line[idx+len(key)+5:]
	if j := strings.IndexByte(rest, ','); j >= 0 {
		rest = rest[:j]
	}
	return strings.TrimSpace(rest)
}

func parseNum(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// SummarizeBuyLog 汇总某个周期日志的统计信息（网页 / 命令行都用得上）。
type BuyLogSummary struct {
	Minute    string
	Records   int
	Symbols   int
	FirstTime string
	LastTime  string
	TopSymbol []string
}

// SummarizeBuyLogFile 读文件并出统计。
func SummarizeBuyLogFile(minute string) (*BuyLogSummary, error) {
	raw, err := os.ReadFile(LogPath(minute))
	if err != nil {
		return nil, err
	}
	recs := ParseBuyLogRecords(string(raw))
	s := &BuyLogSummary{Minute: minute, Records: len(recs)}
	cnt := map[string]int{}
	for _, r := range recs {
		cnt[r.Symbol]++
		if s.FirstTime == "" {
			s.FirstTime = r.Time
		}
		s.LastTime = r.Time
	}
	s.Symbols = len(cnt)
	type kv struct {
		k string
		n int
	}
	var arr []kv
	for k, n := range cnt {
		arr = append(arr, kv{k, n})
	}
	sort.Slice(arr, func(i, j int) bool {
		if arr[i].n != arr[j].n {
			return arr[i].n > arr[j].n
		}
		return arr[i].k < arr[j].k
	})
	for i, e := range arr {
		if i >= 10 {
			break
		}
		s.TopSymbol = append(s.TopSymbol, fmt.Sprintf("%s×%d", e.k, e.n))
	}
	return s, nil
}
