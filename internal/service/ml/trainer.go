package ml

// trainer.go —— 原 okx/Trainer.py 的 Go 版（按真实日志格式重写）
//
// Python 版是个 PyQt5 窗口：parse_txt_content 打标签 + matplotlib 画图 + QTimer 刷新。
// Go 版把「训练 + 报告」抽成纯函数，GUI 交给网页客户端（cmd/okxweb），
// 命令行用 cmd/okxtool train 触发。这样：
//   - 不再依赖 PyQt5 / matplotlib / numpy / talib
//   - 训练结果既落 CSV 也打控制台，网页可以直接读 CSV 画图
//
// 打标签口径（按日志格式分两种）：
//   · 真实格式（datas/log/buylog_*.txt）：
//       日志本身记录的就是买点，标签定义为「下一条记录的 c2 比当前这条高」→ 1
//       （c2 = 最新一根 K 线收/开，代表信号出现后市场是否继续走强）
//   · 旧格式（AttnEncoder.py 期待的那套 a/b/e/macd）：
//       0.1<a<0.2 且 0.1<b<0.2 且 e<0.1 且 0<a-b<0.1 且 0<b-e<0.1 且 1<macd<2 → 1
//       （原样照抄 Trainer.py，万一以后真有这种日志也能用）

import (
	"encoding/csv"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// TrainerFeatures 特征表 + 标签
type TrainerFeatures struct {
	*AttnFeatures
	Label []int
}

// BuildTrainerFeatures 对应 Trainer.py 的 parse_txt_content（带标签）
func BuildTrainerFeatures(content string) *TrainerFeatures {
	rows := ParseBuyLog(content)
	base := BuildAttnFeatures(rows)
	tf := &TrainerFeatures{AttnFeatures: base}
	m := len(base.Matrix)
	tf.Label = make([]int, m)

	switch base.Format {
	case FormatLegacy:
		// 逐列找下标：a / b / e / a-b / b-e / macd
		idx := map[string]int{}
		for i, nm := range base.Names {
			idx[nm] = i
		}
		ia, ib, ie := idx["a"], idx["b"], idx["e"]
		iab, ibe, imacd := idx["a-b"], idx["b-e"], idx["macd"]
		for i, row := range base.Matrix {
			if i == 0 {
				continue
			}
			a, b, e := row[ia], row[ib], row[ie]
			ab, be, macd := row[iab], row[ibe], row[imacd]
			if a > 0.1 && a < 0.2 &&
				b > 0.1 && b < 0.2 &&
				e < 0.1 &&
				ab > 0 && ab < 0.1 &&
				be > 0 && be < 0.1 &&
				macd > 1 && macd < 2 {
				tf.Label[i] = 1
			}
		}

	default: // FormatRatio：下一条的 c2 是否走高
		ic2 := base.TargetCol
		for i := 0; i+1 < m; i++ {
			if base.Matrix[i+1][ic2] > base.Matrix[i][ic2] {
				tf.Label[i] = 1
			}
		}
	}
	return tf
}

// FeatureNames 当前格式的列名
func (t *TrainerFeatures) FeatureNames() []string { return t.AttnFeatures.Names }

// WriteTrainerCSV 落 ethdata.csv
func WriteTrainerCSV(path string, f *TrainerFeatures) error {
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
	header = append(header, "target", "buy_label")
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
		rec := []string{f.Times[i].Format("2006-01-02 15:04:05"), f.Symbols[i]}
		for _, v := range row {
			rec = append(rec, num(v))
		}
		rec = append(rec, num(f.Target[i]), strconv.Itoa(f.Label[i]))
		if err := w.Write(rec); err != nil {
			return err
		}
	}
	return nil
}

// TrainerRunOptions 训练参数
type TrainerRunOptions struct {
	LogPath    string // 输入日志
	CSVPath    string // 输出 CSV（留空不写）
	ModelPath  string // 输出权重 json（留空不存）
	ReportPath string // 输出文本报告（留空不写）
	Epochs     int
	Layers     int
	Lookback   int
	Hidden     int
	BatchSize  int
	LR         float64
	Quiet      bool
}

// DefaultTrainerOptions 默认值
func DefaultTrainerOptions() TrainerRunOptions {
	return TrainerRunOptions{
		Epochs: 30, Layers: 2, Lookback: 10, Hidden: 32, BatchSize: 16, LR: 0.01,
	}
}

// TrainerResult 训练产出（给命令行打印 / 给网页读）
type TrainerResult struct {
	LogPath      string
	Format       string
	Records      int
	Features     int
	Samples      int
	BuyLabels    int
	TestLoss     []float64
	Correlations map[string]float64
	CSVPath      string
	ModelPath    string
}

// RunTrainer 一把梭：读日志 → 组特征 → 落 CSV → 训练 → 存权重。
func RunTrainer(opt TrainerRunOptions, logf func(string, ...any)) (*TrainerResult, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if opt.Lookback <= 0 {
		opt.Lookback = 10
	}
	if opt.Hidden <= 0 {
		opt.Hidden = 32
	}
	if opt.Layers <= 0 {
		opt.Layers = 2
	}
	if opt.Epochs <= 0 {
		opt.Epochs = 30
	}
	if opt.LR <= 0 {
		opt.LR = 0.01
	}

	raw, err := os.ReadFile(opt.LogPath)
	if err != nil {
		return nil, fmt.Errorf("读不到日志 %s：%w", opt.LogPath, err)
	}
	feat := BuildTrainerFeatures(string(raw))
	res := &TrainerResult{
		LogPath:   opt.LogPath,
		Format:    feat.Format.String(),
		Records:   len(feat.Rows),
		Features:  len(feat.Names),
		Samples:   len(feat.Matrix),
		BuyLabels: countOnes(feat.Label),
	}
	if res.Samples == 0 {
		return res, fmt.Errorf("%s 没解析出可用样本（格式=%s，原始记录 %d 条）", opt.LogPath, res.Format, res.Records)
	}
	logf("日志 %s：%s 格式，%d 条记录 → %d 个样本 × %d 列（正样本 %d）",
		filepath.Base(opt.LogPath), res.Format, res.Records, res.Samples, res.Features, res.BuyLabels)

	res.Correlations = AttnCorrelations(feat.AttnFeatures)

	if opt.CSVPath != "" {
		if err := WriteTrainerCSV(opt.CSVPath, feat); err != nil {
			logf("写 CSV 失败（继续训练）：%v", err)
		} else {
			res.CSVPath = opt.CSVPath
			logf("特征已落盘：%s", opt.CSVPath)
		}
	}
	if opt.ReportPath != "" {
		if err := WriteTrainerReport(opt.ReportPath, res, feat); err != nil {
			logf("写报告失败：%v", err)
		}
	}

	cfg := DefaultRNNConfig()
	cfg.Lookback, cfg.Hidden, cfg.Layers = opt.Lookback, opt.Hidden, opt.Layers
	cfg.Epochs, cfg.BatchSize, cfg.LR = opt.Epochs, opt.BatchSize, opt.LR
	cfg.TargetCol = feat.TargetCol

	model, report, err := TrainRNN(feat.Matrix, feat.Target, cfg, func(format string, args ...any) {
		if !opt.Quiet {
			logf(format, args...)
		}
	})
	if err != nil {
		return res, err
	}
	res.TestLoss = report.TestLoss
	if opt.ModelPath != "" {
		if err := SaveRNNModel(opt.ModelPath, model); err != nil {
			return res, fmt.Errorf("存权重失败：%w", err)
		}
		res.ModelPath = opt.ModelPath
		logf("权重已保存：%s", opt.ModelPath)
	}
	if n := len(report.TestLoss); n > 0 {
		logf("训练完成：%d 轮，损失 %.6f → %.6f（降幅 %.1f%%）",
			n, report.TestLoss[0], report.TestLoss[n-1],
			(report.TestLoss[0]-report.TestLoss[n-1])/math.Max(report.TestLoss[0], 1e-12)*100)
	}
	return res, nil
}

// TrainerBatchOptions 批量训练
type TrainerBatchOptions struct {
	LogDir string // 日志目录（datas/log）
	OutDir string // 输出目录（datas/train）
	Glob   string // 默认 "buylog_*.txt"
	Base   TrainerRunOptions
}

// RunTrainerBatch 把目录下所有 buylog_*.txt 逐个训练一遍
func RunTrainerBatch(opt TrainerBatchOptions, logf func(string, ...any)) ([]*TrainerResult, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if opt.Glob == "" {
		opt.Glob = "buylog_*.txt"
	}
	matches, err := filepath.Glob(filepath.Join(opt.LogDir, opt.Glob))
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("%s 下没有匹配 %s 的文件", opt.LogDir, opt.Glob)
	}
	sort.Strings(matches)
	if opt.OutDir != "" {
		if err := os.MkdirAll(opt.OutDir, 0o755); err != nil {
			return nil, err
		}
	}

	var out []*TrainerResult
	for _, m := range matches {
		name := strings.TrimSuffix(filepath.Base(m), ".txt")
		logf("────────── %s ──────────", name)
		o := opt.Base
		o.LogPath = m
		o.CSVPath = filepath.Join(opt.OutDir, name+".csv")
		o.ModelPath = filepath.Join(opt.OutDir, name+".json")
		o.ReportPath = filepath.Join(opt.OutDir, name+".report.txt")
		r, err := RunTrainer(o, logf)
		if err != nil {
			logf("跳过 %s：%v", name, err)
			if r != nil {
				out = append(out, r)
			}
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func countOnes(v []int) int {
	n := 0
	for _, x := range v {
		if x == 1 {
			n++
		}
	}
	return n
}

// WriteTrainerReport 落一份文本报告（网页 / 命令行都能看）
func WriteTrainerReport(path string, res *TrainerResult, feat *TrainerFeatures) error {
	var sb strings.Builder
	sb.WriteString("OKX 买点日志训练报告\n")
	sb.WriteString(strings.Repeat("=", 52) + "\n")
	fmt.Fprintf(&sb, "日志文件   : %s\n", res.LogPath)
	fmt.Fprintf(&sb, "日志格式   : %s\n", res.Format)
	fmt.Fprintf(&sb, "原始记录   : %d 条\n", res.Records)
	fmt.Fprintf(&sb, "可用样本   : %d 行 × %d 列\n", res.Samples, res.Features)
	fmt.Fprintf(&sb, "正样本数   : %d（%.1f%%）\n", res.BuyLabels,
		float64(res.BuyLabels)/math.Max(float64(res.Samples), 1)*100)
	fmt.Fprintf(&sb, "特征列     : %s\n", strings.Join(feat.Names, ", "))
	sb.WriteString("\n相关系数（各列 vs 目标）\n" + strings.Repeat("-", 52) + "\n")
	keys := make([]string, 0, len(res.Correlations))
	for k := range res.Correlations {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&sb, "  %-10s %+.6f\n", k, res.Correlations[k])
	}
	if len(res.TestLoss) > 0 {
		sb.WriteString("\n训练损失（测试集）\n" + strings.Repeat("-", 52) + "\n")
		fmt.Fprintf(&sb, "  首轮 %.6f → 末轮 %.6f\n", res.TestLoss[0], res.TestLoss[len(res.TestLoss)-1])
		sb.WriteString("\n")
		sb.WriteString(RenderLossCurve(res.TestLoss, 64))
	}
	sb.WriteString("\n各符号出现次数 Top20\n" + strings.Repeat("-", 52) + "\n")
	cnt := map[string]int{}
	for _, s := range feat.Symbols {
		cnt[s]++
	}
	type kv struct {
		k string
		n int
	}
	var arr []kv
	for k, n := range cnt {
		arr = append(arr, kv{k, n})
	}
	sort.Slice(arr, func(i, j int) bool { return arr[i].n > arr[j].n })
	for i, e := range arr {
		if i >= 20 {
			break
		}
		fmt.Fprintf(&sb, "  %-24s %d\n", e.k, e.n)
	}
	return os.WriteFile(path, []byte(sb.String()), 0o644)
}

// RenderLossCurve 把 loss 序列画成字符图（terminal 里能直接看，替代 matplotlib）
func RenderLossCurve(loss []float64, width int) string {
	if len(loss) == 0 {
		return "(没有 loss 数据)\n"
	}
	if width < 20 {
		width = 60
	}
	rows := 12
	mn, mx := math.Inf(1), math.Inf(-1)
	for _, v := range loss {
		if v < mn {
			mn = v
		}
		if v > mx {
			mx = v
		}
	}
	if mx-mn < 1e-12 {
		mx = mn + 1e-12
	}
	grid := make([][]rune, rows)
	for i := range grid {
		grid[i] = make([]rune, width)
		for j := range grid[i] {
			grid[i][j] = ' '
		}
	}
	for x := 0; x < width; x++ {
		i := int(float64(x) / float64(width-1) * float64(len(loss)-1))
		if i < 0 {
			i = 0
		}
		if i >= len(loss) {
			i = len(loss) - 1
		}
		v := (loss[i] - mn) / (mx - mn)
		y := int((1 - v) * float64(rows-1))
		if y < 0 {
			y = 0
		}
		if y >= rows {
			y = rows - 1
		}
		grid[y][x] = '*'
	}
	var sb strings.Builder
	for _, row := range grid {
		sb.WriteString(string(row))
		sb.WriteString("\n")
	}
	fmt.Fprintf(&sb, "min=%.6f  max=%.6f  points=%d\n", mn, mx, len(loss))
	return sb.String()
}
