package okx

// market.go —— 原 okx/Market_api.py 的 Go 版（MarketAPI）

// MarketAPI 行情接口。对应 Python 的 MarketAPI。
type MarketAPI struct {
	*Client
}

// NewMarketAPI 对应 MarketAPI(...)
func NewMarketAPI(apiKey, apiSecretKey, passphrase string, useServerTime bool, flag string) *MarketAPI {
	return &MarketAPI{NewClient(apiKey, apiSecretKey, passphrase, useServerTime, flag)}
}

// NewMarketAPIWith 带代理 / 超时配置
func NewMarketAPIWith(apiKey, apiSecretKey, passphrase string, useServerTime bool, flag string, opts ClientOptions) *MarketAPI {
	return &MarketAPI{NewClientWithOptions(apiKey, apiSecretKey, passphrase, useServerTime, flag, opts)}
}

// NewPublicMarketAPI 免密钥行情客户端（回补行情只需要这个）
func NewPublicMarketAPI() *MarketAPI { return &MarketAPI{NewPublicClient()} }

// GetTickers 全市场行情。对应 get_tickers(instType, uly=”)
func (m *MarketAPI) GetTickers(instType, uly string) (map[string]any, error) {
	return m.RequestWithParams(GET, TickersInfo, Params{"instType": instType, "uly": uly})
}

// GetTicker 单个产品行情。对应 get_ticker(instId)
func (m *MarketAPI) GetTicker(instId string) (map[string]any, error) {
	return m.RequestWithParams(GET, TickerInfo, Params{"instId": instId})
}

// GetIndexTicker 指数行情。对应 get_index_ticker(quoteCcy=”, instId=”)
func (m *MarketAPI) GetIndexTicker(quoteCcy, instId string) (map[string]any, error) {
	return m.RequestWithParams(GET, IndexTickers, Params{"quoteCcy": quoteCcy, "instId": instId})
}

// GetOrderBook 深度。对应 get_orderbook(instId, sz=”)
func (m *MarketAPI) GetOrderBook(instId, sz string) (map[string]any, error) {
	return m.RequestWithParams(GET, OrderBooks, Params{"instId": instId, "sz": sz})
}

// GetCandlesticks K 线。对应 get_candlesticks(instId, after=”, before=”, bar=”, limit=”)
func (m *MarketAPI) GetCandlesticks(instId, after, before, bar, limit string) (map[string]any, error) {
	return m.RequestWithParams(GET, MarketCandles, Params{
		"instId": instId, "after": after, "before": before, "bar": bar, "limit": limit,
	})
}

// GetHistoryCandlesticks 历史 K 线（翻页用）。对应 get_history_candlesticks(...)
func (m *MarketAPI) GetHistoryCandlesticks(instId, after, before, bar, limit string) (map[string]any, error) {
	return m.RequestWithParams(GET, HistoryCandles, Params{
		"instId": instId, "after": after, "before": before, "bar": bar, "limit": limit,
	})
}

// GetIndexCandlesticks 指数 K 线。对应 get_index_candlesticks(...)
func (m *MarketAPI) GetIndexCandlesticks(instId, after, before, bar, limit string) (map[string]any, error) {
	return m.RequestWithParams(GET, IndexCandles, Params{
		"instId": instId, "after": after, "before": before, "bar": bar, "limit": limit,
	})
}

// GetMarkPriceCandlesticks 标记价 K 线。对应 get_markprice_candlesticks(...)
func (m *MarketAPI) GetMarkPriceCandlesticks(instId, after, before, bar, limit string) (map[string]any, error) {
	return m.RequestWithParams(GET, MarkPriceCandles, Params{
		"instId": instId, "after": after, "before": before, "bar": bar, "limit": limit,
	})
}

// GetTrades 最新成交。对应 get_trades(instId, limit=”)
func (m *MarketAPI) GetTrades(instId, limit string) (map[string]any, error) {
	return m.RequestWithParams(GET, MarketTrades, Params{"instId": instId, "limit": limit})
}

// GetVolume 平台 24h 成交。对应 get_volume()
func (m *MarketAPI) GetVolume() (map[string]any, error) {
	return m.RequestWithoutParams(GET, Volume)
}

// GetOracle 预言机。对应 get_oracle()
func (m *MarketAPI) GetOracle() (map[string]any, error) {
	return m.RequestWithoutParams(GET, Oracle)
}

// GetTier 档位。对应 get_tier(...)
func (m *MarketAPI) GetTier(instType, tdMode, uly, instId, ccy, tier string) (map[string]any, error) {
	return m.RequestWithParams(GET, Tier, Params{
		"instType": instType, "tdMode": tdMode, "uly": uly,
		"instId": instId, "ccy": ccy, "tier": tier,
	})
}
