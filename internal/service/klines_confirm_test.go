package service

// klines_confirm_test.go —— 守住一条**会静默失效**的回归
//
// 背景（真实踩坑，2026-10-01）：
//
//	把 K 线来源从「网络」换成「本地库」时，有两套转换函数：
//
//	  klinesToCandles    Confirm 恒留 false —— 专给 IndexOfLastClosed 的
//	                     「按周期 + 时间推算」分支用（库里会存正在走的那根）
//	  klinesToCandlesAt  Confirm 按 c.Ts+dur <= now 算出来 —— 给**直接读
//	                     Confirm 字段**的老逻辑用
//
//	老逻辑就在 addon.go：decideAddon 的输入要过 closedWindow，而 closedWindow
//	第一件事就是 `if !c.Confirm { continue }`。
//
//	如果把 klinesToCandles 的输出喂给 closedWindow：
//	  窗口恒为空 → len(win) < 2 → checkAddon 直接返回「不加仓」
//	→ **加仓功能静默地永久失效**。不报错、不崩、日志干净、测试全绿，
//	  只有事后对账才发现「怎么三个月一次都没加过仓」。
//
// 这几个测试就是钉死它：语义必须与 OKX 网络的 confirm 字段一致。

import (
	"testing"

	"finally-main/internal/model"
)

func mkKLines(tsList ...int64) []model.Kline {
	out := make([]model.Kline, len(tsList))
	for i, ts := range tsList {
		out[i] = model.Kline{
			InstID: "T-USDT-SWAP", Bar: "15m", Ts: ts,
			O: 1, H: 1.1, L: 0.9, C: 1.0, V: 100,
		}
	}
	return out
}

// TestKlinesToCandlesAtConfirmSemantics 逐根钉死「已收盘」的判定口径。
func TestKlinesToCandlesAtConfirmSemantics(t *testing.T) {
	const bar = "15m"
	dur := BarDurationMs(bar)
	if dur != 900_000 {
		t.Fatalf("BarDurationMs(%q) = %d，期望 900000", bar, dur)
	}

	// 造 4 根：ts = base, base+dur, base+2dur, base+3dur
	base := int64(1_700_000_000_000)
	rows := mkKLines(base, base+dur, base+2*dur, base+3*dur)

	// now 落在第 4 根走的过程中：前 3 根已收盘，第 4 根没有
	nowMs := base + 3*dur + dur/2
	got := klinesToCandlesAt(rows, bar, nowMs)
	if len(got) != 4 {
		t.Fatalf("长度应当保持 4，实际 %d", len(got))
	}
	for i, c := range got {
		want := c.Ts+dur <= nowMs
		if c.Confirm != want {
			t.Errorf("第 %d 根 ts=%d：Confirm=%v，期望 %v（now=%d, dur=%d）",
				i, c.Ts, c.Confirm, want, nowMs, dur)
		}
	}
	if got[3].Confirm {
		t.Error("第 4 根（正在走）不该算已收盘")
	}
}

// TestKlinesToCandlesAtBoundaryExactly 恰好落在收盘瞬间：ts+dur == now。
// 口径是「<=」，即这一刻起已经算收盘 —— 与 IndexOfLastClosed 的第二分支逐字一致。
func TestKlinesToCandlesAtBoundaryExactly(t *testing.T) {
	const bar = "15m"
	dur := BarDurationMs(bar)
	base := int64(1_700_000_000_000)
	rows := mkKLines(base, base+dur)

	got := klinesToCandlesAt(rows, bar, base+dur)
	if !got[0].Confirm {
		t.Error("ts+dur == now 时应算已收盘（口径是 <=）")
	}
	if got[1].Confirm {
		t.Error("最新一根刚开盘，不该算已收盘")
	}

	// 差 1 毫秒：还没收盘
	got2 := klinesToCandlesAt(rows, bar, base+dur-1)
	if got2[0].Confirm {
		t.Error("ts+dur > now 时不该算已收盘")
	}
}

// TestKlinesToCandlesAtUnknownBarSafe 周期字符串不认识时不 panic、且一律判未收盘。
// 宁可少一根（下轮再来），也不能把正在走的那根当成已收盘去做交易决策。
func TestKlinesToCandlesAtUnknownBarSafe(t *testing.T) {
	base := int64(1_700_000_000_000)
	rows := mkKLines(base)
	got := klinesToCandlesAt(rows, "这不是周期", base+999_999_999)
	if len(got) != 1 {
		t.Fatalf("长度应当保持 1，实际 %d", len(got))
	}
	if got[0].Confirm {
		t.Error("周期不认识时 dur<=0，必须判为未收盘")
	}
}

// TestClosedWindowWorksWithLocalCandles ★ 核心回归 ★
//
// 直接复现「用错转换函数 → 加仓永久失效」：同一份本地库数据，
// klinesToCandlesAt 出来的窗口必须非空，klinesToCandles 出来的必然是空。
// 把这件事写成断言，以后谁改错都会当场红。
func TestClosedWindowWorksWithLocalCandles(t *testing.T) {
	const bar = "15m"
	dur := BarDurationMs(bar)
	nowMs := int64(1_700_000_000_000)
	openTs := nowMs - 5*dur

	rows := mkKLines(
		nowMs-6*dur, nowMs-5*dur, nowMs-4*dur, nowMs-3*dur,
		nowMs-2*dur, nowMs-dur, nowMs, // 最后一根正在走
	)

	// ① 正确用法：Confirm 已按时间补上 → 窗口非空
	good := closedWindow(klinesToCandlesAt(rows, bar, nowMs), openTs, dur)
	if len(good) < 2 {
		t.Fatalf("★ 回归 ★ 用 klinesToCandlesAt 时窗口只有 %d 根，"+
			"decideAddon 会因 len(win)<2 直接跳过 —— 加仓会静默失效", len(good))
	}
	for _, c := range good {
		if c.Ts+dur > nowMs {
			t.Errorf("窗口里混进了未收盘的 K 线 ts=%d", c.Ts)
		}
		if c.Ts+dur <= openTs {
			t.Errorf("窗口里混进了开仓前就收完的 K 线 ts=%d", c.Ts)
		}
	}

	// ② 错误用法：Confirm 恒 false → 窗口必然为空（这正是要防住的事故）
	bad := closedWindow(klinesToCandles(rows), openTs, dur)
	if len(bad) != 0 {
		t.Errorf("klinesToCandles 的 Confirm 是恒 false，窗口却出了 %d 根；"+
			"如果这条挂了，说明两个转换函数的职责被混了", len(bad))
	}
}

// TestTwoConvertersAreDeliberatelyDifferent 记录两者的分工，
// 免得后人「看到两个几乎一样的函数」就想合并掉。
func TestTwoConvertersAreDeliberatelyDifferent(t *testing.T) {
	const bar = "15m"
	dur := BarDurationMs(bar)
	nowMs := int64(1_700_000_000_000)
	rows := mkKLines(nowMs-dur, nowMs-2*dur)

	a := klinesToCandles(rows)
	b := klinesToCandlesAt(rows, bar, nowMs)
	for i := range a {
		if a[i].Confirm {
			t.Errorf("klinesToCandles 不该设置 Confirm（第 %d 根）", i)
		}
		if !b[i].Confirm {
			t.Errorf("klinesToCandlesAt 应当把已收盘的都标上（第 %d 根）", i)
		}
	}
	// 其余字段必须逐位一致 —— 两个函数只允许在 Confirm 上有差别
	for i := range a {
		if a[i].Ts != b[i].Ts || a[i].O != b[i].O || a[i].H != b[i].H ||
			a[i].L != b[i].L || a[i].C != b[i].C || a[i].V != b[i].V {
			t.Errorf("第 %d 根的 OHLCV 不一致：%+v vs %+v", i, a[i], b[i])
		}
	}
}
