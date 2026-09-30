package okx

// exceptions.go —— 原 okx/exceptions.py 的 Go 版
//
// Python 版有三个异常类：OkexAPIException / OkexRequestException / OkexParamsException。
// Go 没有异常，这里保留成 error 类型 + errors.Is/As 判断。

import (
	"encoding/json"
	"errors"
	"fmt"
)

// APIError 对应 OkexAPIException：接口返回了非 2xx 或 code != "0"
type APIError struct {
	Code       string // OKX 业务码，如 "51008"
	Message    string // OKX 的 msg
	StatusCode int    // HTTP 状态码
	Path       string // 请求路径，方便定位
	Raw        string // 原始响应体（截断）
}

func (e *APIError) Error() string {
	return fmt.Sprintf("API Request Error(code=%s http=%d path=%s): %s",
		e.Code, e.StatusCode, e.Path, e.Message)
}

// Retryable 判断是否值得重试：限速 / 系统繁忙 / 网关错误
func (e *APIError) Retryable() bool {
	switch e.Code {
	case "429", "50011", "50013", "50026", "50004":
		return true
	case "0":
		return false
	}
	if e.StatusCode == 429 || e.StatusCode >= 500 {
		return true
	}
	return false
}

// RequestError 对应 OkexRequestException：网络层失败（连不上、超时、解析失败）
type RequestError struct {
	Message string
	Err     error
}

func (e *RequestError) Error() string { return "OkexRequestException: " + e.Message }
func (e *RequestError) Unwrap() error { return e.Err }

// ParamsError 对应 OkexParamsException：调用方参数写错了
type ParamsError struct {
	Message string
}

func (e *ParamsError) Error() string { return "OkexParamsException: " + e.Message }

// ErrNoCredentials 没配 Key 就想调私有接口
var ErrNoCredentials = errors.New("OKX API Key / Secret / Passphrase 未配置（见 runtime/okx_strategy.json 的 okx 段）")

// newAPIError 从响应体构造 APIError，尽量把 code/msg 抽出来
func newAPIError(path string, status int, raw []byte) *APIError {
	e := &APIError{Path: path, StatusCode: status, Raw: truncate(string(raw), 300)}
	var env struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &env); err == nil {
		e.Code = env.Code
		e.Message = env.Msg
	}
	if e.Code == "" {
		e.Code = "None"
	}
	if e.Message == "" {
		e.Message = fmt.Sprintf("HTTP %d：%s", status, e.Raw)
	}
	return e
}
