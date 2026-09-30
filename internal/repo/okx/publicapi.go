package okx

// publicapi.go —— 原 okx/Public_api.py 的 Go 版（PublicAPI）

// PublicAPI 公共数据接口。对应 Python 的 PublicAPI。
type PublicAPI struct {
	*Client
}

// NewPublicAPI 对应 PublicAPI(...)
func NewPublicAPI(apiKey, apiSecretKey, passphrase string, useServerTime bool, flag string) *PublicAPI {
	return &PublicAPI{NewClient(apiKey, apiSecretKey, passphrase, useServerTime, flag)}
}

// NewPublicAPIWith 带代理 / 超时配置
func NewPublicAPIWith(apiKey, apiSecretKey, passphrase string, useServerTime bool, flag string, opts ClientOptions) *PublicAPI {
	return &PublicAPI{NewClientWithOptions(apiKey, apiSecretKey, passphrase, useServerTime, flag, opts)}
}

// NewPublicOnlyAPI 免密钥。合约列表 / 时间这类接口只需要这个。
func NewPublicOnlyAPI() *PublicAPI { return &PublicAPI{NewPublicClient()} }

// GetInstruments 合约列表。对应 get_instruments(instType, uly=”, instId=”)
func (p *PublicAPI) GetInstruments(instType, uly, instId string) (map[string]any, error) {
	return p.RequestWithParams(GET, InstrumentInfo, Params{"instType": instType, "uly": uly, "instId": instId})
}

// GetDeliverHistory 交割历史。对应 get_deliver_history(...)
func (p *PublicAPI) GetDeliverHistory(instType, uly, after, before, limit string) (map[string]any, error) {
	return p.RequestWithParams(GET, DeliveryExercise, Params{
		"instType": instType, "uly": uly, "after": after, "before": before, "limit": limit,
	})
}

// GetOpenInterest 持仓量。对应 get_open_interest(instType, uly=”, instId=”)
func (p *PublicAPI) GetOpenInterest(instType, uly, instId string) (map[string]any, error) {
	return p.RequestWithParams(GET, OpenInterest, Params{"instType": instType, "uly": uly, "instId": instId})
}

// GetFundingRate 资金费率。对应 get_funding_rate(instId)
func (p *PublicAPI) GetFundingRate(instId string) (map[string]any, error) {
	return p.RequestWithParams(GET, FundingRate, Params{"instId": instId})
}

// FundingRateHistory 资金费率历史。对应 funding_rate_history(...)
func (p *PublicAPI) FundingRateHistory(instId, after, before, limit string) (map[string]any, error) {
	return p.RequestWithParams(GET, FundingRateHistory, Params{
		"instId": instId, "after": after, "before": before, "limit": limit,
	})
}

// GetPriceLimit 限价。对应 get_price_limit(instId)
func (p *PublicAPI) GetPriceLimit(instId string) (map[string]any, error) {
	return p.RequestWithParams(GET, PriceLimit, Params{"instId": instId})
}

// GetOptSummary 期权行情。对应 get_opt_summary(uly, expTime=”)
func (p *PublicAPI) GetOptSummary(uly, expTime string) (map[string]any, error) {
	return p.RequestWithParams(GET, OptSummary, Params{"uly": uly, "expTime": expTime})
}

// GetEstimatedPrice 预估交割价。对应 get_estimated_price(instId)
func (p *PublicAPI) GetEstimatedPrice(instId string) (map[string]any, error) {
	return p.RequestWithParams(GET, EstimatedPrice, Params{"instId": instId})
}

// DiscountInterestFreeQuota 折扣率与免息额度。对应 discount_interest_free_quota(ccy=”)
func (p *PublicAPI) DiscountInterestFreeQuota(ccy string) (map[string]any, error) {
	return p.RequestWithParams(GET, DiscountInterestFreeQuota, Params{"ccy": ccy})
}

// GetSystemTime 服务器时间。对应 get_system_time()
func (p *PublicAPI) GetSystemTime() (map[string]any, error) {
	return p.RequestWithoutParams(GET, SystemTime)
}

// GetLiquidationOrders 强平订单。对应 get_liquidation_orders(...)
func (p *PublicAPI) GetLiquidationOrders(instType, mgnMode, instId, ccy, uly, alias, state, before, after, limit string) (map[string]any, error) {
	return p.RequestWithParams(GET, LiquidationOrders, Params{
		"instType": instType, "mgnMode": mgnMode, "instId": instId, "ccy": ccy, "uly": uly,
		"alias": alias, "state": state, "before": before, "after": after, "limit": limit,
	})
}

// GetMarkPrice 标记价。对应 get_mark_price(instType, uly=”, instId=”)
func (p *PublicAPI) GetMarkPrice(instType, uly, instId string) (map[string]any, error) {
	return p.RequestWithParams(GET, MarkPriceInfo, Params{"instType": instType, "uly": uly, "instId": instId})
}

// GetAnnouncements 公告列表。走「免鉴权」路径。
//
// annType 传空字符串表示不带该参数（返回全部类型）；
// 传非法值 OKX 会回 51000 Parameter annType error，所以这里做了非空判断。
// page 从 1 开始，留空等价于 1。
//
// 【重要】不能用 RequestWithParams：那会带上空的 OK-ACCESS-KEY 头，
// 公告接口会直接回 401 + 50103（Request header OK-ACCESS-KEY can not be empty）。
func (p *PublicAPI) GetAnnouncements(annType, page string) (map[string]any, error) {
	ps := Params{}
	if annType != "" {
		ps["annType"] = annType
	}
	if page != "" {
		ps["page"] = page
	}
	return p.RequestPublic(GET, Announcements, ps)
}
