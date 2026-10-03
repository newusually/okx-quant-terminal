package handler

// api_nqchart.go —— 第二张图：NQ（纳斯达克指数期货）5 分钟 K 线 + 对齐后的 taker MACD
//
// 用户口径（2026-10-03 二十二·三期）：
//   「K线图下面再给我做个K线图，但是这个K线图的数据有要求就是必须是 duck* 的
//    30 天的 K 线图，其中 K 线图的 K 线柱子必须是 NQ 也就是纳斯达克指数期货的
//    5 分钟数据，下面要放上刚刚上面 K 线图的 macd 数据，而不是 NQ 的 macd 数据，
//    而且时间要对齐；周末时间因为纳斯达克指数无法交易所以不交易的时候请还是要
//    实时更新数据，就是不显示不交易的 K 线图；交易的时间的数据请对齐 macd，
//    并且保存在数据库这样读取直接数据库进行，更新数据也在数据库进行。」
//
// 拆成三条硬约束：
//   ① 柱子 = NQ 5 分钟（数据源 Dukascopy，落 kline 表 inst_id=NQ-INDEX）
//      —— 不实时外部查询，读库即用（更新由 StartNQSync / StartNQIntraday 写库）
//   ② 副图 = **主图那份** taker 买卖比 MACD(12,26,60)（读 taker_macd 表），
//      不是 NQ 自己的 MACD
//   ③ 两块严格同轴：MACD 只保留「NQ 确实有柱子」的那些时间戳
//      —— 纳指周末 + 每日维护时段不交易，那段时间 K 线本来就不存在；
//         若把 24 小时的 MACD 原样铺上去，副图会比柱子长出一截、两边对不上。
//
// ★ 为什么不新增表：NQ 的 5m 数据在 kline 表里（与加密同一张表、同一个
//   KlineQuery 通道，分区/保留期/清理都自动复用），MACD 在 taker_macd 表里。
//   本接口是**纯读**接口，两张表的写入分别由 NQ 同步链路与 TakerPanelRebuild 负责。

import (
	"fmt"
	"net/http"
	"time"

	"finally-main/internal/model"
	"finally-main/internal/service"
)

// nqChartCandle 一根 NQ 5m 蜡烛（字段名与前端 chart 的 toCandle 对齐）
type nqChartCandle struct {
	Ts int64   `json:"ts"`
	O  float64 `json:"o"`
	H  float64 `json:"h"`
	L  float64 `json:"l"`
	C  float64 `json:"c"`
	V  float64 `json:"v"`
}

// nqChartMacd 一个 MACD 点（与主图 /api/takermacd 同结构，前端可复用渲染）
type nqChartMacd struct {
	Ts   int64   `json:"ts"`
	Src  float64 `json:"src"`
	Dif  float64 `json:"dif"`
	Dea  float64 `json:"dea"`
	Hist float64 `json:"hist"`
}

// handleNQChart GET /api/nqchart?days=30
//
// 返回：
//
//	kline —— NQ 5m 蜡烛（升序；周末/休市天然没有行，时间轴不留空档）
//	macd  —— 主图那份 taker 买卖比 MACD，**已按 NQ 柱子时间戳过滤**（对齐）
//	coverage —— NQ 数据覆盖情况（首尾 ts / 根数 / 天数），空数据时前端据此提示
//	note  —— 口径说明（前端原样展示，避免「库里有但没画出来」时的误解）
func (s *Server) handleNQChart(w http.ResponseWriter, r *http.Request) (any, error) {
	q := r.URL.Query()
	days := atoiDefault(q.Get("days"), 30)
	if days <= 0 || days > 90 {
		days = 30
	}

	// 窗口右端对齐到 5m 边界再 +1 根，保证「正在走的那一根」也在窗口里
	toTs := time.Now().UnixMilli()/300000*300000 + 300000
	fromTs := toTs - int64(days)*86400*1000

	// ---- ① NQ 5m 柱子（读库，不实时外部查）----
	kl, err := s.db.QueryKlines(model.KlineQuery{
		InstID: service.NQInstID, Bar: "5m",
		FromTs: fromTs, ToTs: toTs, Asc: true,
	})
	if err != nil {
		return nil, err
	}
	// ★ 过滤「没在交易」的占位行（用户口径「不显示不交易的 K 线图」）：
	//   Dukascopy 在休市（周末、节假日、每日维护时段）当天没有数据文件，而
	//   NQ 同步为了时间轴连续会把上一根的收盘价前向填成一根
	//   开=高=低=收、成交量为 0 的平柱（实测 30 天 5760 根里 1755 根是这种）。
	//   这类根**必须照写库、照实时更新**（用户明确要求「不交易的时候还是要实时
	//   更新数据」，开盘那一刻才能立刻接上），但**不能画出来** —— 画出来就是
	//   一排横线把周末伪装成「有行情但不动」。
	//   判据只用 v>0：实测 v>0 的根数（4005）与非平柱根数完全相等，两个判据
	//   等价，取字段更直观（也顺带挡住「极安静但有量」的极小实体被误杀）。
	//   过滤后 lightweight-charts 按数据点排轴，周末自然被压掉、不留空档。
	candles := make([]nqChartCandle, 0, len(kl))
	hidden := 0
	for _, k := range kl {
		if !(k.V > 0) {
			hidden++
			continue
		}
		candles = append(candles, nqChartCandle{Ts: k.Ts, O: k.O, H: k.H, L: k.L, C: k.C, V: k.V})
	}

	// ---- ② 主图那份 taker MACD（读 taker_macd 预计算表）----
	macdRows, err := s.db.QueryTakerMacd(service.TakerPanelMacdBar, fromTs, toTs)
	if err != nil {
		return nil, err
	}

	// ---- ③ 对齐：只保留 NQ 有柱子的时间戳 ----
	//
	// 纳指周末休市 + 每日维护时段（约 21:00–22:00 UTC）无 K 线，而 MACD 是
	// 24 小时的。不做这一步，副图会比柱子多出整段周末 —— 两边不仅对不上，
	// 视觉上还会误导（看着像加密那边周末也在跟纳指联动）。
	have := make(map[int64]struct{}, len(candles))
	for _, c := range candles {
		have[c.Ts] = struct{}{}
	}
	macd := make([]nqChartMacd, 0, len(macdRows))
	for _, m := range macdRows {
		if _, ok := have[m.Ts]; !ok {
			continue
		}
		macd = append(macd, nqChartMacd{Ts: m.Ts, Src: m.SrcVal, Dif: m.Dif, Dea: m.Dea, Hist: m.Hist})
	}

	cov := map[string]any{
		"bars":  len(candles),
		"macd":  len(macd),
		"days":  days,
		"inst":  service.NQInstID,
		"bar":   "5m",
		"first": int64(0),
		"last":  int64(0),
		// 休市占位根：库里存在、但按口径不画（前端可显示「已隐藏 N 根休市」）
		"hidden": hidden,
		"raw":    len(kl),
	}
	if n := len(candles); n > 0 {
		cov["first"] = candles[0].Ts
		cov["last"] = candles[n-1].Ts
		cov["spanDays"] = float64(candles[n-1].Ts-candles[0].Ts) / 86400000.0
	}

	note := fmt.Sprintf("柱子 = NQ 纳斯达克100 5 分钟（Dukascopy 30 天，落 kline 表）；"+
		"副图 = 主图那份 taker 买卖比 MACD(12,26,60)，已按 NQ 交易时间对齐；"+
		"休市（周末 + 每日维护）的占位根照写库但不画（本窗口隐藏 %d 根）", hidden)

	return map[string]any{
		"ok":       true,
		"inst":     service.NQInstID,
		"name":     service.ReadonlyInstName(service.NQInstID),
		"bar":      "5m",
		"kline":    candles,
		"macd":     macd,
		"fast":     service.TakerMacdFast(),
		"slow":     service.TakerMacdSlow(),
		"signal":   service.TakerMacdSignal(),
		"coverage": cov,
		"note":     note,
	}, nil
}
