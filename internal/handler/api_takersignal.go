package handler

// api_takersignal.go —— taker 买卖比 MACD 的买入信号接口（网页声音提醒的数据源）
//
// 用户口径（2026-10-03）：
//   「盘中有信号就实时提醒买入，买入条件就是 macd>0 and ref macd<0
//    refref macd<0」（原话是「发送邮箱…电脑端软件放声音」，随后改为
//    「不要客户端不要收邮件，就给我网页播放声音」）
//
// 所以链路是：
//   后台 TakerPanelRebuild → taker_signal 表（预计算，只落已收盘的根）
//   前端 signalalert.js 每 15 秒轮询本接口 → 有比「已看到水位线」更新的
//   → 扬琴音效 + 右上角横幅（网页出声，不需要任何客户端）
//
// ★ 为什么接口要支持 since（增量）而不是每次拉 30 天：
//   ① 30 天信号有几十上百条，每 15 秒传一遍纯属浪费（这机器带宽/内存都紧）；
//   ② 「有没有新信号」本质是「最大值有没有变大」，用增量语义最直接 ——
//      前端记一个 seenTs，请求 ts>seenTs 即可，返回空数组就等于没信号。
//
// ★ 为什么首屏（since 不传）返回的是**最新** limit 条而不是最早的：
//   前端拿它只为初始化 seenTs（不能一打开网页就把 30 天前的信号全播一遍）。

import (
	"net/http"
	"time"

	"finally-main/internal/service"
)

// takerSignalItem 一条信号（= 一次「要求买入」）
type takerSignalItem struct {
	Ts        int64   `json:"ts"`
	Val       float64 `json:"val"`   // 触发那根的 MACD 值（>0）
	Prev      float64 `json:"prev"`  // 前一根（<0）
	Prev2     float64 `json:"prev2"` // 前前一根（<0）
	Ratio     float64 `json:"ratio"` // 触发那根的买卖比
	EthRise   float64 `json:"eth_rise"`
	EthRiseOK bool    `json:"eth_rise_ok"`
}

// handleTakerSignal GET /api/takersignal?since=&rule=&limit=
//
//	since > 0  只回 (since, now] 的信号（升序）—— 前端轮询用
//	since 缺省 回最新 limit 条 —— 前端首屏用（初始化水位线）
//	rule        dif / dea / hist，默认 hist（详见 service/taker_signal.go）
func (s *Server) handleTakerSignal(w http.ResponseWriter, r *http.Request) (any, error) {
	q := r.URL.Query()
	rule := service.NormalizeTakerSignalRule(q.Get("rule"))
	since := int64(atoiDefault(q.Get("since"), 0))
	limit := atoiDefault(q.Get("limit"), 50)
	if limit <= 0 || limit > service.TakerSignalMaxLimit {
		limit = 50
	}

	rows, err := s.db.QueryTakerSignals(service.TakerPanelMacdBar, rule, since, 0, limit)
	if err != nil {
		return nil, err
	}
	items := make([]takerSignalItem, 0, len(rows))
	var newest int64
	for _, m := range rows {
		if m.Ts > newest {
			newest = m.Ts
		}
		items = append(items, takerSignalItem{
			Ts: m.Ts, Val: m.Val, Prev: m.Prev, Prev2: m.Prev2, Ratio: m.Ratio,
			EthRise: m.EthRise, EthRiseOK: m.EthRiseOK,
		})
	}

	// 覆盖情况：让前端能显示「库里有多少条 / 到哪一根」。
	// 查不到（表刚建、还没重算）不算错误，返回 0 即可。
	var covMin, covMax, covCnt int64
	if a, b, c, cerr := s.db.TakerSignalRange(service.TakerPanelMacdBar, rule); cerr == nil {
		covMin, covMax, covCnt = a, b, c
	}

	return map[string]any{
		"ok":       true,
		"bar":      service.TakerPanelMacdBar,
		"rule":     rule,
		"rules":    service.TakerSignalRules(),
		"since":    since,
		"count":    len(items),
		"newest":   newest,
		"now":      time.Now().UnixMilli(),
		"source":   "taker_macd." + rule,
		"fast":     service.TakerMacdFast(),
		"slow":     service.TakerMacdSlow(),
		"signal":   service.TakerMacdSignal(),
		"coverage": map[string]any{"min": covMin, "max": covMax, "count": covCnt},
		"signals":  items,
	}, nil
}
