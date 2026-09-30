package okx

// subaccount.go —— 原 okx/subAccount_api.py 的 Go 版（SubAccountAPI）

// SubAccountAPI 子账户接口。对应 Python 的 SubAccountAPI。
type SubAccountAPI struct {
	*Client
}

// NewSubAccountAPI 对应 SubAccountAPI(...)
func NewSubAccountAPI(apiKey, apiSecretKey, passphrase string, useServerTime bool, flag string) *SubAccountAPI {
	return &SubAccountAPI{NewClient(apiKey, apiSecretKey, passphrase, useServerTime, flag)}
}

// NewSubAccountAPIWith 带代理 / 超时配置
func NewSubAccountAPIWith(apiKey, apiSecretKey, passphrase string, useServerTime bool, flag string, opts ClientOptions) *SubAccountAPI {
	return &SubAccountAPI{NewClientWithOptions(apiKey, apiSecretKey, passphrase, useServerTime, flag, opts)}
}

// Balances 子账户余额。对应 balances(subAcct)
func (s *SubAccountAPI) Balances(subAcct string) (map[string]any, error) {
	return s.RequestWithParams(GET, SubAccountBalance, Params{"subAcct": subAcct})
}

// Bills 子账户流水。对应 bills(...)
func (s *SubAccountAPI) Bills(ccy, typ, subAcct, after, before, limit string) (map[string]any, error) {
	return s.RequestWithParams(GET, SubAccountBills, Params{
		"ccy": ccy, "type": typ, "subAcct": subAcct, "after": after, "before": before, "limit": limit,
	})
}

// Delete 删除子账户 API Key。对应 delete(pwd, subAcct, apiKey)
func (s *SubAccountAPI) Delete(pwd, subAcct, apiKey string) (map[string]any, error) {
	return s.RequestWithParams(POST, SubAccountDelete, Params{"pwd": pwd, "subAcct": subAcct, "apiKey": apiKey})
}

// Reset 重置子账户 API Key。对应 reset(pwd, subAcct, label, apiKey, perm, ip=”)
func (s *SubAccountAPI) Reset(pwd, subAcct, label, apiKey, perm, ip string) (map[string]any, error) {
	return s.RequestWithParams(POST, SubAccountReset, Params{
		"pwd": pwd, "subAcct": subAcct, "label": label, "apiKey": apiKey, "perm": perm, "ip": ip,
	})
}

// Create 创建子账户 API Key。对应 create(pwd, subAcct, label, Passphrase, perm=”, ip=”)
func (s *SubAccountAPI) Create(pwd, subAcct, label, passphrase, perm, ip string) (map[string]any, error) {
	return s.RequestWithParams(POST, SubAccountCreate, Params{
		"pwd": pwd, "subAcct": subAcct, "label": label,
		"Passphrase": passphrase, "perm": perm, "ip": ip,
	})
}

// ViewList 子账户列表。对应 view_list(...)
func (s *SubAccountAPI) ViewList(enable, subAcct, after, before, limit string) (map[string]any, error) {
	return s.RequestWithParams(GET, SubAccountViewList, Params{
		"enable": enable, "subAcct": subAcct, "after": after, "before": before, "limit": limit,
	})
}

// ControlTransfer 子账户间划转。对应 control_transfer(...)
func (s *SubAccountAPI) ControlTransfer(ccy, amt, froms, to, fromSubAccount, toSubAccount string) (map[string]any, error) {
	return s.RequestWithParams(POST, SubAccountControlTransfer, Params{
		"ccy": ccy, "amt": amt, "from": froms, "to": to,
		"fromSubAccount": fromSubAccount, "toSubAccount": toSubAccount,
	})
}
