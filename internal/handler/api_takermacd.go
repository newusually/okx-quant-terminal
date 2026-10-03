package handler

// api_takermacd.go —— taker 买卖比的 MACD(12,26,60) 副图数据接口
//
// 用户口径（2026-10-03）：
//   「给一个指标，这个指标就是刚刚算的 5 分钟的 takervol 比例，
//    把比例放到 macd 指标里面，close 的值改成这个 takervol 值，
//    参数是 12 26 60，放到 K 线图上，就是 K 线的柱子下面 vol 值的下面，
//    要同步上面的 K 线柱子指标，5 分钟才能显示这个指标，就是副图」
//
// 所以：
//   · 输入序列 = 全池买卖比（taker_panel.ratio），不是价格
//   · 参数 = 12 / 26 / 60（★ 不是常见的 9）
//   · 位置 = K 线图内、VOL 之下（前端用独立 priceScale 叠在底部实现）
//   · 仅 5m 显示（前端按 bar 判定，后端也固定只发 5m）
//
// ★ 数据来源：taker_macd 预计算表（后台 TakerPanelRebuild 写入）。
//   与面板同一条 ratio 序列、同一份 emaInto 实现 —— 口径不会漂。
//
// ★ 时间轴对齐：返回的 ts 是 5m 切片起始毫秒（与 kline.ts 完全对齐），
//   前端按 ts 直接映射到图表的 time（秒），不需要任何插值。

import (
	"net/http"
	"time"

	"finally-main/internal/service"
)

// takerMacdPoint 一个点的 MACD 三件套
type takerMacdPoint struct {
	Ts     int64   `json:"ts"`
	SrcVal float64 `json:"src"`  // 输入值（买卖比）
	Dif    float64 `json:"dif"`  // DIF = EMA12 - EMA26
	Dea    float64 `json:"dea"`  // DEA = EMA(DIF, 60)
	Hist   float64 `json:"hist"` // 2*(DIF-DEA)
}

// handleTakerMacd GET /api/takermacd?from=&to=&limit=
//
// from / to：毫秒时间戳（可选）。前端按当前可视区间请求，翻页时重取。
// limit：不传 from/to 时，取最近 limit 根（默认 1000，上限 8640）。
func (s *Server) handleTakerMacd(w http.ResponseWriter, r *http.Request) (any, error) {
	q := r.URL.Query()
	fromTs := int64(atoiDefault(q.Get("from"), 0))
	toTs := int64(atoiDefault(q.Get("to"), 0))
	limit := atoiDefault(q.Get("limit"), 1000)
	if limit <= 0 || limit > 288*30 {
		limit = 288 * 30
	}

	if fromTs == 0 && toTs == 0 {
		// 没给区间 → 取最近 limit 根
		toTs = time.Now().UnixMilli()/300000*300000 + 300000
		fromTs = toTs - int64(limit+2)*300000
	}

	rows, err := s.db.QueryTakerMacd(service.TakerPanelMacdBar, fromTs, toTs)
	if err != nil {
		return nil, err
	}
	pts := make([]takerMacdPoint, 0, len(rows))
	for _, m := range rows {
		pts = append(pts, takerMacdPoint{
			Ts: m.Ts, SrcVal: m.SrcVal, Dif: m.Dif, Dea: m.Dea, Hist: m.Hist,
		})
	}

	// 参数回报给前端（图例上要写「MACD(12,26,60)」，
	// 写死在两处迟早不一致）
	return map[string]any{
		"ok":     true,
		"bar":    service.TakerPanelMacdBar,
		"fast":   service.TakerMacdFast(),
		"slow":   service.TakerMacdSlow(),
		"signal": service.TakerMacdSignal(),
		"source": "taker_panel.ratio",
		"count":  len(pts),
		"points": pts,
	}, nil
}
