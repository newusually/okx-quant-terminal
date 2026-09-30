package okx

// consts.go —— OKX V5 接口常量（原 okx/consts.py 的 Go 版，一字不改地搬过来）
//
// 命名沿用 Python 版，方便和旧代码对照：
//
//	Python:  consts.ACCOUNT_INFO
//	Go:      okx.AccountInfo
//
// 只有 Python 里带下划线的名字做了大驼峰处理，其余全大写常量保持原样语义。

// ---------------------------------------------------------------------------
// HTTP 头
// ---------------------------------------------------------------------------

const (
	ContentType = "Content-Type"

	OkAccessKey        = "OK-ACCESS-KEY"
	OkAccessSign       = "OK-ACCESS-SIGN"
	OkAccessTimestamp  = "OK-ACCESS-TIMESTAMP"
	OkAccessPassphrase = "OK-ACCESS-PASSPHRASE"

	Accept          = "Accept"
	Cookie          = "Cookie"
	Locale          = "Locale="
	ApplicationJSON = "application/json"

	GET  = "GET"
	POST = "POST"
)

// APIURL 默认域名。被墙时可以换 AWS 节点。
const (
	APIURL             = "https://www.okx.com"
	APIURLAlternate    = "https://aws.okx.com"
	ServerTimestampURL = "/api/v5/public/time"
)

// ---------------------------------------------------------------------------
// Account
// ---------------------------------------------------------------------------

const (
	PositionRisk       = "/api/v5/account/account-position-risk"
	AccountInfo        = "/api/v5/account/balance"
	PositionInfo       = "/api/v5/account/positions"
	BillsDetail        = "/api/v5/account/bills"
	BillsArchive       = "/api/v5/account/bills-archive"
	AccountConfig      = "/api/v5/account/config"
	PositionMode       = "/api/v5/account/set-position-mode"
	SetLeverage        = "/api/v5/account/set-leverage"
	MaxTradeSize       = "/api/v5/account/max-size"
	MaxAvailSize       = "/api/v5/account/max-avail-size"
	AdjustmentMargin   = "/api/v5/account/position/margin-balance"
	GetLeverage        = "/api/v5/account/leverage-info"
	MaxLoan            = "/api/v5/account/max-loan"
	FeeRates           = "/api/v5/account/trade-fee"
	InterestAccrued    = "/api/v5/account/interest-accrued"
	InterestRate       = "/api/v5/account/interest-rate"
	SetGreeks          = "/api/v5/account/set-greeks"
	MaxWithdrawal      = "/api/v5/account/max-withdrawal"
	AdjustLeverageInfo = "/api/v5/account/adjust-leverage-info"
)

// ---------------------------------------------------------------------------
// Funding（资金账户）
// ---------------------------------------------------------------------------

const (
	DepositAddress    = "/api/v5/asset/deposit-address"
	GetBalances       = "/api/v5/asset/balances"
	FundsTransfer     = "/api/v5/asset/transfer"
	WithdrawalCoin    = "/api/v5/asset/withdrawal"
	DepositHistory    = "/api/v5/asset/deposit-history"
	WithdrawalHistory = "/api/v5/asset/withdrawal-history"
	CurrencyInfo      = "/api/v5/asset/currencies"
	PurchaseRedempt   = "/api/v5/asset/purchase_redempt"
	BillsInfo         = "/api/v5/asset/bills"
)

// ---------------------------------------------------------------------------
// Market Data
// ---------------------------------------------------------------------------

const (
	TickersInfo      = "/api/v5/market/tickers"
	TickerInfo       = "/api/v5/market/ticker"
	IndexTickers     = "/api/v5/market/index-tickers"
	OrderBooks       = "/api/v5/market/books"
	MarketCandles    = "/api/v5/market/candles"
	HistoryCandles   = "/api/v5/market/history-candles"
	IndexCandles     = "/api/v5/market/index-candles"
	MarkPriceCandles = "/api/v5/market/mark-price-candles"
	MarketTrades     = "/api/v5/market/trades"
	Volume           = "/api/v5/market/platform-24-volume"
	Oracle           = "/api/v5/market/oracle"
	Tier             = "/api/v5/public/tier"
)

// ---------------------------------------------------------------------------
// Public Data
// ---------------------------------------------------------------------------

const (
	InstrumentInfo            = "/api/v5/public/instruments"
	DeliveryExercise          = "/api/v5/public/delivery-exercise-history"
	OpenInterest              = "/api/v5/public/open-interest"
	FundingRate               = "/api/v5/public/funding-rate"
	FundingRateHistory        = "/api/v5/public/funding-rate-history"
	PriceLimit                = "/api/v5/public/price-limit"
	OptSummary                = "/api/v5/public/opt-summary"
	EstimatedPrice            = "/api/v5/public/estimated-price"
	DiscountInterestFreeQuota = "/api/v5/public/discount-rate-interest-free-quota"
	SystemTime                = "/api/v5/public/time"
	LiquidationOrders         = "/api/v5/public/liquidation-orders"
	MarkPriceInfo             = "/api/v5/public/mark-price"

	// Announcements 公告中心：上线 / 下线 / 交易调整等。
	// annType 可选值（实测）：
	//   announcements-new-listings          新币上线
	//   announcements-delistings            下架 / 下线
	//   announcements-trading-updates       交易规则调整
	//   announcements-deposit-withdrawal-suspension-resumption  充提暂停
	//   latest-events / announcements-earn-and-loan / announcements-web3
	// 留空表示不带该参数（= 全部类型）。注意：传了非法的 annType 会返回 51000。
	Announcements = "/api/v5/support/announcements"

	// AnnTypeNewListings 新上线公告
	AnnTypeNewListings = "announcements-new-listings"
	// AnnTypeDelistings 下线公告
	AnnTypeDelistings = "announcements-delistings"
)

// ---------------------------------------------------------------------------
// Trade
// ---------------------------------------------------------------------------

const (
	PlaceOrder        = "/api/v5/trade/order"
	BatchOrders       = "/api/v5/trade/batch-orders"
	CancelOrder       = "/api/v5/trade/cancel-order"
	CancelBatchOrders = "/api/v5/trade/cancel-batch-orders"
	AmendOrder        = "/api/v5/trade/amend-order"
	AmendBatchOrders  = "/api/v5/trade/amend-batch-orders"
	ClosePosition     = "/api/v5/trade/close-position"
	OrderInfo         = "/api/v5/trade/order"
	OrdersPending     = "/api/v5/trade/orders-pending"
	OrdersHistory     = "/api/v5/trade/orders-history"
	OrdersHistoryArch = "/api/v5/trade/orders-history-archive"
	OrderFills        = "/api/v5/trade/fills"
	PlaceAlgoOrder    = "/api/v5/trade/order-algo"
	CancelAlgos       = "/api/v5/trade/cancel-algos"
	OrdersAlgoPending = "/api/v5/trade/orders-algo-pending"
	OrdersAlgoHistory = "/api/v5/trade/orders-algo-history"
)

// ---------------------------------------------------------------------------
// SubAccount
// ---------------------------------------------------------------------------

const (
	SubAccountBalance         = "/api/v5/account/subaccount/balances"
	SubAccountBills           = "/api/v5/asset/subaccount/bills"
	SubAccountDelete          = "/api/v5/users/subaccount/delete-apikey"
	SubAccountReset           = "/api/v5/users/subaccount/modify-apikey"
	SubAccountCreate          = "/api/v5/users/subaccount/apikey"
	SubAccountViewList        = "/api/v5/users/subaccount/list"
	SubAccountControlTransfer = "/api/v5/asset/subaccount/transfer"
)

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

const Status = "/api/v5/system/status"

// ---------------------------------------------------------------------------
// 盘口 / 交易方向（原 Python 里到处硬编码的字符串，抽成常量，少打错字）
// ---------------------------------------------------------------------------

const (
	SideBuy  = "buy"
	SideSell = "sell"

	PosSideLong  = "long"
	PosSideShort = "short"
	PosSideNet   = "net"

	TdModeCross    = "cross"
	TdModeIsolated = "isolated"

	OrdTypeMarket      = "market"
	OrdTypeLimit       = "limit"
	OrdTypeConditional = "conditional"

	InstTypeSpot = "SPOT"
	InstTypeSwap = "SWAP"

	// FlagDemo 模拟盘；FlagLive 实盘。对应原 Python 的 flag='1' / flag='0'
	FlagDemo = "1"
	FlagLive = "0"
)
