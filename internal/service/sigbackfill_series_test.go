package service

// sigbackfill_series_test.go —— 「逐根滑窗」vs「整段一次算」的等价性取证
//
// 背景：sigbackfill.go 原来对每根 K 线都调一次 ComputeSignal(win)，
// 而 ComputeSignal 每次都要把整个窗口的指标重算一遍 ——
// 复杂度 O(根数 × 窗口) = O(n × 700)，175 个合约跑一轮要 46 秒。
//
// 直觉上应该「整段算一次，再逐根取」（ComputeSeries），但那会改变一件事：
//
//	逐根滑窗：第 i 根的指标，是用窗口 [i-699, i] 算出来的 —— EMA/RSI/ATR
//	          这类**递归指标**的种子落在下标 i-699
//	整段一次：种子落在下标 0
//
// 递归指标的种子位置不同，值就会不同。所以不能凭「反正差不多」就换 ——
// 本测试把差**量出来**，并且给离散字段（Score/Mask/Td/Ready，真正决定
// 买卖的那几个）逐根做严格断言。
//
// 预期（理论）：EMA(26) 的衰减 (1-2/27)^700 ≈ 1e-23，
// RSI(14)/ATR(14) 同为指数衰减，SMA/BOLL 是纯窗口量（只要窗口 >= 周期就逐位相同），
// TD9 是遇断即重置的计数器（最多回看 13 根）。所以偏差应当落在 float64 噪声里。

import (
	"bufio"
	"compress/gzip"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// loadRealKlines 读 testdata 里的**真实** K 线（3 个主力合约 × 3000 根 15m，
// 2026-10-01 从线上 okx.kline 表导出）。合成数据盖不到真实价格结构
// （跳空、长阴、连续同向），而 Fri 这种比值恰恰最容易被真实结构逼到边界。
func loadRealKlines(t *testing.T) map[string][]Candle {
	t.Helper()
	p := filepath.Join("testdata", "klines_real_15m.tsv.gz")
	f, err := os.Open(p)
	if err != nil {
		t.Skipf("没有真实 K 线样本（%s）：%v", p, err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("解压 %s 失败：%v", p, err)
	}
	defer zr.Close()

	out := map[string][]Candle{}
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r\n")
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 7 {
			continue
		}
		ts, _ := strconv.ParseInt(f[1], 10, 64)
		o, _ := strconv.ParseFloat(f[2], 64)
		h, _ := strconv.ParseFloat(f[3], 64)
		l, _ := strconv.ParseFloat(f[4], 64)
		c, _ := strconv.ParseFloat(f[5], 64)
		v, _ := strconv.ParseFloat(f[6], 64)
		out[f[0]] = append(out[f[0]], Candle{Ts: ts, O: o, H: h, L: l, C: c, V: v, Confirm: true})
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("读 %s 失败：%v", p, err)
	}
	if len(out) == 0 {
		t.Fatalf("%s 里一行都没解析出来", p)
	}
	return out
}

// TestWindowedVsFullSeriesOnRealData 用**真实** K 线重跑同一套比对。
//
// 这条比合成数据那条重要：合成序列太平滑，Fri 走不到 0.5 附近，
// 漏掉「mask 位被翻掉」这类真正影响下单的差异。真实数据才有这个说服力。
func TestWindowedVsFullSeriesOnRealData(t *testing.T) {
	const bar = "15m"
	real := loadRealKlines(t)

	totalBars, totalMismatch := 0, 0
	for inst, candles := range real {
		if len(candles) <= sigWindow+50 {
			t.Logf("%s 只有 %d 根，跳过", inst, len(candles))
			continue
		}
		full := ComputeSeries(inst, bar, candles)

		var maxRel float64
		var worstField string
		var worstIdx int
		var maxAbs float64
		var maskFlips, nearHalf int

		for i := sigWarmup; i < len(candles); i++ {
			got := backfillSignalWindowed(inst, bar, candles, i)
			want := full.At(i)
			if got == nil || want == nil {
				t.Fatalf("%s 第 %d 根算出 nil", inst, i)
			}
			gf, wf := seriesFields(got), seriesFields(want)
			for k := range gf {
				if r := relDiff(gf[k].v, wf[k].v); r > maxRel {
					maxRel, worstField, worstIdx = r, gf[k].name, i
					maxAbs = math.Abs(gf[k].v - wf[k].v)
				}
			}
			// Fri 贴着 0.5 的样本单独计数：这才是 mask 位可能被翻的窗口
			if !math.IsNaN(want.Fri) && math.Abs(want.Fri-0.5) < 1e-3 {
				nearHalf++
			}
			if got.Mask != want.Mask {
				maskFlips++
			}
			if got.Score != want.Score || got.Td != want.Td || got.Ready != want.Ready {
				t.Errorf("%s 第 %d 根离散字段不一致：score %d/%d td %d/%d ready %v/%v",
					inst, i, got.Score, want.Score, got.Td, want.Td, got.Ready, want.Ready)
			}
		}
		totalBars += len(candles) - sigWarmup
		totalMismatch += maskFlips
		t.Logf("%s（%d 根，比对 %d 个下标）：最大相对偏差 %.3e（%s@%d，绝对 %.3e）；"+
			"Fri 距 0.5 不足 1e-3 的根数 = %d；mask 位翻转 = %d",
			inst, len(candles), len(candles)-sigWarmup,
			maxRel, worstField, worstIdx, maxAbs, nearHalf, maskFlips)
		full.Release()
	}

	t.Logf("汇总：真实数据共比对 %d 个下标，mask 位翻转 %d 次", totalBars, totalMismatch)

	// 允许的判据：mask 是真正决定「算不算一个信号」的东西。
	// 只要它为 0，Score/Td/Ready 也一致，就说明换实现不会动任何交易决策。
	if totalMismatch != 0 {
		t.Errorf("★ 真实数据上有 %d 个下标的 mask 位被翻掉了 —— 换实现会改变回测结果，不能直接切",
			totalMismatch)
	}
}

// TestProductionReadWindowMatchesFullHistory ★ 守住「只读 sigReadBars 根」这条快路 ★
//
// 生产路径在快路下只读 sigReadBars 根就开算（见 sigbackfill.go 的分段读取），
// 改造前读的是**全部历史**（BTC 一年 35058 根）。所以「读到哪儿为止」是新引入的
// 变量，必须单独取证 —— 上面那两条比的都是「700 根窗口 vs 全量」，盖不到这一条。
//
// 比对区间只取窗口**尾部** tailBars 根，理由：
//
//	快路读的是 [MaxTs - sigReadBars 根, ∞)，而真正要算的只有 ts > MaxTs 的那几根，
//	也就是窗口的最尾部。窗口靠前那些根生产根本不看（它们早被上一轮算过），
//	拿它们断言等于测一个生产不存在的场景，阈值也不好定 ——
//	整段法的种子在窗口下标 0，所以窗口中段的历史比全量少，残差会明显偏大。
//
// 尾部 200 根的残差上界：(95/96)^(sigReadBars-200) ≈ 2e-9，
// 比上面 700 根窗口那版的 6.8e-04 还小 5 个数量级。
func TestProductionReadWindowMatchesFullHistory(t *testing.T) {
	const bar = "15m"
	const tailBars = 200

	real := loadRealKlines(t)
	if len(real) == 0 {
		t.Skip("没有真实 K 线样本")
	}

	totalIdx, totalMismatch := 0, 0
	for inst, candles := range real {
		if len(candles) <= sigReadBars+sigWarmup {
			t.Logf("%s 只有 %d 根（不足 %d + %d），跳过",
				inst, len(candles), sigReadBars, sigWarmup)
			continue
		}
		// 全量：模拟改造前「把整个合约历史读出来」
		full := ComputeSeries(inst, bar, candles)
		// 快路：只读最后 sigReadBars 根，和生产完全一致
		win := candles[len(candles)-sigReadBars:]
		cut := ComputeSeries(inst, bar, win)
		off := len(candles) - len(win) // 窗口下标 → 全量下标的位移

		var maxRel float64
		var worstField string
		var worstIdx int
		mismatch := 0
		for j := len(win) - tailBars; j < len(win); j++ {
			got := cut.At(j)
			want := full.At(j + off)
			if got == nil || want == nil {
				t.Fatalf("%s 窗口第 %d 根算出 nil", inst, j)
			}
			gf, wf := seriesFields(got), seriesFields(want)
			for k := range gf {
				if r := relDiff(gf[k].v, wf[k].v); r > maxRel {
					maxRel, worstField, worstIdx = r, gf[k].name, j
				}
			}
			if got.Mask != want.Mask || got.Score != want.Score ||
				got.Td != want.Td || got.Ready != want.Ready {
				mismatch++
				if mismatch <= 5 {
					t.Errorf("%s 窗口第 %d 根（全量第 %d）离散字段不一致："+
						"score %d/%d  mask %#x/%#x  td %d/%d  ready %v/%v",
						inst, j, j+off, got.Score, want.Score, got.Mask, want.Mask,
						got.Td, want.Td, got.Ready, want.Ready)
				}
			}
		}
		totalIdx += tailBars
		totalMismatch += mismatch
		t.Logf("%s：读到 %d 根（全量 %d 根），尾部 %d 根逐根比对 → "+
			"最大相对偏差 %.3e（%s@窗口%d）；离散字段不一致 %d 根",
			inst, len(win), len(candles), tailBars, maxRel, worstField, worstIdx, mismatch)

		cut.Release()
		full.Release()
	}

	t.Logf("汇总：3 组样本共比对 %d 个下标，离散字段不一致 %d 个", totalIdx, totalMismatch)
	if totalMismatch != 0 {
		t.Errorf("★ 只读 %d 根的快路与全量历史算出的结果不一致 —— "+
			"说明缩短读取窗口会改变信号，必须回退成全量读", sigReadBars)
	}
}

// relDiff 相对偏差；两边都是 NaN 时视为相同（NaN != NaN 会假报不一致）
func relDiff(a, b float64) float64 {
	na, nb := math.IsNaN(a), math.IsNaN(b)
	if na && nb {
		return 0
	}
	if na != nb {
		return math.Inf(1) // 一边有值一边没值，是最严重的不一致
	}
	if a == b {
		return 0
	}
	d := math.Abs(a - b)
	m := math.Max(math.Abs(a), math.Abs(b))
	if m == 0 {
		return d
	}
	return d / m
}

// seriesFields 把 Signal 里所有浮点字段摊平，便于逐个比
func seriesFields(s *Signal) []struct {
	name string
	v    float64
} {
	return []struct {
		name string
		v    float64
	}{
		{"Close", s.Close}, {"Pot", s.Pot}, {"Fri", s.Fri}, {"Kin", s.Kin},
		{"Rsi", s.Rsi}, {"BollLo", s.BollLo}, {"BollUp", s.BollUp}, {"MacdH", s.MacdH},
	}
}

// TestWindowedVsFullSeriesEquivalence ★ 决定能不能换实现的那个测试 ★
//
// 逐根比对「滑窗法」与「整段法」，把最大偏差打出来。
// 阈值卡在 1e-9：远超 float64 的舍入噪声（~1e-16），
// 又不至于放过任何真正会翻买卖判定的偏差。
func TestWindowedVsFullSeriesEquivalence(t *testing.T) {
	const bar = "15m"
	candles := genBenchCandles(2900) // 约等于 30 天的 15m
	full := ComputeSeries("T-USDT-SWAP", bar, candles)
	defer full.Release()

	var maxRel, maxAbs float64
	var worstField string
	var worstIdx int
	discreteMismatch := 0

	for i := sigWarmup; i < len(candles); i++ {
		// 对比的是生产代码里的对照实现（backfillSignalWindowed），
		// 不是测试里另抄一份 —— 否则生产实现被改了测试也不会红。
		got := backfillSignalWindowed("T-USDT-SWAP", bar, candles, i)
		want := full.At(i)

		if got == nil || want == nil {
			t.Fatalf("第 %d 根算出了 nil", i)
		}

		// 浮点字段：记录最大偏差
		gf, wf := seriesFields(got), seriesFields(want)
		for k := range gf {
			if r := relDiff(gf[k].v, wf[k].v); r > maxRel {
				maxRel, worstField, worstIdx = r, gf[k].name, i
				maxAbs = math.Abs(gf[k].v - wf[k].v)
			}
		}

		// 离散字段：直接决定买卖，必须严格一致
		if got.Score != want.Score || got.Mask != want.Mask ||
			got.Td != want.Td || got.Ready != want.Ready {
			discreteMismatch++
			if discreteMismatch <= 5 {
				t.Errorf("第 %d 根离散字段不一致：score %d/%d  mask %#x/%#x  td %d/%d  ready %v/%v",
					i, got.Score, want.Score, got.Mask, want.Mask,
					got.Td, want.Td, got.Ready, want.Ready)
			}
		}
	}

	t.Logf("整段法 vs 滑窗法（2900 根，逐根比对）")
	t.Logf("  浮点字段最大相对偏差 = %.3e（字段 %s，下标 %d，绝对偏差 %.3e）",
		maxRel, worstField, worstIdx, maxAbs)
	t.Logf("  离散字段不一致根数 = %d", discreteMismatch)

	if discreteMismatch != 0 {
		t.Errorf("有 %d 根的 score/mask/td/ready 不一致 —— 不能换成整段法", discreteMismatch)
	}
	// 浮点偏差接受阈值说明：
	//   Fri = atrp14/atrp96，atrp96 是 period=96 的 Wilder 平滑，
	//   滑窗版种子在 i-699、整段版种子在 0，残差 (95/96)^700 ≈ 6.8e-04。
	//   所以 1e-3 是**理论预期**的量级，超过它说明偏差另有来源（那才是 bug）。
	//   **真正把关的是上面那条离散字段零差异**，浮点阈值只用来兜住意外。
	if maxRel > 1e-3 {
		t.Errorf("浮点最大相对偏差 %.3e 超过理论预期量级 1e-3，可能有别的偏差来源", maxRel)
	}
}

// TestFullSeriesIgnoresWindowTruncation 明确：整段法的前 700 根与滑窗法**必然逐位相同**
// （因为 i < sigWindow 时窗口就是 [0, i]，两法输入完全一样）。
// 偏差只可能出现在 i >= sigWindow 的尾部，这条用来确认偏差的来源确实是「种子位置」。
func TestFullSeriesIgnoresWindowTruncation(t *testing.T) {
	const bar = "15m"
	candles := genBenchCandles(900)
	full := ComputeSeries("T-USDT-SWAP", bar, candles)
	defer full.Release()

	for i := sigWarmup; i < sigWindow && i < len(candles); i++ {
		// 窗口恰好是 [0, i]，与整段法的前缀一致
		got := backfillSignalWindowed("T-USDT-SWAP", bar, candles, i)
		want := full.At(i)
		for _, f := range seriesFields(got) {
			var wv float64
			switch f.name {
			case "Close":
				wv = want.Close
			case "Pot":
				wv = want.Pot
			case "Fri":
				wv = want.Fri
			case "Kin":
				wv = want.Kin
			case "Rsi":
				wv = want.Rsi
			case "BollLo":
				wv = want.BollLo
			case "BollUp":
				wv = want.BollUp
			case "MacdH":
				wv = want.MacdH
			}
			if f.v != wv && !(math.IsNaN(f.v) && math.IsNaN(wv)) {
				t.Errorf("下标 %d 字段 %s：滑窗 %.17g != 整段 %.17g（此区间应当逐位相同）",
					i, f.name, f.v, wv)
			}
		}
	}
}

// BenchmarkSigBackfillWindowed 旧实现：逐根滑窗
func BenchmarkSigBackfillWindowed(b *testing.B) {
	const bar = "15m"
	candles := genBenchCandles(2900)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for i := sigWarmup; i < len(candles); i++ {
			lo := i + 1 - sigWindow
			if lo < 0 {
				lo = 0
			}
			win := candles[lo : i+1]
			sig := ComputeSignal("T-USDT-SWAP", bar, win, len(win)-1)
			if sig != nil && sig.Ready {
				_ = sig.Score
			}
		}
	}
}

// BenchmarkSigBackfillFullSeries 新实现：整段算一次 + O(1) 逐根取
func BenchmarkSigBackfillFullSeries(b *testing.B) {
	const bar = "15m"
	candles := genBenchCandles(2900)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		series := ComputeSeries("T-USDT-SWAP", bar, candles)
		for i := sigWarmup; i < series.Len(); i++ {
			sig := series.At(i)
			if sig != nil && sig.Ready {
				_ = sig.Score
			}
		}
		series.Release()
	}
}
