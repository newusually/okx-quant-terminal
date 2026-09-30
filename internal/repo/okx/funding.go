package okx

// funding.go —— 原 okx/Funding_api.py 的 Go 版（FundingAPI）

// FundingAPI 资金账户接口。对应 Python 的 FundingAPI。
type FundingAPI struct {
	*Client
}

// NewFundingAPI 对应 FundingAPI(...)
func NewFundingAPI(apiKey, apiSecretKey, passphrase string, useServerTime bool, flag string) *FundingAPI {
	return &FundingAPI{NewClient(apiKey, apiSecretKey, passphrase, useServerTime, flag)}
}

// NewFundingAPIWith 带代理 / 超时配置
func NewFundingAPIWith(apiKey, apiSecretKey, passphrase string, useServerTime bool, flag string, opts ClientOptions) *FundingAPI {
	return &FundingAPI{NewClientWithOptions(apiKey, apiSecretKey, passphrase, useServerTime, flag, opts)}
}

// GetDepositAddress 充值地址。对应 get_deposit_address(ccy)
func (f *FundingAPI) GetDepositAddress(ccy string) (map[string]any, error) {
	return f.RequestWithParams(GET, DepositAddress, Params{"ccy": ccy})
}

// GetBalances 资金余额。对应 get_balances(ccy=”)
func (f *FundingAPI) GetBalances(ccy string) (map[string]any, error) {
	return f.RequestWithParams(GET, GetBalances, Params{"ccy": ccy})
}

// FundsTransfer 资金划转。对应 funds_transfer(ccy, amt, froms, to, type='0', ...)
//
// 注意 Python 版把 from 写成形参 froms（from 是关键字），Go 版沿用 froms 这个名字。
func (f *FundingAPI) FundsTransfer(ccy, amt, froms, to, typ, subAcct, instId, toInstId string) (map[string]any, error) {
	return f.RequestWithParams(POST, FundsTransfer, Params{
		"ccy": ccy, "amt": amt, "from": froms, "to": to, "type": typ,
		"subAcct": subAcct, "instId": instId, "toInstId": toInstId,
	})
}

// CoinWithdraw 提币。对应 coin_withdraw(ccy, amt, dest, toAddr, pwd, fee)
//
// ⚠️ 只有持有「提币」权限的 Key 才能调用。本项目建议永不开启提币权限。
func (f *FundingAPI) CoinWithdraw(ccy, amt, dest, toAddr, pwd, fee string) (map[string]any, error) {
	return f.RequestWithParams(POST, WithdrawalCoin, Params{
		"ccy": ccy, "amt": amt, "dest": dest, "toAddr": toAddr, "pwd": pwd, "fee": fee,
	})
}

// GetDepositHistory 充值记录。对应 get_deposit_history(...)
func (f *FundingAPI) GetDepositHistory(ccy, state, after, before, limit string) (map[string]any, error) {
	return f.RequestWithParams(GET, DepositHistory, Params{
		"ccy": ccy, "state": state, "after": after, "before": before, "limit": limit,
	})
}

// GetWithdrawalHistory 提币记录。对应 get_withdrawal_history(...)
func (f *FundingAPI) GetWithdrawalHistory(ccy, state, after, before, limit string) (map[string]any, error) {
	return f.RequestWithParams(GET, WithdrawalHistory, Params{
		"ccy": ccy, "state": state, "after": after, "before": before, "limit": limit,
	})
}

// GetCurrency 币种信息。对应 get_currency()
func (f *FundingAPI) GetCurrency() (map[string]any, error) {
	return f.RequestWithoutParams(GET, CurrencyInfo)
}

// PurchaseRedempt 余币宝申购赎回。对应 purchase_redempt(ccy, amt, side)
func (f *FundingAPI) PurchaseRedempt(ccy, amt, side string) (map[string]any, error) {
	return f.RequestWithParams(POST, PurchaseRedempt, Params{"ccy": ccy, "amt": amt, "side": side})
}

// GetBills 资金流水。对应 get_bills(...)
func (f *FundingAPI) GetBills(ccy, typ, after, before, limit string) (map[string]any, error) {
	return f.RequestWithParams(GET, BillsInfo, Params{
		"ccy": ccy, "type": typ, "after": after, "before": before, "limit": limit,
	})
}
