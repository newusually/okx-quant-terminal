package service

// indicators.go —— 8 因子共振（对应文案 §5，逐位照抄，不许自创）
//
// 方向：抄底做多。每一项命中就把对应 bit 置 1，最后 score = popcount(mask)（0~8）
//
//	bit0  1    势能   (close - SMA200) / ATRP14  <  -1.0
//	bit1  2    摩擦   ATRP14 / ATRP96            >   0.5
//	bit2  4    动能   0.5 * vr * vel^2 * sign(vel)  <  0.0
//	bit3  8    RSI    RSI14                      <  30.0
//	bit4  16   布林   close                      <   布林下轨(SMA20 - 2σ, 总体方差/20)
//	bit5  32   MACD   hist[i] > hist[i-1] > hist[i-2]，DEA 周期 = 60（不是 9）
//	bit6  64   TD9    close[i] < close[i-4] 连续计数 >= 7
//	bit7  128  放量   volume > 1.5 * SMA(volume,20)

import (
	"math"
	"math/bits"
	"strings"
)

// 因子名称（bit 顺序）
var factorNames = [8]string{"势能", "摩擦", "动能", "RSI", "布林", "MACD", "TD9", "放量"}

// Candle 一根 K 线
type Candle struct {
	Ts      int64
	O       float64
	H       float64
	L       float64
	C       float64
	V       float64
	Confirm bool // OKX 的 confirm 字段：true = 已收盘
}

// Signal 一次共振判定结果
type Signal struct {
	InstID  string
	Bar     string
	Ts      int64
	Close   float64
	Open    float64
	High    float64
	Low     float64
	Vol     float64
	Mask    int
	Score   int
	HitList string // 命中的因子名，逗号分隔

	// RisePct 这一根 K 线自己的涨跌幅（%）= (收 − 开) ÷ 开 × 100。
	//
	// ★ 2026-10-02 三期新增（用户口径：「有信号的那个 K 线必须大于 1% 涨幅才行」；
	//   五期门槛 0.5%；六期反转为「必须真跌 < -0.7%」——门槛值本身没变过语义，
	//   变的是符号承载的方向，见 SignalQualified 的带符号门槛说明）。
	//   放在 Signal 里而不是让调用方自己拿 candles[idx] 算，
	//   是为了让买入扫描与加仓判定读到**同一个数** —— 两处各算一遍是本项目的老坑。
	//   开盘价为 0（脏数据）时记 0：无论门槛是正是负，0 都过不了，偏保守。
	RisePct float64

	Pot     float64
	Fri     float64
	Kin     float64
	Rsi     float64
	Td      int
	BollLo  float64
	BollUp  float64
	MacdH   float64
	Ready   bool // 指标是否算得出来（暖机够不够）
}

// SignalQualified 判断一个信号是否**够格下单**。
//
// ★★ 这是「买入」与「加仓」共用的唯一判据 —— 两处都必须调它 ★★
//
// 用户口径（2026-10-02 六期）：
//
//	「Score >= 3 且 RisePct < -0.7（严格小于）」—— 触发那根 K 线必须真跌超 0.7%
//
// 三个条件同时成立才通过：
//
//	① sig.Ready           指标暖机完整。不 Ready 时 Pot / Rsi / BollLo 是 NaN，
//	                      score 本身没有意义（比如冷启动只拉到几十根 K 线）
//	② sig.Score >= 门槛   调用方传 cfg.ThresholdFor(instID)。判定是**非严格** `>=`，
//	                      所以 threshold = 3 就是用户说的「Score >= 3」（Score 是 0~8 的整数）
//	③ minRisePct 是**带符号门槛**（六期起）：
//	                      > 0 → RisePct 必须严格大于它（「必须真涨」，五期及以前的用法）
//	                      < 0 → RisePct 必须严格小于它（「必须真跌」，六期：-0.7）
//	                      = 0 → 关闭这个条件（只看分数）
//
// 为什么必须做成一个函数：加仓的口径是「与买入条件完全一致」。只要两处
// 各写一遍判定，迟早会在边界上走岔 ——「>= 还是 >」「用 Close 还是 RisePct」
// —— 而且不会报错。一期 closedWindow 喂错 Candle.Confirm 就是这么静默失效的。
//
// 注意 threshold <= 0 时**直接拒绝**：那是配置坏掉的状态（归一化保证它 ≥ 1），
// 与其「放宽到只看 1 个因子」乱开单，不如这一轮不下单。
func SignalQualified(sig *Signal, threshold int, minRisePct float64) bool {
	if sig == nil || !sig.Ready {
		return false
	}
	if threshold <= 0 {
		return false
	}
	if sig.Score < threshold {
		return false
	}
	// 带符号门槛（六期）：正数=必须真涨、负数=必须真跌、0=关闭。
	// 写成 !(a > b) / !(a < b) 而不是反向比较：RisePct 理论上是 NaN 时
	// 所有比较都是 false，前者会把 NaN 判成「不合格」（保守），后者会放行。
	if minRisePct > 0 && !(sig.RisePct > minRisePct) {
		return false
	}
	if minRisePct < 0 && !(sig.RisePct < minRisePct) {
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// 基础指标
// ---------------------------------------------------------------------------

// trueRange tr[0] = h-l；tr[i] = max(h-l, |h-c[i-1]|, |l-c[i-1]|)
func trueRange(h, l, c []float64) []float64 {
	n := len(c)
	tr := make([]float64, n)
	if n == 0 {
		return tr
	}
	tr[0] = h[0] - l[0]
	for i := 1; i < n; i++ {
		v := h[i] - l[i]
		if d := math.Abs(h[i] - c[i-1]); d > v {
			v = d
		}
		if d := math.Abs(l[i] - c[i-1]); d > v {
			v = d
		}
		tr[i] = v
	}
	return tr
}

// atrPercent Wilder 平滑的 ATR%，返回 atr[i]/c[i]*100；i < period 为 NaN
func atrPercent(h, l, c []float64, period int) []float64 {
	n := len(c)
	out := make([]float64, n)
	for i := range out {
		out[i] = math.NaN()
	}
	if period <= 0 || n < period+1 {
		return out
	}
	tr := trueRange(h, l, c)
	sum := 0.0
	for i := 1; i <= period; i++ {
		sum += tr[i]
	}
	atr := sum / float64(period)
	out[period] = atr / c[period] * 100
	for i := period + 1; i < n; i++ {
		atr = (atr*float64(period-1) + tr[i]) / float64(period)
		out[i] = atr / c[i] * 100
	}
	return out
}

// sma 简单滑动平均；i < period-1 为 NaN
func sma(x []float64, period int) []float64 {
	n := len(x)
	out := make([]float64, n)
	for i := range out {
		out[i] = math.NaN()
	}
	if period <= 0 || n < period {
		return out
	}
	sum := 0.0
	for i := 0; i < n; i++ {
		sum += x[i]
		if i >= period {
			sum -= x[i-period]
		}
		if i >= period-1 {
			out[i] = sum / float64(period)
		}
	}
	return out
}

// ema 指数平均；ema[0] = x[0]
func ema(x []float64, period int) []float64 {
	n := len(x)
	out := make([]float64, n)
	if n == 0 || period <= 0 {
		return out
	}
	k := 2.0 / float64(period+1)
	out[0] = x[0]
	for i := 1; i < n; i++ {
		out[i] = x[i]*k + out[i-1]*(1-k)
	}
	return out
}

// rsiWilder RSI；i < period 为 NaN
func rsiWilder(c []float64, period int) []float64 {
	n := len(c)
	out := make([]float64, n)
	for i := range out {
		out[i] = math.NaN()
	}
	if period <= 0 || n < period+1 {
		return out
	}
	ag, al := 0.0, 0.0
	for i := 1; i <= period; i++ {
		d := c[i] - c[i-1]
		if d > 0 {
			ag += d
		} else {
			al += -d
		}
	}
	ag /= float64(period)
	al /= float64(period)
	out[period] = rsiFrom(ag, al)
	for i := period + 1; i < n; i++ {
		d := c[i] - c[i-1]
		g, ls := 0.0, 0.0
		if d > 0 {
			g = d
		} else {
			ls = -d
		}
		ag = (ag*float64(period-1) + g) / float64(period)
		al = (al*float64(period-1) + ls) / float64(period)
		out[i] = rsiFrom(ag, al)
	}
	return out
}

func rsiFrom(ag, al float64) float64 {
	if al == 0 {
		return 100
	}
	return 100 - 100/(1+ag/al)
}

// boll 布林带。方差用「总体方差」（除以 period，不是 period-1）
func boll(c []float64, period int, mult float64) (lower, upper, mid []float64) {
	n := len(c)
	lower = make([]float64, n)
	upper = make([]float64, n)
	mid = make([]float64, n)
	for i := 0; i < n; i++ {
		lower[i], upper[i], mid[i] = math.NaN(), math.NaN(), math.NaN()
	}
	if period <= 0 || n < period {
		return
	}
	for i := period - 1; i < n; i++ {
		s := 0.0
		for k := i - period + 1; k <= i; k++ {
			s += c[k]
		}
		m := s / float64(period)
		v := 0.0
		for k := i - period + 1; k <= i; k++ {
			d := c[k] - m
			v += d * d
		}
		v /= float64(period) // 总体方差
		sd := math.Sqrt(v)
		mid[i] = m
		lower[i] = m - mult*sd
		upper[i] = m + mult*sd
	}
	return
}

// macdHist DIF = EMA(fast) - EMA(slow)；DEA = EMA(DIF, signal)；hist = 2*(DIF-DEA)
func macdHist(c []float64, fast, slow, signal int) []float64 {
	n := len(c)
	out := make([]float64, n)
	if n == 0 {
		return out
	}
	ef := ema(c, fast)
	es := ema(c, slow)
	dif := make([]float64, n)
	for i := 0; i < n; i++ {
		dif[i] = ef[i] - es[i]
	}
	dea := ema(dif, signal)
	for i := 0; i < n; i++ {
		out[i] = 2 * (dif[i] - dea[i])
	}
	return out
}

// tdSetup TD 下跌计数：close[i] < close[i-4] 则累加，否则归零
func tdSetup(c []float64) []int {
	n := len(c)
	out := make([]int, n)
	for i := 4; i < n; i++ {
		if c[i] < c[i-4] {
			out[i] = out[i-1] + 1
		} else {
			out[i] = 0
		}
	}
	return out
}

func isNaN(v float64) bool { return math.IsNaN(v) || math.IsInf(v, 0) }

// ---------------------------------------------------------------------------
// 核心：算一次共振
// ---------------------------------------------------------------------------

// ComputeSignal 判 cands[idx] 这一根的 8 因子。
//
// ★ 实现已迁到 indicator_series.go（2026-10-01 性能改造）★
//
// 为什么搬走：老版本为了判**一根** K 线，把整段 1500 根的指标全部重算一遍，
// 逐根扫完整段就退化成 O(n²)（实测 bars1500 逐根 = 440.6ms / 361MB）。
// 而且每次都 make 出十几个数组，实测 numGC 每秒 7.2 次、live heap 只有 3.5MB。
//
// 新实现拆成「指标算一遍」+「O(1) 取下标」，并用 sync.Pool 复用缓冲；
// 对外语义不变。要对同一段 K 线取多个下标，请用 ComputeSeries。
//
// 本文件里的 sma / ema / rsiWilder / boll / macdHist / tdSetup 保留下来，
// 作为 indicator_series_test.go 等价性测试的**参照实现** —— 两边结果必须比特一致，
// 否则买卖点会漂移。（下面还有一份老的 computeSignalReference 供对照。）

// computeSignalReference 改造前的原实现，逐行保留，只做等价性对照用。
//
// ⚠️ 不要在生产路径调用它 —— 它每次分配 258KB 且是 O(n) per call。
func computeSignalReference(instID, bar string, candles []Candle, idx int) *Signal {
	s := &Signal{InstID: instID, Bar: bar}
	n := len(candles)
	if n == 0 || idx < 0 || idx >= n {
		return s
	}
	cur := candles[idx]
	s.Ts = cur.Ts
	s.Close = cur.C
	s.Open = cur.O
	s.High = cur.H
	s.Low = cur.L
	s.Vol = cur.V
	// 与 indicator_series.go 的 signalAt 保持一致（等价性测试会逐字段比对）
	if cur.O > 0 {
		s.RisePct = (cur.C - cur.O) / cur.O * 100
	}

	h := make([]float64, n)
	l := make([]float64, n)
	c := make([]float64, n)
	v := make([]float64, n)
	for i := 0; i < n; i++ {
		h[i], l[i], c[i], v[i] = candles[i].H, candles[i].L, candles[i].C, candles[i].V
	}

	atrp14 := atrPercent(h, l, c, 14)
	atrp96 := atrPercent(h, l, c, 96)
	sma200 := sma(c, 200)
	sma48v := sma(v, 48)
	sma20v := sma(v, 20)
	rsi14 := rsiWilder(c, 14)
	bollLo, bollUp, _ := boll(c, 20, 2.0)
	hist := macdHist(c, 12, 26, 60)
	td := tdSetup(c)

	i := idx

	// ---- 中间量 ----
	if !isNaN(sma200[i]) && !isNaN(atrp14[i]) && atrp14[i] != 0 {
		s.Pot = (c[i] - sma200[i]) / atrp14[i]
	} else {
		s.Pot = math.NaN()
	}
	if !isNaN(atrp14[i]) && !isNaN(atrp96[i]) && atrp96[i] != 0 {
		s.Fri = atrp14[i] / atrp96[i]
	} else {
		s.Fri = math.NaN()
	}
	if i >= 6 && !isNaN(sma48v[i]) && sma48v[i] != 0 && c[i-6] != 0 {
		vel := (c[i]/c[i-6] - 1) * 100
		vr := v[i] / sma48v[i]
		sign := 0.0
		if vel > 0 {
			sign = 1
		} else if vel < 0 {
			sign = -1
		}
		s.Kin = 0.5 * vr * vel * vel * sign
	} else {
		s.Kin = math.NaN()
	}
	s.Rsi = rsi14[i]
	s.Td = td[i]
	s.BollLo = bollLo[i]
	s.BollUp = bollUp[i]
	s.MacdH = hist[i]

	// ---- 位掩码 ----
	mask := 0
	if !isNaN(s.Pot) && s.Pot < -1.0 {
		mask |= 1 << 0
	}
	if !isNaN(s.Fri) && s.Fri > 0.5 {
		mask |= 1 << 1
	}
	if !isNaN(s.Kin) && s.Kin < 0.0 {
		mask |= 1 << 2
	}
	if !isNaN(s.Rsi) && s.Rsi < 30.0 {
		mask |= 1 << 3
	}
	if !isNaN(s.BollLo) && c[i] < s.BollLo {
		mask |= 1 << 4
	}
	if i >= 2 && hist[i] > hist[i-1] && hist[i-1] > hist[i-2] {
		mask |= 1 << 5
	}
	if i >= 4 && td[i] >= 7 {
		mask |= 1 << 6
	}
	if !isNaN(sma20v[i]) && sma20v[i] != 0 && v[i] > 1.5*sma20v[i] {
		mask |= 1 << 7
	}

	s.Mask = mask
	s.Score = bits.OnesCount(uint(mask))
	s.HitList = hitList(mask)
	// 暖机是否够：势能和布林算得出来就算 Ready
	s.Ready = !isNaN(s.Pot) && !isNaN(s.BollLo) && !isNaN(s.Rsi)
	return s
}

func hitList(mask int) string {
	var parts []string
	for b := 0; b < 8; b++ {
		if mask&(1<<uint(b)) != 0 {
			parts = append(parts, factorNames[b])
		}
	}
	return strings.Join(parts, ",")
}

// IndexOfLastClosed 找出最后一根已收盘 K 线的下标。
// 优先用 OKX 的 confirm 字段；拿不到就按「bar 周期 + 当前时间」推算。
func IndexOfLastClosed(candles []Candle, bar string, nowMs int64) int {
	if len(candles) == 0 {
		return -1
	}
	// 1) 优先用 OKX 的 confirm 字段：最后一根往往是正在走、还没收盘的那根，必须丢掉
	for i := len(candles) - 1; i >= 0; i-- {
		if candles[i].Confirm {
			return i
		}
	}
	// 2) 拿不到 confirm（异常情况）→ 按 bar 周期 + 当前时间推算
	dur := BarDurationMs(bar)
	if dur <= 0 {
		return len(candles) - 1
	}
	for i := len(candles) - 1; i >= 0; i-- {
		if candles[i].Ts+dur <= nowMs {
			return i
		}
	}
	return -1
}

// BarDurationMs 周期字符串 → 毫秒
func BarDurationMs(bar string) int64 {
	b := strings.TrimSpace(bar)
	if b == "" {
		return 0
	}
	unit := b[len(b)-1]
	num := b[:len(b)-1]
	n := atoiSafe(num)
	if n <= 0 {
		return 0
	}
	switch unit {
	case 'm':
		return int64(n) * 60 * 1000
	case 'H', 'h':
		return int64(n) * 3600 * 1000
	case 'D', 'd':
		return int64(n) * 24 * 3600 * 1000
	case 'W', 'w':
		return int64(n) * 7 * 24 * 3600 * 1000
	}
	return 0
}

func atoiSafe(s string) int {
	v := 0
	if s == "" {
		return 0
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0
		}
		v = v*10 + int(s[i]-'0')
	}
	return v
}
