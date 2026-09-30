package okx

// trade.go —— 原 okx/Trade_api.py 的 Go 版（TradeAPI）

// TradeAPI 交易接口。对应 Python 的 TradeAPI。
type TradeAPI struct {
	*Client
}

// NewTradeAPI 对应 TradeAPI(...)
func NewTradeAPI(apiKey, apiSecretKey, passphrase string, useServerTime bool, flag string) *TradeAPI {
	return &TradeAPI{NewClient(apiKey, apiSecretKey, passphrase, useServerTime, flag)}
}

// NewTradeAPIWith 带代理 / 超时配置
func NewTradeAPIWith(apiKey, apiSecretKey, passphrase string, useServerTime bool, flag string, opts ClientOptions) *TradeAPI {
	return &TradeAPI{NewClientWithOptions(apiKey, apiSecretKey, passphrase, useServerTime, flag, opts)}
}

// PlaceOrderParams 下单参数。对应 Python 的 place_order 一长串关键字参数。
//
// 只有 InstID / TdMode / Side / OrdType / Sz 是必填，其余留空即不发送。
type PlaceOrderParams struct {
	InstID     string
	TdMode     string
	Side       string
	OrdType    string
	Sz         string
	Ccy        string
	ClOrdID    string
	Tag        string
	PosSide    string
	Px         string
	ReduceOnly string
}

// toParams 转成请求参数（空值会被 Encode / jsonMarshalNoEscape 自动丢掉）
func (o PlaceOrderParams) toParams() Params {
	return Params{
		"instId": o.InstID, "tdMode": o.TdMode, "side": o.Side, "ordType": o.OrdType,
		"sz": o.Sz, "ccy": o.Ccy, "clOrdId": o.ClOrdID, "tag": o.Tag,
		"posSide": o.PosSide, "px": o.Px, "reduceOnly": o.ReduceOnly,
	}
}

// PlaceOrder 下单。对应 place_order(instId, tdMode, side, ordType, sz, ccy=”, ...)
func (t *TradeAPI) PlaceOrder(o PlaceOrderParams) (map[string]any, error) {
	return t.RequestWithParams(POST, PlaceOrder, o.toParams())
}

// PlaceMultipleOrders 批量下单。对应 place_multiple_orders(orders_data)
//
// orders_data 是数组，不能走 Params（那是对象），所以单独发。
func (t *TradeAPI) PlaceMultipleOrders(orders []PlaceOrderParams) (map[string]any, error) {
	arr := make([]Params, 0, len(orders))
	for _, o := range orders {
		arr = append(arr, o.toParams())
	}
	return t.postArray(BatchOrders, arr)
}

// CancelOrder 撤单。对应 cancel_order(instId, ordId=”, clOrdId=”)
func (t *TradeAPI) CancelOrder(instId, ordId, clOrdId string) (map[string]any, error) {
	return t.RequestWithParams(POST, CancelOrder, Params{"instId": instId, "ordId": ordId, "clOrdId": clOrdId})
}

// CancelMultipleOrders 批量撤单。对应 cancel_multiple_orders(orders_data)
func (t *TradeAPI) CancelMultipleOrders(orders []Params) (map[string]any, error) {
	return t.postArray(CancelBatchOrders, orders)
}

// AmendOrder 改单。对应 amend_order(...)
func (t *TradeAPI) AmendOrder(instId, cxlOnFail, ordId, clOrdId, reqId, newSz, newPx string) (map[string]any, error) {
	return t.RequestWithParams(POST, AmendOrder, Params{
		"instId": instId, "cxlOnFail": cxlOnFail, "ordId": ordId, "clOrdId": clOrdId,
		"reqId": reqId, "newSz": newSz, "newPx": newPx,
	})
}

// AmendMultipleOrders 批量改单。对应 amend_multiple_orders(orders_data)
func (t *TradeAPI) AmendMultipleOrders(orders []Params) (map[string]any, error) {
	return t.postArray(AmendBatchOrders, orders)
}

// ClosePositions 市价全平。对应 close_positions(instId, mgnMode, posSide=”, ccy=”)
func (t *TradeAPI) ClosePositions(instId, mgnMode, posSide, ccy string) (map[string]any, error) {
	return t.RequestWithParams(POST, ClosePosition, Params{
		"instId": instId, "mgnMode": mgnMode, "posSide": posSide, "ccy": ccy,
	})
}

// GetOrders 订单详情。对应 get_orders(instId, ordId=”, clOrdId=”)
func (t *TradeAPI) GetOrders(instId, ordId, clOrdId string) (map[string]any, error) {
	return t.RequestWithParams(GET, OrderInfo, Params{"instId": instId, "ordId": ordId, "clOrdId": clOrdId})
}

// GetOrderList 未成交订单。对应 get_order_list(...)
func (t *TradeAPI) GetOrderList(instType, uly, instId, ordType, state, after, before, limit string) (map[string]any, error) {
	return t.RequestWithParams(GET, OrdersPending, Params{
		"instType": instType, "uly": uly, "instId": instId, "ordType": ordType,
		"state": state, "after": after, "before": before, "limit": limit,
	})
}

// GetOrdersHistory 历史订单（近 7 天）。对应 get_orders_history(...)
func (t *TradeAPI) GetOrdersHistory(instType, uly, instId, ordType, state, after, before, limit string) (map[string]any, error) {
	return t.RequestWithParams(GET, OrdersHistory, Params{
		"instType": instType, "uly": uly, "instId": instId, "ordType": ordType,
		"state": state, "after": after, "before": before, "limit": limit,
	})
}

// OrdersHistoryArchive 历史订单（近 3 个月）。对应 orders_history_archive(...)
func (t *TradeAPI) OrdersHistoryArchive(instType, uly, instId, ordType, state, after, before, limit string) (map[string]any, error) {
	return t.RequestWithParams(GET, OrdersHistoryArch, Params{
		"instType": instType, "uly": uly, "instId": instId, "ordType": ordType,
		"state": state, "after": after, "before": before, "limit": limit,
	})
}

// GetFills 成交明细。对应 get_fills(...)
func (t *TradeAPI) GetFills(instType, uly, instId, ordId, after, before, limit string) (map[string]any, error) {
	return t.RequestWithParams(GET, OrderFills, Params{
		"instType": instType, "uly": uly, "instId": instId, "ordId": ordId,
		"after": after, "before": before, "limit": limit,
	})
}

// AlgoOrderParams 策略委托参数。对应 place_algo_order 的关键字参数。
type AlgoOrderParams struct {
	InstID      string
	TdMode      string
	Side        string
	OrdType     string
	Sz          string
	Ccy         string
	PosSide     string
	ReduceOnly  string
	TpTriggerPx string
	TpOrdPx     string
	SlTriggerPx string
	SlOrdPx     string
	TriggerPx   string
	OrderPx     string
}

func (o AlgoOrderParams) toParams() Params {
	return Params{
		"instId": o.InstID, "tdMode": o.TdMode, "side": o.Side, "ordType": o.OrdType, "sz": o.Sz,
		"ccy": o.Ccy, "posSide": o.PosSide, "reduceOnly": o.ReduceOnly,
		"tpTriggerPx": o.TpTriggerPx, "tpOrdPx": o.TpOrdPx,
		"slTriggerPx": o.SlTriggerPx, "slOrdPx": o.SlOrdPx,
		"triggerPx": o.TriggerPx, "orderPx": o.OrderPx,
	}
}

// PlaceAlgoOrder 策略委托。对应 place_algo_order(...)
func (t *TradeAPI) PlaceAlgoOrder(o AlgoOrderParams) (map[string]any, error) {
	return t.RequestWithParams(POST, PlaceAlgoOrder, o.toParams())
}

// CancelAlgoOrder 撤销策略委托。对应 cancel_algo_order(params)
func (t *TradeAPI) CancelAlgoOrder(params Params) (map[string]any, error) {
	return t.RequestWithParams(POST, CancelAlgos, params)
}

// OrderAlgosList 未触发策略委托。对应 order_algos_list(...)
func (t *TradeAPI) OrderAlgosList(ordType, algoId, instType, instId, after, before, limit string) (map[string]any, error) {
	return t.RequestWithParams(GET, OrdersAlgoPending, Params{
		"ordType": ordType, "algoId": algoId, "instType": instType, "instId": instId,
		"after": after, "before": before, "limit": limit,
	})
}

// OrderAlgosHistory 策略委托历史。对应 order_algos_history(...)
func (t *TradeAPI) OrderAlgosHistory(ordType, state, algoId, instType, instId, after, before, limit string) (map[string]any, error) {
	return t.RequestWithParams(GET, OrdersAlgoHistory, Params{
		"ordType": ordType, "state": state, "algoId": algoId, "instType": instType,
		"instId": instId, "after": after, "before": before, "limit": limit,
	})
}

// postArray 发 JSON 数组 body 的 POST（批量订单类接口）。
//
// Params 是 map，装不下数组，这里直接走底层 RequestJSON。
func (t *TradeAPI) postArray(path string, arr []Params) (map[string]any, error) {
	return t.RequestJSON(POST, path, arr)
}
