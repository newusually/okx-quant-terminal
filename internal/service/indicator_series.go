package service

// indicator_series.go —— 指标批量化 + 缓冲池化（2026-10-01 性能改造）
//
// 解决两个实测问题：
//
//	① O(n²)：原 ComputeSignal(instID, bar, candles, idx) 为了判 **一根** K 线，
//	   把整段 1500 根的指标全部重算一遍。逐根扫完整段 = 1500 次全长计算。
//	   实测 bars1500 逐根跑完要 440.6ms、分配 361MB。
//	   修法：拆成「算一次序列」+「O(1) 取任意下标」，逐根扫描退化成 O(n)。
//
//	② 每次分配 258KB：原实现每次都 make 出 h/l/c/v + 十几个指标数组。
//	   实测 numGC 每秒 7.2 次，而 live heap 只有 3.5MB —— 全是短命大对象。
//	   修法：sync.Pool 复用缓冲，容量够就只 reslice、不重新分配。
//
// ★ 数值口径绝对不许变 ★
//	本文件是把 indicator.go 里的计算**逐行照搬**过来的，只改了「写到哪里」。
//	公式、周期、NaN 填充范围、比较方向必须与原实现比特一致，
//	否则买卖点会漂移。indicator_series_test.go 里有等价性测试兜底。

import (
	"math"
	"math/bits"
	"sync"
)

// ---------------------------------------------------------------------------
// 复用的指标缓冲
// ---------------------------------------------------------------------------

// indicatorScratch 一次计算要用到的全部中间数组。
// 所有切片都只增不减：容量够就 reslice，不够才扩容 —— 稳态下零分配。
type indicatorScratch struct {
	h, l, c, v []float64
	tr         []float64 // atrPercent 的中间量，两个周期共用

	atrp14, atrp96          []float64
	sma200, sma48v, sma20v  []float64
	rsi14                   []float64
	bollLo, bollUp, bollMid []float64
	macdH                   []float64

	// MACD 内部临时量
	macdEf, macdEs, macdDif, macdDea []float64

	td []int
}

var scratchPool = sync.Pool{New: func() any { return new(indicatorScratch) }}

// grow 保证 dst 至少能装 n 个元素，并返回长度为 n 的切片。
//
// 关键点：**已经存在的内容不保证保留**，调用方必须自己写满全部 n 个位置。
// 靠这个约定，长数组复用给短数组时不会被旧值污染（各指标函数都显式填满）。
func grow(dst []float64, n int) []float64 {
	if cap(dst) < n {
		return make([]float64, n)
	}
	return dst[:n]
}

func growInt(dst []int, n int) []int {
	if cap(dst) < n {
		return make([]int, n)
	}
	return dst[:n]
}

// polyfills 把 out 的前 n 个元素全部填成 v。
// 比 for i := range out 更明确：范围只到 n，不碰容量尾部。
func fillF(out []float64, v float64) {
	for i := range out {
		out[i] = v
	}
}

// compute 把 candles 解包进缓冲并把全部指标算一遍。O(n)，只做一次。
func (s *indicatorScratch) compute(candles []Candle) {
	n := len(candles)
	s.h = grow(s.h, n)
	s.l = grow(s.l, n)
	s.c = grow(s.c, n)
	s.v = grow(s.v, n)
	for i := 0; i < n; i++ {
		s.h[i], s.l[i], s.c[i], s.v[i] = candles[i].H, candles[i].L, candles[i].C, candles[i].V
	}

	s.tr = grow(s.tr, n)
	trueRangeInto(s.tr, s.h, s.l, s.c, n)

	// 与 ComputeSignal 的调用顺序、参数完全一致
	s.atrp14 = grow(s.atrp14, n)
	atrPercentInto(s.atrp14, s.tr, s.c, n, 14)

	s.atrp96 = grow(s.atrp96, n)
	atrPercentInto(s.atrp96, s.tr, s.c, n, 96)

	s.sma200 = grow(s.sma200, n)
	smaInto(s.sma200, s.c, n, 200)

	s.sma48v = grow(s.sma48v, n)
	smaInto(s.sma48v, s.v, n, 48)

	s.sma20v = grow(s.sma20v, n)
	smaInto(s.sma20v, s.v, n, 20)

	s.rsi14 = grow(s.rsi14, n)
	rsiInto(s.rsi14, s.c, n, 14)

	s.bollLo = grow(s.bollLo, n)
	s.bollUp = grow(s.bollUp, n)
	s.bollMid = grow(s.bollMid, n)
	bollInto(s.bollLo, s.bollUp, s.bollMid, s.c, n, 20, 2.0)

	s.macdH = grow(s.macdH, n)
	s.macdEf = grow(s.macdEf, n)
	s.macdEs = grow(s.macdEs, n)
	s.macdDif = grow(s.macdDif, n)
	s.macdDea = grow(s.macdDea, n)
	macdHistInto(s.macdH, s.c, n, 12, 26, 60, s.macdEf, s.macdEs, s.macdDif, s.macdDea)

	s.td = growInt(s.td, n)
	tdSetupInto(s.td, s.c, n)
}

// signalAt 从已算好的指标里取第 idx 根，组装成 Signal。**O(1)**。
//
// 返回的 *Signal 是独立副本，跟 scratch 没有共享，调用方可以随便留。
func (s *indicatorScratch) signalAt(instID, bar string, candles []Candle, idx int) *Signal {
	out := &Signal{InstID: instID, Bar: bar}
	n := len(candles)
	if n == 0 || idx < 0 || idx >= n {
		return out // Ready 保持 false —— 与原实现一致
	}

	cur := candles[idx]
	out.Ts = cur.Ts
	out.Close = cur.C
	out.Open = cur.O
	out.High = cur.H
	out.Low = cur.L
	out.Vol = cur.V

	// 这一根自己的涨跌幅（三期新增，见 Signal.RisePct）。
	// 开价为 0 是脏数据 → 保持 0，这样它过不了「必须涨过 N%」的门槛（偏保守）。
	if cur.O > 0 {
		out.RisePct = (cur.C - cur.O) / cur.O * 100
	}

	c := s.c
	v := s.v
	i := idx

	// ---- 中间量 ----
	if !isNaN(s.sma200[i]) && !isNaN(s.atrp14[i]) && s.atrp14[i] != 0 {
		out.Pot = (c[i] - s.sma200[i]) / s.atrp14[i]
	} else {
		out.Pot = math.NaN()
	}
	if !isNaN(s.atrp14[i]) && !isNaN(s.atrp96[i]) && s.atrp96[i] != 0 {
		out.Fri = s.atrp14[i] / s.atrp96[i]
	} else {
		out.Fri = math.NaN()
	}
	if i >= 6 && !isNaN(s.sma48v[i]) && s.sma48v[i] != 0 && c[i-6] != 0 {
		vel := (c[i]/c[i-6] - 1) * 100
		vr := v[i] / s.sma48v[i]
		sign := 0.0
		if vel > 0 {
			sign = 1
		} else if vel < 0 {
			sign = -1
		}
		out.Kin = 0.5 * vr * vel * vel * sign
	} else {
		out.Kin = math.NaN()
	}
	out.Rsi = s.rsi14[i]
	out.Td = s.td[i]
	out.BollLo = s.bollLo[i]
	out.BollUp = s.bollUp[i]
	out.MacdH = s.macdH[i]

	// ---- 位掩码 ----
	mask := 0
	if !isNaN(out.Pot) && out.Pot < -1.0 {
		mask |= 1 << 0
	}
	if !isNaN(out.Fri) && out.Fri > 0.5 {
		mask |= 1 << 1
	}
	if !isNaN(out.Kin) && out.Kin < 0.0 {
		mask |= 1 << 2
	}
	if !isNaN(out.Rsi) && out.Rsi < 30.0 {
		mask |= 1 << 3
	}
	if !isNaN(out.BollLo) && c[i] < out.BollLo {
		mask |= 1 << 4
	}
	if i >= 2 && s.macdH[i] > s.macdH[i-1] && s.macdH[i-1] > s.macdH[i-2] {
		mask |= 1 << 5
	}
	if i >= 4 && s.td[i] >= 7 {
		mask |= 1 << 6
	}
	if !isNaN(s.sma20v[i]) && s.sma20v[i] != 0 && v[i] > 1.5*s.sma20v[i] {
		mask |= 1 << 7
	}

	out.Mask = mask
	out.Score = bits.OnesCount(uint(mask))
	out.HitList = hitList(mask)
	out.Ready = !isNaN(out.Pot) && !isNaN(out.BollLo) && !isNaN(out.Rsi)
	return out
}

// ---------------------------------------------------------------------------
// 公开 API
// ---------------------------------------------------------------------------

// ComputeSignal 判 cands[idx] 这一根的 8 因子。
//
// 语义与改造前**完全一致**，但内部换成「池化缓冲 + 算一遍」。
// 注意：如果要对同一段 K 线取多个下标，请改用 ComputeSeries ——
// 那样指标只算一次，而不是每个下标都重算一遍（O(n²) → O(n)）。
func ComputeSignal(instID, bar string, candles []Candle, idx int) *Signal {
	sc := scratchPool.Get().(*indicatorScratch)
	defer scratchPool.Put(sc)
	sc.compute(candles)
	return sc.signalAt(instID, bar, candles, idx)
}

// SignalSeries 一段 K 线的信号序列。指标只算一次，之后任意下标都是 O(1)。
//
// 生命周期：**调用方必须 Release()**（通常 `defer ss.Release()`）。
// 忘记 Release 不会出错，只是这次池化白做；Release 之后的 At() 返回 nil。
type SignalSeries struct {
	InstID  string
	Bar     string
	candles []Candle
	sc      *indicatorScratch
}

// ComputeSeries 把整段 K 线的指标算一遍，返回可按下标取信号的序列。
func ComputeSeries(instID, bar string, candles []Candle) *SignalSeries {
	sc := scratchPool.Get().(*indicatorScratch)
	sc.compute(candles)
	return &SignalSeries{InstID: instID, Bar: bar, candles: candles, sc: sc}
}

// Len 序列长度（= K 线根数）
func (ss *SignalSeries) Len() int {
	if ss == nil {
		return 0
	}
	return len(ss.candles)
}

// At 取第 idx 根的信号。O(1)。Release() 之后返回 nil。
func (ss *SignalSeries) At(idx int) *Signal {
	if ss == nil || ss.sc == nil {
		return nil
	}
	return ss.sc.signalAt(ss.InstID, ss.Bar, ss.candles, idx)
}

// All 逐根取出全部信号（顺序：老 → 新）。
// 只在确实需要整段结果时用；只要尾部一根就用 At()。
func (ss *SignalSeries) All() []*Signal {
	n := ss.Len()
	if n == 0 {
		return nil
	}
	out := make([]*Signal, n)
	for i := 0; i < n; i++ {
		out[i] = ss.At(i)
	}
	return out
}

// Release 把内部缓冲还给池。可重复调用。
func (ss *SignalSeries) Release() {
	if ss != nil && ss.sc != nil {
		scratchPool.Put(ss.sc)
		ss.sc = nil
	}
}

// ---------------------------------------------------------------------------
// 就地（into）版指标 —— 与 indicator.go 里的分配版逐行等价
// ---------------------------------------------------------------------------

// trueRangeInto 与原 trueRange 等价：tr[0]=h-l，tr[i]=max(h-l,|h-c[i-1]|,|l-c[i-1]|)
func trueRangeInto(tr, h, l, c []float64, n int) {
	if n == 0 {
		return
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
}

// atrPercentInto 与原 atrPercent 等价，但 tr 由外部传入（两个周期共用一次 TR）
func atrPercentInto(out, tr, c []float64, n, period int) {
	fillF(out, math.NaN())
	if period <= 0 || n < period+1 {
		return
	}
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
}

// smaInto 与原 sma 等价
func smaInto(out, x []float64, n, period int) {
	fillF(out, math.NaN())
	if period <= 0 || n < period {
		return
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
}

// emaInto 与原 ema 等价（ema[0] = x[0]）
func emaInto(out, x []float64, n, period int) {
	if n == 0 {
		return
	}
	if period <= 0 {
		fillF(out, 0)
		return
	}
	k := 2.0 / float64(period+1)
	out[0] = x[0]
	for i := 1; i < n; i++ {
		out[i] = x[i]*k + out[i-1]*(1-k)
	}
}

// rsiInto 与原 rsiWilder 等价
func rsiInto(out, c []float64, n, period int) {
	fillF(out, math.NaN())
	if period <= 0 || n < period+1 {
		return
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
}

// bollInto 与原 boll 等价（总体方差，除以 period）
func bollInto(lower, upper, mid, c []float64, n, period int, mult float64) {
	fillF(lower, math.NaN())
	fillF(upper, math.NaN())
	fillF(mid, math.NaN())
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
		v /= float64(period)
		sd := math.Sqrt(v)
		mid[i] = m
		lower[i] = m - mult*sd
		upper[i] = m + mult*sd
	}
}

// macdHistInto 与原 macdHist 等价；ef/es/dif/dea 是调用方提供的临时缓冲
func macdHistInto(out, c []float64, n, fast, slow, signal int, ef, es, dif, dea []float64) {
	if n == 0 {
		return
	}
	emaInto(ef, c, n, fast)
	emaInto(es, c, n, slow)
	for i := 0; i < n; i++ {
		dif[i] = ef[i] - es[i]
	}
	emaInto(dea, dif, n, signal)
	for i := 0; i < n; i++ {
		out[i] = 2 * (dif[i] - dea[i])
	}
}

// tdSetupInto 与原 tdSetup 等价。
//
// ★ 唯一需要注意的复用陷阱：原实现靠 make 把前 4 个元素默认成 0，
// 池化复用后它们会是上一次留下的旧值，必须显式清零。
func tdSetupInto(out []int, c []float64, n int) {
	head := n
	if head > 4 {
		head = 4
	}
	for i := 0; i < head; i++ {
		out[i] = 0
	}
	for i := 4; i < n; i++ {
		if c[i] < c[i-4] {
			out[i] = out[i-1] + 1
		} else {
			out[i] = 0
		}
	}
}
