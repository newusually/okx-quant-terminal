package okx

// account.go —— 原 okx/Account_api.py 的 Go 版（AccountAPI）

// AccountAPI 账户相关接口。对应 Python 的 AccountAPI。
type AccountAPI struct {
	*Client
}

// NewAccountAPI 对应 AccountAPI(api_key, secret, passphrase, use_server_time, flag)
func NewAccountAPI(apiKey, apiSecretKey, passphrase string, useServerTime bool, flag string) *AccountAPI {
	return &AccountAPI{NewClient(apiKey, apiSecretKey, passphrase, useServerTime, flag)}
}

// NewAccountAPIWith 带代理 / 超时等配置
func NewAccountAPIWith(apiKey, apiSecretKey, passphrase string, useServerTime bool, flag string, opts ClientOptions) *AccountAPI {
	return &AccountAPI{NewClientWithOptions(apiKey, apiSecretKey, passphrase, useServerTime, flag, opts)}
}

// GetPositionRisk 查看持仓与风险。对应 get_position_risk(instType=”)
func (a *AccountAPI) GetPositionRisk(instType string) (map[string]any, error) {
	return a.RequestWithParams(GET, PositionRisk, Params{"instType": instType})
}

// GetAccount 查看账户余额。对应 get_account(ccy=”)
func (a *AccountAPI) GetAccount(ccy string) (map[string]any, error) {
	return a.RequestWithParams(GET, AccountInfo, Params{"ccy": ccy})
}

// GetPositions 查看持仓信息。对应 get_positions(instType=”, instId=”)
func (a *AccountAPI) GetPositions(instType, instId string) (map[string]any, error) {
	return a.RequestWithParams(GET, PositionInfo, Params{"instType": instType, "instId": instId})
}

// GetBillsDetail 账单流水（近 7 天）。对应 get_bills_detail(...)
func (a *AccountAPI) GetBillsDetail(instType, ccy, mgnMode, ctType, typ, subType, after, before, limit string) (map[string]any, error) {
	return a.RequestWithParams(GET, BillsDetail, Params{
		"instType": instType, "ccy": ccy, "mgnMode": mgnMode, "ctType": ctType,
		"type": typ, "subType": subType, "after": after, "before": before, "limit": limit,
	})
}

// GetBillsDetails 账单流水（近 3 个月）。对应 get_bills_details(...)
func (a *AccountAPI) GetBillsDetails(instType, ccy, mgnMode, ctType, typ, subType, after, before, limit string) (map[string]any, error) {
	return a.RequestWithParams(GET, BillsArchive, Params{
		"instType": instType, "ccy": ccy, "mgnMode": mgnMode, "ctType": ctType,
		"type": typ, "subType": subType, "after": after, "before": before, "limit": limit,
	})
}

// GetAccountConfig 账户配置。对应 get_account_config()
func (a *AccountAPI) GetAccountConfig() (map[string]any, error) {
	return a.RequestWithoutParams(GET, AccountConfig)
}

// GetPositionMode 设置持仓模式。对应 get_position_mode(posMode)
func (a *AccountAPI) GetPositionMode(posMode string) (map[string]any, error) {
	return a.RequestWithParams(POST, PositionMode, Params{"posMode": posMode})
}

// SetLeverage 设置杠杆。对应 set_leverage(lever, mgnMode, instId=”, ccy=”, posSide=”)
//
// 注意 Python 版参数是字符串（lever='50'），Go 版接受任意数值类型。
func (a *AccountAPI) SetLeverage(lever, mgnMode, instId, ccy, posSide string) (map[string]any, error) {
	return a.RequestWithParams(POST, SetLeverage, Params{
		"lever": lever, "mgnMode": mgnMode, "instId": instId, "ccy": ccy, "posSide": posSide,
	})
}

// GetMaximumTradeSize 最大可交易数量。对应 get_maximum_trade_size(instId, tdMode, ccy=”, px=”)
func (a *AccountAPI) GetMaximumTradeSize(instId, tdMode, ccy, px string) (map[string]any, error) {
	return a.RequestWithParams(GET, MaxTradeSize, Params{"instId": instId, "tdMode": tdMode, "ccy": ccy, "px": px})
}

// GetMaxAvailSize 最大可用数量。对应 get_max_avail_size(instId, tdMode, ccy=”, reduceOnly=”)
func (a *AccountAPI) GetMaxAvailSize(instId, tdMode, ccy, reduceOnly string) (map[string]any, error) {
	return a.RequestWithParams(GET, MaxAvailSize, Params{
		"instId": instId, "tdMode": tdMode, "ccy": ccy, "reduceOnly": reduceOnly,
	})
}

// AdjustmentMargin 调整保证金。对应 Adjustment_margin(instId, posSide, type, amt)
func (a *AccountAPI) AdjustmentMargin(instId, posSide, typ, amt string) (map[string]any, error) {
	return a.RequestWithParams(POST, AdjustmentMargin, Params{
		"instId": instId, "posSide": posSide, "type": typ, "amt": amt,
	})
}

// GetLeverage 查询杠杆。对应 get_leverage(instId, mgnMode)
func (a *AccountAPI) GetLeverage(instId, mgnMode string) (map[string]any, error) {
	return a.RequestWithParams(GET, GetLeverage, Params{"instId": instId, "mgnMode": mgnMode})
}

// GetMaxLoan 逐仓最大借币。对应 get_max_load(instId, mgnMode, mgnCcy)
func (a *AccountAPI) GetMaxLoan(instId, mgnMode, mgnCcy string) (map[string]any, error) {
	return a.RequestWithParams(GET, MaxLoan, Params{"instId": instId, "mgnMode": mgnMode, "mgnCcy": mgnCcy})
}

// GetFeeRates 手续费率。对应 get_fee_rates(instType, instId=”, uly=”, category=”)
func (a *AccountAPI) GetFeeRates(instType, instId, uly, category string) (map[string]any, error) {
	return a.RequestWithParams(GET, FeeRates, Params{
		"instType": instType, "instId": instId, "uly": uly, "category": category,
	})
}

// GetInterestAccrued 计息记录。对应 get_interest_accrued(...)
func (a *AccountAPI) GetInterestAccrued(instId, ccy, mgnMode, after, before, limit string) (map[string]any, error) {
	return a.RequestWithParams(GET, InterestAccrued, Params{
		"instId": instId, "ccy": ccy, "mgnMode": mgnMode, "after": after, "before": before, "limit": limit,
	})
}

// GetInterestRate 借币利率。对应 get_interest_rate(ccy=”)
func (a *AccountAPI) GetInterestRate(ccy string) (map[string]any, error) {
	return a.RequestWithParams(GET, InterestRate, Params{"ccy": ccy})
}

// SetGreeks 设置 Greeks 类型。对应 set_greeks(greeksType)
func (a *AccountAPI) SetGreeks(greeksType string) (map[string]any, error) {
	return a.RequestWithParams(POST, SetGreeks, Params{"greeksType": greeksType})
}

// GetMaxWithdrawal 最大可提。对应 get_max_withdrawal(ccy=”)
func (a *AccountAPI) GetMaxWithdrawal(ccy string) (map[string]any, error) {
	return a.RequestWithParams(GET, MaxWithdrawal, Params{"ccy": ccy})
}

// GetAdjustLeverageInfo 杠杆预估。对应 get_adjust_leverage_info(...)
func (a *AccountAPI) GetAdjustLeverageInfo(instType, mgnMode, lever, instId string) (map[string]any, error) {
	return a.RequestWithParams(GET, AdjustLeverageInfo, Params{
		"instType": instType, "mgnMode": mgnMode, "lever": lever, "instId": instId,
	})
}
