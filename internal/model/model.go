package model

// model.go —— 模型层：全项目共享的实体 / DTO。
//
// 这一层不做任何 IO，也没有任何业务逻辑，只是把「数据长什么样」定义清楚，
// 让 repo（数据层）、service（业务层）、handler（接口层）三方共用同一套结构，
// 避免同一个概念在不同层各定义一遍。
//
// 注意：service 包内部还有一组「交易侧」的 Instrument / Ticker（带 LotSzDec、
// 由 OKXClient 直接构造）。那组和这里的 Instrument / Ticker 用途不同，
// 故意分开：这里的是「落库 + 给前端」的宽结构。

import "encoding/json"

// ---------------------------------------------------------------------------
// 行情
// ---------------------------------------------------------------------------

// Kline K 线（Web 侧，字段名给 TradingView 用）
type Kline struct {
	InstID string  `json:"inst"`
	Bar    string  `json:"bar"`
	Ts     int64   `json:"ts"`
	O      float64 `json:"o"`
	H      float64 `json:"h"`
	L      float64 `json:"l"`
	C      float64 `json:"c"`
	V      float64 `json:"v"`
}

// KlineRow K 线（引擎侧，列名与表字段一一对应）
type KlineRow struct {
	InstID string  `json:"inst_id"`
	Bar    string  `json:"bar"`
	Ts     int64   `json:"ts"`
	O      float64 `json:"o"`
	H      float64 `json:"h"`
	L      float64 `json:"l"`
	C      float64 `json:"c"`
	V      float64 `json:"v"`
}

// KlineQuery K 线查询条件
type KlineQuery struct {
	InstID string
	Bar    string
	FromTs int64 // 含
	ToTs   int64 // 含，0 表示不限
	Limit  int   // 从最新往回取 N 根，0 表示不限
}

// KlineCoverage 某合约某周期的数据覆盖情况
type KlineCoverage struct {
	InstID string  `json:"instId"`
	Bar    string  `json:"bar"`
	Count  int64   `json:"count"`
	MinTs  int64   `json:"minTs"`
	MaxTs  int64   `json:"maxTs"`
	Days   float64 `json:"days"`
}

// Instrument 合约信息（落库 + 给前端展示用）
type Instrument struct {
	InstID      string  `json:"instId"`
	BaseCcy     string  `json:"baseCcy"`
	QuoteCcy    string  `json:"quoteCcy"`
	SettleCcy   string  `json:"settleCcy"`
	CtVal       float64 `json:"ctVal"`
	CtMult      float64 `json:"ctMult"`
	LotSz       float64 `json:"lotSz"`
	MinSz       float64 `json:"minSz"`
	TickSz      float64 `json:"tickSz"`
	Lever       int     `json:"lever"`
	State       string  `json:"state"`
	ListTime    int64   `json:"listTime"`
	QuoteVol24h float64 `json:"quoteVol24h"`
	UpdatedAt   int64   `json:"updatedAt"`

	// InstCategory OKX 品种分类：1=加密 3=美股/ETF 4=商品(黄金原油等)
	// 「不买 ETF 和美股」就是靠这个字段过滤
	InstCategory string `json:"instCategory"`

	// Tradeable 是否通过准入过滤（1=可交易，0=被排除）
	Tradeable int `json:"tradeable"`
	// ExcludeReason 被排除的原因（stock_etf / new_listing / delisting / notional / state / manual）
	ExcludeReason string `json:"excludeReason"`
}

// Ticker 行情快照
type Ticker struct {
	InstID      string  `json:"instId"`
	Ts          int64   `json:"ts"`
	Last        float64 `json:"last"`
	Open24h     float64 `json:"open24h"`
	High24h     float64 `json:"high24h"`
	Low24h      float64 `json:"low24h"`
	Vol24h      float64 `json:"vol24h"`
	VolCcy24h   float64 `json:"volCcy24h"`
	QuoteVol24h float64 `json:"quoteVol24h"`
	ChgPct      float64 `json:"chgPct"`
}

// ---------------------------------------------------------------------------
// 交易
// ---------------------------------------------------------------------------

// OpenPosition 在持仓（trade 表 status='open'）
// OpenPosition 在持仓（前端展示）
type OpenPosition struct {
	ID       int64   `json:"id"`
	InstID   string  `json:"instId"`
	Side     string  `json:"side"`
	Sz       float64 `json:"sz"`
	EntryPx  float64 `json:"entryPx"`
	Margin   float64 `json:"margin"`
	Leverage int     `json:"leverage"`
	OpenTs   int64   `json:"openTs"`
	Bar      string  `json:"bar"`
	Score    int     `json:"score"`
	AINote   string  `json:"aiNote"`

	// 加仓：次数 / 累计加仓保证金 / 最近一次加仓时间
	AddonCount  int     `json:"addonCount"`
	AddonMargin float64 `json:"addonMargin"`
	LastAddonTs int64   `json:"lastAddonTs"`
}

// ClosedTrade 历史仓位
//
// 名字里的 Closed 是历史遗留：现在这张表结构同时承载「持仓中」的仓位
// （status=open，exit_px/close_ts 为 0），前端历史列表把两类一起展示，
// 持仓中的行由前端用实时行情补上浮盈。所以额外带一个 Status 字段。
type ClosedTrade struct {
	ID       int64   `json:"id"`
	InstID   string  `json:"instId"`
	Side     string  `json:"side"`
	Sz       float64 `json:"sz"`
	EntryPx  float64 `json:"entryPx"`
	ExitPx   float64 `json:"exitPx"`
	Margin   float64 `json:"margin"`
	Leverage int     `json:"leverage"`
	OpenTs   int64   `json:"openTs"`
	CloseTs  int64   `json:"closeTs"`
	Pnl      float64 `json:"pnl"`
	PnlPct   float64 `json:"pnlPct"`
	Reason   string  `json:"reason"`
	Bar      string  `json:"bar"`
	AINote   string  `json:"aiNote"`
	Status   string  `json:"status"`
}

// TradeRow 开仓成交（引擎侧写入）
type TradeRow struct {
	InstID   string  `json:"inst_id"`
	Side     string  `json:"side"`
	Sz       float64 `json:"sz"`
	EntryPx  float64 `json:"entry_px"`
	Margin   float64 `json:"margin"`
	Leverage int     `json:"leverage"`
	OpenTs   int64   `json:"open_ts"`
	Score    int     `json:"score"`
	Bar      string  `json:"bar"`
	Reason   string  `json:"reason"`
	OrdID    string  `json:"ord_id"`
	Status   string  `json:"status"`
	AINote   string  `json:"ai_note"`
}

// CloseRow 平仓（引擎侧写入）
type CloseRow struct {
	ID      int64   `json:"id"`
	ExitPx  float64 `json:"exit_px"`
	Pnl     float64 `json:"pnl"`
	PnlPct  float64 `json:"pnl_pct"`
	Reason  string  `json:"reason"`
	CloseTs int64   `json:"close_ts"`
	OrdID   string  `json:"ord_id"`
}

// OpenPos 数据库里的在持仓记录（引擎侧读）
type OpenPos struct {
	ID       int64   `json:"id"`
	InstID   string  `json:"inst_id"`
	Sz       float64 `json:"sz"`
	EntryPx  float64 `json:"entry_px"`
	Margin   float64 `json:"margin"`
	Leverage int     `json:"leverage"`
	OpenTs   int64   `json:"open_ts"`
	Bar      string  `json:"bar"`
	Score    int     `json:"score"`
	AINote   string  `json:"ai_note"`

	// 加仓（浮亏补仓）相关：加了几次 / 累计加了多少保证金 / 上次加仓时间
	AddonCount  int     `json:"addon_count"`
	AddonMargin float64 `json:"addon_margin"`
	LastAddonTs int64   `json:"last_addon_ts"`
}

// AddonRow 一次加仓的结果，用来把原持仓行「合并」成新的均价 / 张数 / 保证金。
//
// 加仓不做成一条新的持仓行，而是把原行就地更新：
//
//	sz       = sz + addSz
//	entryPx  = 加权均价
//	margin   = margin + addMargin
//
// 这样出场逻辑（止盈 / 布林上轨）不用改也能拿到正确的均价。
type AddonRow struct {
	ID          int64   `json:"id"`
	Sz          float64 `json:"sz"`           // 合并后的总张数
	EntryPx     float64 `json:"entry_px"`     // 合并后的加权开仓均价
	Margin      float64 `json:"margin"`       // 合并后的总保证金
	AddSz       float64 `json:"add_sz"`       // 本次加仓张数
	AddPx       float64 `json:"add_px"`       // 本次加仓价格
	AddMargin   float64 `json:"add_margin"`   // 本次加仓保证金
	AddonCount  int     `json:"addon_count"`  // 累计加仓次数
	AddonMargin float64 `json:"addon_margin"` // 累计加仓保证金
	LastAddonTs int64   `json:"last_addon_ts"`
	OrdID       string  `json:"ord_id"`
	Reason      string  `json:"reason"`
}

// ---------------------------------------------------------------------------
// 信号
// ---------------------------------------------------------------------------

// SignalRow 信号（Web 侧读取展示）
type SignalRow struct {
	ID        int64   `json:"id"`
	InstID    string  `json:"instId"`
	Bar       string  `json:"bar"`
	Ts        int64   `json:"ts"`
	Close     float64 `json:"close"`
	Mask      int     `json:"mask"`
	Score     int     `json:"score"`
	HitList   string  `json:"hitList"`
	Rsi       float64 `json:"rsi"`
	Td        int     `json:"td"`
	Acted     int     `json:"acted"`
	Reason    string  `json:"reason"`
	AINote    string  `json:"aiNote"`
	CreatedAt int64   `json:"createdAt"`
}

// EngineSignalRow 信号（引擎侧写入，含 8 因子明细）
type EngineSignalRow struct {
	InstID    string  `json:"inst_id"`
	Bar       string  `json:"bar"`
	Ts        int64   `json:"ts"`
	Close     float64 `json:"close"`
	Mask      int     `json:"mask"`
	Score     int     `json:"score"`
	HitList   string  `json:"hit_list"`
	Pot       float64 `json:"pot"`
	Fri       float64 `json:"fri"`
	Kin       float64 `json:"kin"`
	Rsi       float64 `json:"rsi"`
	Td        int     `json:"td"`
	Acted     int     `json:"acted"` // 0=只记信号 1=已下单 2=被风控拦
	Reason    string  `json:"reason"`
	AINote    string  `json:"ai_note"`
	CreatedAt int64   `json:"created_at"`
}

// SignalUpdate 对已存在信号行的补充更新（acted / reason / ai_note）
//
// 单独一类而不是覆盖整行：INSERT OR REPLACE 会把 AI 点评和下单状态抹掉。
type SignalUpdate struct {
	InstID string `json:"inst_id"`
	Bar    string `json:"bar"`
	Ts     int64  `json:"ts"`
	Acted  int    `json:"acted"`
	Reason string `json:"reason"`
	AINote string `json:"ai_note"`
}

// ---------------------------------------------------------------------------
// 权益 / 统计
// ---------------------------------------------------------------------------

// EquityRow 权益快照
type EquityRow struct {
	Ts       int64   `json:"ts"`
	TotalEq  float64 `json:"total_eq"`
	Avail    float64 `json:"avail"`
	Upl      float64 `json:"upl"`
	PosCount int     `json:"pos_count"`
}

// Counters 风控要用的当日统计
type Counters struct {
	OrdersToday       int              `json:"orders_today"`
	SignalsToday      int              `json:"signals_today"`
	ClosedToday       int              `json:"closed_today"`
	WinsToday         int              `json:"wins_today"`
	TodayPnl          float64          `json:"today_pnl"`
	ConsecutiveLosses int              `json:"consecutive_losses"`
	LastEntryTs       map[string]int64 `json:"last_entry_ts"`
}

// Stats 给前端顶部条用的汇总
type Stats struct {
	ServerTime   string  `json:"serverTime"`
	Ts           int64   `json:"ts"`
	InstCount    int64   `json:"instCount"`
	KlineRows    int64   `json:"klineRows"`
	SignalsToday int64   `json:"signalsToday"`
	OrdersToday  int64   `json:"ordersToday"`
	PosCount     int64   `json:"posCount"`
	TradesTotal  int64   `json:"tradesTotal"`
	TodayPnl     float64 `json:"todayPnl"`
	PnlTotal     float64 `json:"pnlTotal"`
	WinRate      float64 `json:"winRate"`
}

// ---------------------------------------------------------------------------
// 回补任务
// ---------------------------------------------------------------------------

// BackfillJob 回补进度
type BackfillJob struct {
	InstID    string `json:"instId"`
	Bar       string `json:"bar"`
	FromTs    int64  `json:"fromTs"`
	ToTs      int64  `json:"toTs"`
	Rows      int64  `json:"rows"`
	Status    string `json:"status"`
	Msg       string `json:"msg"`
	UpdatedAt int64  `json:"updatedAt"`
}

// ---------------------------------------------------------------------------
// 落库批次
// ---------------------------------------------------------------------------

// StorePayload 一批要写库的数据
type StorePayload struct {
	Kline        []KlineRow        `json:"kline,omitempty"`
	Signal       []EngineSignalRow `json:"signal,omitempty"`
	SignalUpdate []SignalUpdate    `json:"signal_update,omitempty"`
	Trade        []TradeRow        `json:"trade,omitempty"`
	Equity       []EquityRow       `json:"equity,omitempty"`
	Runlog       []RunLogRow       `json:"runlog,omitempty"`
	CloseTrade   *CloseRow         `json:"close_trade,omitempty"`
}

// RunLogRow 一条运行日志（写进 SQLite 的 runlog 表）
type RunLogRow struct {
	Ts    int64  `json:"ts"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

// MarshalJSONString 调试用：把整批数据打成一行 JSON
func (p StorePayload) MarshalJSONString() string {
	b, err := json.Marshal(p)
	if err != nil {
		return "{}"
	}
	return string(b)
}
