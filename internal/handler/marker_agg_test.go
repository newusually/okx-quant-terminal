package handler

import (
	"math"
	"testing"

	"finally-main/internal/repo"
)

// 三期口径（2026-10-01）：「买入同一根 K 线的时候不能重复在 K 线图上画出来，
// 只能显示买入一笔，但是买入金额可以叠加多次，加起来显示买入金额。平仓也是如此」。
//
// 这组测试守的就是「同一根 K 线只画一笔 + 金额累加」。
// 图画错了不会报错、也没有日志 —— 只能靠断言把它钉住。
//
// snap 用真实的 1 分钟对齐逻辑（毫秒 → 对齐到分钟 → 转秒），
// 而不是写个恒等函数：时间对齐错了的话标记会落在两根 K 线中间，
// 图表整套丢弃，表现就是「一个标记都不显示」。

const minute = int64(60000)

func snapMin(tsMs int64) int64 {
	if tsMs <= 0 {
		return 0
	}
	return (tsMs - tsMs%minute) / 1000
}

func ev(kind string, tsMs int64, px, sz, margin, pnl, pnlPct float64) repo.TradeEventPoint {
	return repo.TradeEventPoint{
		ID: tsMs, Kind: kind, Ts: tsMs, Px: px, Sz: sz,
		Margin: margin, Pnl: pnl, PnlPct: pnlPct, Leverage: 20, Reason: kind + "-reason",
	}
}

// A. 同一根 K 线两笔买入 → 合并成一笔，金额累加。
func TestAggregateEvents_SameBarBuysMerge(t *testing.T) {
	// 刻意选一个**整分钟对齐**的时刻：不对齐的话 base 与 base+40s 会落在
	// 相邻两根 K 线上，「同一根」的用例会写成假阳性。
	base := int64(1760000040000)
	evs := []repo.TradeEventPoint{
		ev("open", base+1000, 100.0, 1, 0.10, 0, 0),
		ev("open", base+20000, 101.5, 2, 0.20, 0, 0),
	}
	got := aggregateEvents(evs, snapMin)
	if len(got) != 1 {
		t.Fatalf("同一根 K 线的两笔买入应合并成 1 笔，得到 %d 笔", len(got))
	}
	c := got[0]
	if c.Count != 2 {
		t.Errorf("Count 应为 2，实际 %d", c.Count)
	}
	if math.Abs(c.Margin-0.30) > 1e-9 {
		t.Errorf("买入金额应累加为 0.30，实际 %.6f", c.Margin)
	}
	if math.Abs(c.Sz-3) > 1e-9 {
		t.Errorf("张数应累加为 3，实际 %.6f", c.Sz)
	}
	if math.Abs(c.Px-101.5) > 1e-9 {
		t.Errorf("价格应取最后一笔 101.5，实际 %.6f", c.Px)
	}
	if c.Time != snapMin(base+20000) {
		t.Errorf("时间应为对齐后的秒级时间 %d，实际 %d", snapMin(base+20000), c.Time)
	}
	if c.Ts != base+20000 {
		t.Errorf("Ts 应取最后一笔的毫秒时间 %d，实际 %d", base+20000, c.Ts)
	}
}

// B. 同一根 K 线上的「买入」和「加仓」不互相合并（语义不同，各自一条）。
func TestAggregateEvents_OpenAndAddonStaySeparate(t *testing.T) {
	base := int64(1760000040000) // 整分钟对齐，便于推算「同一根 K 线」
	evs := []repo.TradeEventPoint{
		ev("open", base+1000, 100, 1, 0.10, 0, 0),
		ev("addon", base+5000, 100, 1, 0.03, 0, 0),
	}
	got := aggregateEvents(evs, snapMin)
	if len(got) != 2 {
		t.Fatalf("开仓与加仓语义不同，应各出一条（共 2），得到 %d", len(got))
	}
	kinds := map[string]float64{}
	for _, c := range got {
		kinds[c.Kind] = c.Margin
	}
	if math.Abs(kinds["open"]-0.10) > 1e-9 || math.Abs(kinds["addon"]-0.03) > 1e-9 {
		t.Errorf("金额被串了：%v", kinds)
	}
}

// C. 不同 K 线的买入保持各自一笔。
func TestAggregateEvents_DifferentBarsKeepSeparate(t *testing.T) {
	base := int64(1760000040000) // 整分钟对齐，便于推算「同一根 K 线」
	evs := []repo.TradeEventPoint{
		ev("open", base, 100, 1, 0.10, 0, 0),
		ev("open", base+minute, 100, 1, 0.10, 0, 0),
		ev("open", base+2*minute, 100, 1, 0.10, 0, 0),
	}
	got := aggregateEvents(evs, snapMin)
	if len(got) != 3 {
		t.Fatalf("三根不同的 K 线应得到 3 笔，实际 %d", len(got))
	}
	for _, c := range got {
		if c.Count != 1 {
			t.Errorf("不应发生合并，Count 实际 %d", c.Count)
		}
	}
}

// D. 同一根 K 线多笔平仓 → 盈亏累加，收益率改用「合计盈亏 ÷ 合计保证金」。
func TestAggregateEvents_ClosePnlSumsAndPctIsWeighted(t *testing.T) {
	base := int64(1760000040000) // 整分钟对齐，便于推算「同一根 K 线」
	evs := []repo.TradeEventPoint{
		ev("close", base+1000, 100, 1, 0.10, 0.01, 10.0),
		ev("close", base+30000, 100, 1, 0.30, 0.03, 10.0),
	}
	got := aggregateEvents(evs, snapMin)
	if len(got) != 1 {
		t.Fatalf("同一根 K 线的两笔平仓应合并成 1 笔，得到 %d", len(got))
	}
	c := got[0]
	if math.Abs(c.Pnl-0.04) > 1e-9 {
		t.Errorf("盈亏应累加为 0.04，实际 %.6f", c.Pnl)
	}
	if math.Abs(c.Margin-0.40) > 1e-9 {
		t.Errorf("保证金应累加为 0.40，实际 %.6f", c.Margin)
	}
	// 0.04 / 0.40 * 100 = 10%
	if math.Abs(c.PnlPct-10.0) > 1e-6 {
		t.Errorf("多笔合并后收益率应为「合计盈亏÷合计保证金」=10%%，实际 %.4f", c.PnlPct)
	}
}

// E. 单笔平仓必须原样保留 pnl_pct（不能被「加权」改写成别的数）。
func TestAggregateEvents_SingleCloseKeepsPnlPct(t *testing.T) {
	base := int64(1760000040000) // 整分钟对齐，便于推算「同一根 K 线」
	evs := []repo.TradeEventPoint{ev("close", base+1000, 100, 1, 0.10, 0.0077, 7.7)}
	got := aggregateEvents(evs, snapMin)
	if len(got) != 1 || got[0].Count != 1 {
		t.Fatalf("应为单笔，实际 %+v", got)
	}
	if math.Abs(got[0].PnlPct-7.7) > 1e-9 {
		t.Errorf("单笔应保留原始 pnl_pct 7.7，实际 %.6f", got[0].PnlPct)
	}
}

// F. snap 判定为无效（<=0）的事件必须丢弃 —— 否则图表会整套丢弃标记。
func TestAggregateEvents_DropsInvalidTime(t *testing.T) {
	base := int64(1760000040000) // 整分钟对齐，便于推算「同一根 K 线」
	evs := []repo.TradeEventPoint{
		ev("open", base, 100, 1, 0.10, 0, 0),
		ev("open", 0, 100, 1, 0.10, 0, 0),
		ev("open", -5, 100, 1, 0.10, 0, 0),
	}
	got := aggregateEvents(evs, snapMin)
	if len(got) != 1 {
		t.Fatalf("时间脏数据应被丢弃，只剩 1 笔，实际 %d", len(got))
	}
	if got[0].Count != 1 {
		t.Errorf("被丢弃的事件不应计入 Count，实际 %d", got[0].Count)
	}
}

// G. 空输入返回 nil（前端 markerKey 去重依赖「没有就不 push」）。
func TestAggregateEvents_Empty(t *testing.T) {
	if got := aggregateEvents(nil, snapMin); got != nil {
		t.Errorf("nil 输入应返回 nil，实际 %v", got)
	}
	if got := aggregateEvents([]repo.TradeEventPoint{}, snapMin); got != nil {
		t.Errorf("空切片应返回 nil，实际 %v", got)
	}
}

// H. 价格取「时间上最后一笔」—— 即使输入顺序被打乱。
func TestAggregateEvents_PriceFromLatestEvent(t *testing.T) {
	base := int64(1760000040000) // 整分钟对齐，便于推算「同一根 K 线」
	// 故意乱序：最后一笔（base+40000，价 105）放在最前面
	evs := []repo.TradeEventPoint{
		ev("open", base+40000, 105, 1, 0.10, 0, 0),
		ev("open", base+1000, 100, 1, 0.10, 0, 0),
	}
	got := aggregateEvents(evs, snapMin)
	if len(got) != 1 {
		t.Fatalf("应合并为 1 笔，实际 %d", len(got))
	}
	if math.Abs(got[0].Px-105) > 1e-9 {
		t.Errorf("价格应取时间最晚的那笔 105，实际 %.6f", got[0].Px)
	}
	if got[0].Ts != base+40000 {
		t.Errorf("Ts 应取最晚的 %d，实际 %d", base+40000, got[0].Ts)
	}
}

// I. 输出按「首次出现顺序」排列（输入是 ts 升序，所以输出也是时间升序）。
func TestAggregateEvents_OutputOrderIsStable(t *testing.T) {
	base := int64(1760000040000) // 整分钟对齐，便于推算「同一根 K 线」
	evs := []repo.TradeEventPoint{
		ev("open", base, 100, 1, 0.10, 0, 0),
		ev("close", base+minute, 101, 1, 0.10, 0.01, 10),
		ev("open", base+2*minute, 102, 1, 0.10, 0, 0),
		ev("close", base+2*minute, 102, 1, 0.10, 0.02, 20),
	}
	got := aggregateEvents(evs, snapMin)
	// 3 个时间点：第 1、2 个点只有 open，第 3 个点上 open 与 close 各成一条 → 共 4 条
	if len(got) != 4 {
		t.Fatalf("应为 4 条（前两根各 1 条，最后一根 open+close 各 1 条），实际 %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Time < got[i-1].Time {
			t.Fatalf("输出未按时间升序：%d 出现在 %d 之后", got[i].Time, got[i-1].Time)
		}
	}
	// 最后两条同属最后一根 K 线，顺序应与输入一致（open 先、close 后）
	wantLast := snapMin(base + 2*minute)
	if got[2].Time != wantLast || got[3].Time != wantLast {
		t.Fatalf("最后两条应属于同一根 K 线（%d），实际 %d / %d", wantLast, got[2].Time, got[3].Time)
	}
	if got[2].Kind != "open" || got[3].Kind != "close" {
		t.Errorf("同一根上的输出顺序应与输入一致（open 先），实际 %s / %s", got[2].Kind, got[3].Kind)
	}
	if got[2].Count != 1 || got[3].Count != 1 {
		t.Errorf("不同 kind 不能被合并，Count 应为 1/1，实际 %d/%d", got[2].Count, got[3].Count)
	}
}
