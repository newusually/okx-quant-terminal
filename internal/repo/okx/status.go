package okx

// status.go —— 原 okx/status_api.py 的 Go 版（StatusAPI）

// StatusAPI 系统状态接口。对应 Python 的 StatusAPI。
type StatusAPI struct {
	*Client
}

// NewStatusAPI 对应 StatusAPI(...)
func NewStatusAPI(apiKey, apiSecretKey, passphrase string, useServerTime bool, flag string) *StatusAPI {
	return &StatusAPI{NewClient(apiKey, apiSecretKey, passphrase, useServerTime, flag)}
}

// NewStatusAPIWith 带代理 / 超时配置
func NewStatusAPIWith(apiKey, apiSecretKey, passphrase string, useServerTime bool, flag string, opts ClientOptions) *StatusAPI {
	return &StatusAPI{NewClientWithOptions(apiKey, apiSecretKey, passphrase, useServerTime, flag, opts)}
}

// Status 系统维护状态。对应 status(state=”)
//
// 返回里 scheduled/ongoing 非空表示该时段有维护，此时下单可能被拒。
func (s *StatusAPI) Status(state string) (map[string]any, error) {
	return s.RequestWithParams(GET, Status, Params{"state": state})
}
