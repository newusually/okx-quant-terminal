package service

// dukascopy_test.go —— NQ 只读板块核心逻辑单测
//
// 这里刻意**不依赖网络**：bi5 的测试数据用 lzma.NewWriter 现场生成，
// 与真实文件同格式（自带 13 字节 .lzma 头），所以能真正验证
// 「解码 → 聚合」这条链，而不是把网络故障伪装成测试失败。

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
	"time"

	"github.com/ulikunitz/xz/lzma"
)

// buildBi5 按 Dukascopy 真实字节布局造一个 .bi5 文件。
// 记录 24 字节、大端：off秒, O, C, L, H, float32 V（★ 顺序 O/C/L/H）
func buildBi5(t *testing.T, recs []dukaMin) []byte {
	t.Helper()
	raw := make([]byte, 0, len(recs)*24)
	for _, r := range recs {
		var b [24]byte
		binary.BigEndian.PutUint32(b[0:4], uint32(r.Off))
		binary.BigEndian.PutUint32(b[4:8], uint32(math.Round(r.O*nqPointDiv)))
		binary.BigEndian.PutUint32(b[8:12], uint32(math.Round(r.C*nqPointDiv)))
		binary.BigEndian.PutUint32(b[12:16], uint32(math.Round(r.L*nqPointDiv)))
		binary.BigEndian.PutUint32(b[16:20], uint32(math.Round(r.H*nqPointDiv)))
		binary.BigEndian.PutUint32(b[20:24], math.Float32bits(float32(r.V)))
		raw = append(raw, b[:]...)
	}
	var buf bytes.Buffer
	w, err := lzma.NewWriter(&buf)
	if err != nil {
		t.Fatalf("lzma.NewWriter: %v", err)
	}
	if _, err := w.Write(raw); err != nil {
		t.Fatalf("write raw: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// 1. URL 的月份是 0-based —— 踩过一次的坑，必须钉死
// ---------------------------------------------------------------------------

func TestDukaDayURL_MonthIsZeroBased(t *testing.T) {
	cases := []struct {
		day  time.Time
		want string
	}{
		{time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), "https://datafeed.dukascopy.com/datafeed/USATECHIDXUSD/2026/09/02/BID_candles_min_1.bi5"},
		{time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC), "https://datafeed.dukascopy.com/datafeed/USATECHIDXUSD/2026/00/15/BID_candles_min_1.bi5"},
		{time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC), "https://datafeed.dukascopy.com/datafeed/USATECHIDXUSD/2025/11/31/BID_candles_min_1.bi5"},
	}
	for _, c := range cases {
		got := dukaDayURL(c.day)
		if got != c.want {
			t.Errorf("dukaDayURL(%s)\n  得到 %s\n  期望 %s", c.day.Format("2006-01-02"), got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// 2. 解码往返：造 1440 条 → 压缩 → 解码，字段必须逐条一致
// ---------------------------------------------------------------------------

func TestDecodeBi5_RoundTrip(t *testing.T) {
	in := make([]dukaMin, 1440)
	for i := range in {
		base := 30000.0 + float64(i)*0.25
		in[i] = dukaMin{
			Off: i * 60,
			O:   base,
			H:   base + 3.5,
			L:   base - 2.25,
			C:   base + 1.75,
			V:   float64(i%7) * 0.1,
		}
	}
	raw := buildBi5(t, in)
	out, err := decodeBi5(raw)
	if err != nil {
		t.Fatalf("decodeBi5: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("记录数 = %d，期望 %d", len(out), len(in))
	}
	for i := range in {
		if out[i].Off != in[i].Off {
			t.Fatalf("第 %d 条 off = %d，期望 %d", i, out[i].Off, in[i].Off)
		}
		if math.Abs(out[i].O-in[i].O) > 1e-9 || math.Abs(out[i].H-in[i].H) > 1e-9 ||
			math.Abs(out[i].L-in[i].L) > 1e-9 || math.Abs(out[i].C-in[i].C) > 1e-9 {
			t.Fatalf("第 %d 条 OHLC 不一致：%+v vs %+v", i, out[i], in[i])
		}
	}
}

// TestDecodeBi5_FieldOrderIsOCLH 是**防回归的关键测试**。
//
// 记录里的字段顺序是 O / C / L / H，不是直觉的 O / H / L / C。
// 一旦有人「顺手改成正序」，OHLC 会静默错位（图能画出来但全是错的）。
// 这里造一条刻意区分四者的记录：O=1000 C=1001 L=990 H=1010
// 如果顺序写错，解出来的 H 会等于 990 或 L 会等于 1010，立刻被抓。
func TestDecodeBi5_FieldOrderIsOCLH(t *testing.T) {
	one := []dukaMin{{Off: 0, O: 1000, C: 1001, L: 990, H: 1010, V: 1.5}}
	out, err := decodeBi5(buildBi5(t, one))
	if err != nil {
		t.Fatalf("decodeBi5: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("记录数 = %d", len(out))
	}
	r := out[0]
	if r.O != 1000 || r.C != 1001 || r.L != 990 || r.H != 1010 {
		t.Fatalf("字段顺序错了：O=%v C=%v L=%v H=%v（期望 O=1000 C=1001 L=990 H=1010）", r.O, r.C, r.L, r.H)
	}
}

// TestDecodeBi5_DropsInconsistent 不自洽的记录（H < L / 价格 <= 0）必须被丢掉
func TestDecodeBi5_DropsInconsistent(t *testing.T) {
	recs := []dukaMin{
		{Off: 0, O: 1000, C: 1001, L: 990, H: 1010, V: 1},   // 好
		{Off: 60, O: 1000, C: 1001, L: 1050, H: 1010, V: 1}, // H < L 坏
		{Off: 120, O: 0, C: 1001, L: 990, H: 1010, V: 1},    // O=0 坏
		{Off: 180, O: 1000, C: 1002, L: 995, H: 1005, V: 1}, // 好
	}
	out, err := decodeBi5(buildBi5(t, recs))
	if err != nil {
		t.Fatalf("decodeBi5: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("有效记录 = %d，期望 2（另 2 条该被丢弃）", len(out))
	}
}

// ---------------------------------------------------------------------------
// 3. 聚合：1m → 3m / 5m / 15m
// ---------------------------------------------------------------------------

func TestAggregateNQDay_BarCounts(t *testing.T) {
	recs := make([]dukaMin, 1440)
	for i := range recs {
		p := 30000.0 + float64(i)
		recs[i] = dukaMin{Off: i * 60, O: p, H: p + 1, L: p - 1, C: p + 0.5, V: 1}
	}
	dayStart := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).UnixMilli()

	want := map[string]int{"3m": 480, "5m": 288, "15m": 96}
	for bar, n := range want {
		got := aggregateNQDay(recs, dayStart, bar)
		if len(got) != n {
			t.Errorf("%s 根数 = %d，期望 %d（1440/%s）", bar, len(got), n, bar[:len(bar)-1])
		}
		// 首根必须是当天 00:00 UTC，且 ts 对齐整秒（IsValidKline 的硬要求）
		if got[0].Ts != dayStart {
			t.Errorf("%s 首根 ts = %d，期望 %d", bar, got[0].Ts, dayStart)
		}
		for i, k := range got {
			if k.Ts%1000 != 0 {
				t.Fatalf("%s 第 %d 根 ts 未对齐整秒：%d", bar, i, k.Ts)
			}
			if k.InstID != NQInstID || k.Bar != bar {
				t.Fatalf("%s 第 %d 根标识错：%s/%s", bar, i, k.InstID, k.Bar)
			}
		}
		// 末根：一天 1440 分钟切成 N = 1440×60/step 根，最后一根 ts = dayStart + (N-1)×step
		// ⚠ step 的单位是「秒」，1440 的单位是「分钟」—— 换算必须乘 60
		step := barSeconds(bar)
		nBars := 1440 * 60 / step
		wantLast := dayStart + int64(nBars-1)*int64(step)*1000
		if got[len(got)-1].Ts != wantLast {
			t.Errorf("%s 末根 ts = %d，期望 %d", bar, got[len(got)-1].Ts, wantLast)
		}
	}
}

// TestAggregateNQDay_OHLCV 聚合后的开高低收量必须是「首开、最高、最低、末收、累加」
func TestAggregateNQDay_OHLCV(t *testing.T) {
	// 3 根 1m：价格刻意做出「先冲高、再砸低、最后回收」
	recs := []dukaMin{
		{Off: 0, O: 100, H: 110, L: 99, C: 105, V: 2},
		{Off: 60, O: 105, H: 108, L: 90, C: 92, V: 3},
		{Off: 120, O: 92, H: 96, L: 91, C: 95, V: 5},
	}
	got := aggregateNQDay(recs, 0, "3m")
	if len(got) != 1 {
		t.Fatalf("根数 = %d，期望 1", len(got))
	}
	k := got[0]
	if k.O != 100 {
		t.Errorf("开盘 = %v，期望首根开盘 100", k.O)
	}
	if k.H != 110 {
		t.Errorf("最高 = %v，期望 110", k.H)
	}
	if k.L != 90 {
		t.Errorf("最低 = %v，期望 90", k.L)
	}
	if k.C != 95 {
		t.Errorf("收盘 = %v，期望末根收盘 95", k.C)
	}
	if k.V != 10 {
		t.Errorf("成交量 = %v，期望 2+3+5=10", k.V)
	}
}

// TestAggregateNQDay_SplitsBuckets 桶边界必须落在周期整刻上（UTC 对齐）
func TestAggregateNQDay_SplitsBuckets(t *testing.T) {
	// 6 根 1m，跨 3 分钟边界
	recs := make([]dukaMin, 6)
	for i := range recs {
		recs[i] = dukaMin{Off: i * 60, O: 100 + float64(i), H: 100 + float64(i), L: 100 + float64(i), C: 100 + float64(i), V: 1}
	}
	got := aggregateNQDay(recs, 0, "3m")
	if len(got) != 2 {
		t.Fatalf("根数 = %d，期望 2（每 3 分钟一根）", len(got))
	}
	if got[0].Ts != 0 || got[1].Ts != 180000 {
		t.Fatalf("ts 错位：%d / %d，期望 0 / 180000", got[0].Ts, got[1].Ts)
	}
	if got[0].C != 102 || got[1].O != 103 {
		t.Fatalf("桶切分错位：第一根收 %v（期望 102）、第二根开 %v（期望 103）", got[0].C, got[1].O)
	}
}

func TestAggregateNQDay_UnknownBarReturnsNil(t *testing.T) {
	recs := []dukaMin{{Off: 0, O: 100, H: 101, L: 99, C: 100, V: 1}}
	if got := aggregateNQDay(recs, 0, "7m"); got != nil {
		t.Fatalf("未知周期应返回 nil，得到 %d 根", len(got))
	}
	if got := aggregateNQDay(nil, 0, "3m"); got != nil {
		t.Fatalf("空输入应返回 nil，得到 %d 根", len(got))
	}
}

// ---------------------------------------------------------------------------
// 4. 只读合约名单
// ---------------------------------------------------------------------------

func TestIsReadonlyInst(t *testing.T) {
	if !IsReadonlyInst(NQInstID) {
		t.Errorf("%s 必须是只读合约", NQInstID)
	}
	// OKX 的正常合约绝不能落进只读名单，否则会「莫名其妙不交易」
	for _, s := range []string{"BTC-USDT-SWAP", "ETH-USDT-SWAP", ""} {
		if IsReadonlyInst(s) {
			t.Errorf("%q 不该是只读合约", s)
		}
	}
}

func TestReadonlyInstName(t *testing.T) {
	if got := ReadonlyInstName(NQInstID); got != "NQ / 纳斯达克100" {
		t.Errorf("展示名 = %q", got)
	}
	if got := ReadonlyInstName("BTC-USDT-SWAP"); got != "BTC-USDT-SWAP" {
		t.Errorf("未知合约应原样返回，得到 %q", got)
	}
}

// ---------------------------------------------------------------------------
// 5. 周期秒数
// ---------------------------------------------------------------------------

func TestBarSeconds(t *testing.T) {
	cases := map[string]int{"3m": 180, "5m": 300, "15m": 900, "1m": 0, "": 0}
	for bar, want := range cases {
		if got := barSeconds(bar); got != want {
			t.Errorf("barSeconds(%q) = %d，期望 %d", bar, got, want)
		}
	}
	// NQBars 里的每个周期都必须能算出秒数，否则聚合会静默产出 0 根
	for _, bar := range NQBars {
		if barSeconds(bar) <= 0 {
			t.Fatalf("NQBars 里的 %q 没有对应秒数", bar)
		}
	}
}
