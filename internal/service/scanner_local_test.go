package service

import (
	"testing"

	"finally-main/internal/model"
)

// mkRows 造 n 根 15m K 线，最新一根的 ts = lastTs
func mkRows(n int, lastTs int64) []model.Kline {
	const dur = 900_000
	out := make([]model.Kline, n)
	for i := 0; i < n; i++ {
		out[i] = model.Kline{
			InstID: "T-USDT-SWAP", Bar: "15m",
			Ts: lastTs - int64(n-1-i)*dur,
			O:  1, H: 1.1, L: 0.9, C: 1.0, V: 100,
		}
	}
	return out
}

// TestLocalCandlesFresh ★ 交易安全的关键判据 ★
//
// 判错方向只会有一个后果：拿着**上一根** K 线算信号 → 漏单或重复下单。
// 所以这里把边界逐个钉死。
func TestLocalCandlesFresh(t *testing.T) {
	const dur = 900_000 // 15m
	// 选一个「刚好在某根 K 线中间」的时刻：now = 10 根 + 半根
	nowMs := int64(1000)*dur + dur/2
	// 正在走的那根开盘于 1000*dur
	// 最后一根已收盘的 K 线开盘于 999*dur
	curBarOpen := int64(1000) * dur
	lastClosed := int64(999) * dur

	cases := []struct {
		name       string
		n          int
		lastTs     int64
		minCandles int
		want       bool
	}{
		{"刚好含最后一根已收盘 → 可用", 400, lastClosed, 400, true},
		{"含正在走的那根（更新）→ 可用", 400, curBarOpen, 400, true},
		{"还差一根（DB 落后）→ 必须回网络", 400, lastClosed - dur, 400, false},
		{"落后很多 → 必须回网络", 400, lastClosed - 10*dur, 400, false},
		{"根数刚好达标 → 可用", 400, lastClosed, 400, true},
		{"根数差一根 → 必须回网络", 399, lastClosed, 400, false},
		{"根数为 0 → 必须回网络", 0, 0, 400, false},
		{"minCandles=0（未配置）→ 必须回网络", 400, lastClosed, 0, false},
	}
	for _, c := range cases {
		got := localCandlesFresh(mkRows(c.n, c.lastTs), "15m", c.minCandles, nowMs)
		if got != c.want {
			t.Errorf("%s：localCandlesFresh=%v，期望 %v", c.name, got, c.want)
		}
	}

	// 边界：now 恰好落在 K 线开盘的瞬间
	if !localCandlesFresh(mkRows(400, lastClosed), "15m", 400, curBarOpen) {
		t.Error("now 正好等于当前 K 线开盘时刻时，应该认为「最后一根已收盘」可用")
	}

	// 未知周期必须回退，不能瞎猜
	if localCandlesFresh(mkRows(400, lastClosed), "99x", 400, nowMs) {
		t.Error("无法解析的周期必须判为不可用（回退网络）")
	}
}

// TestLocalCandlesFreshAcrossBars 换个周期，判据同样成立
func TestLocalCandlesFreshAcrossBars(t *testing.T) {
	for _, bar := range []string{"1m", "5m", "15m", "1H"} {
		dur := BarDurationMs(bar)
		if dur <= 0 {
			t.Fatalf("BarDurationMs(%q) 不该为 0", bar)
		}
		now := int64(50)*dur + dur/3
		lastClosed := (now/dur)*dur - dur
		if !localCandlesFresh(mkRows(400, lastClosed), bar, 400, now) {
			t.Errorf("%s：含最后一根已收盘时应判可用", bar)
		}
		if localCandlesFresh(mkRows(400, lastClosed-dur), bar, 400, now) {
			t.Errorf("%s：落后一根时应判不可用", bar)
		}
	}
}

// TestKlinesToCandles 转换必须保真，且 Confirm 必须全部为 false
func TestKlinesToCandles(t *testing.T) {
	rows := []model.Kline{
		{InstID: "A", Bar: "15m", Ts: 1000, O: 1.5, H: 1.7, L: 1.4, C: 1.6, V: 42},
		{InstID: "A", Bar: "15m", Ts: 1900, O: 1.6, H: 1.8, L: 1.5, C: 1.55, V: 43},
	}
	cs := klinesToCandles(rows)
	if len(cs) != 2 {
		t.Fatalf("长度 %d != 2", len(cs))
	}
	if cs[0].Ts != 1000 || cs[0].O != 1.5 || cs[0].H != 1.7 || cs[0].L != 1.4 || cs[0].C != 1.6 || cs[0].V != 42 {
		t.Fatalf("第 0 根字段没对上传：%+v", cs[0])
	}
	if cs[1].Ts != 1900 || cs[1].C != 1.55 || cs[1].V != 43 {
		t.Fatalf("第 1 根字段没对上传：%+v", cs[1])
	}
	for i, c := range cs {
		if c.Confirm {
			t.Fatalf("第 %d 根 Confirm 必须为 false（让 IndexOfLastClosed 走时间推算），实得 true", i)
		}
	}
	if len(klinesToCandles(nil)) != 0 {
		t.Fatal("nil 输入应返回空切片")
	}
}

// TestDBPathMatchesNetworkIndex 本地库路径与网络路径必须选中**同一根** K 线
//
// 这是「本地优先」改造最核心的等价性要求：两条路拿到的窗口既然等长，
// 那么 IndexOfLastClosed 也必须落在同一个下标上，否则买卖点会错位。
func TestDBPathMatchesNetworkIndex(t *testing.T) {
	const dur = 900_000
	const n = 400
	nowMs := int64(2000)*dur + dur/2
	curBarOpen := int64(2000) * dur

	// 本地库：含「正在走的那根」
	dbRows := mkRows(n, curBarOpen)
	if !localCandlesFresh(dbRows, "15m", n, nowMs) {
		t.Fatal("前置条件不成立：这份数据应判为可用")
	}
	dbCands := klinesToCandles(dbRows)
	dbIdx := IndexOfLastClosed(dbCands, "15m", nowMs)

	// 网络：同样的 400 根，但 OKX 会给 last 那根 confirm=false，其余 true
	netCands := make([]Candle, n)
	copy(netCands, dbCands)
	for i := 0; i < n-1; i++ {
		netCands[i].Confirm = true
	}
	netCands[n-1].Confirm = false
	netIdx := IndexOfLastClosed(netCands, "15m", nowMs)

	if dbIdx != netIdx {
		t.Fatalf("两条路径选中的下标不一致：本地 %d，网络 %d", dbIdx, netIdx)
	}
	if dbIdx != n-2 {
		t.Fatalf("应选中倒数第二根（最后一根已收盘），实得 %d", dbIdx)
	}
	// 且那一根的收盘时刻必须真的已经过去
	if got := dbCands[dbIdx].Ts + dur; got > nowMs {
		t.Fatalf("选中的 K 线（ts=%d）其实还没收盘（收盘于 %d，now=%d）",
			dbCands[dbIdx].Ts, got, nowMs)
	}

	// 反向：库里只有已收盘的（没有正在走的那根）→ 仍应选中最后一根
	rows2 := mkRows(n, dbCands[dbIdx].Ts)
	c2 := klinesToCandles(rows2)
	if idx := IndexOfLastClosed(c2, "15m", nowMs); idx != n-1 {
		t.Fatalf("库里不含未收盘那根时，应选中最后一根，实得 %d", idx)
	}
}

// TestLocalCandlesFreshRejectsStaleAfterGap 库停了一段时间后重启的场景
//
// 服务停机两小时后重启：库里最新一根还是两小时前的。
// 此时**绝对不能用本地**，必须回网络 —— 否则会拿着两小时前的 K 线算信号。
func TestLocalCandlesFreshRejectsStaleAfterGap(t *testing.T) {
	const dur = 900_000
	lastClosed := int64(500) * dur
	// 停了两小时 = 8 根 15m
	nowMs := lastClosed + 8*dur + dur/2
	if localCandlesFresh(mkRows(400, lastClosed), "15m", 400, nowMs) {
		t.Fatal("停机两小时后，本地数据必须判为不可用（回退网络）")
	}
	// 补上来之后就可用
	if !localCandlesFresh(mkRows(400, (nowMs/dur)*dur-dur), "15m", 400, nowMs) {
		t.Fatal("补齐后应判为可用")
	}
}
