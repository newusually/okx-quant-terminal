package okx

// utils.go —— 签名 / 时间戳 / 参数拼接（原 okx/utils.py 的 Go 版）
//
// 与 Python 版逐行等价，唯一区别是返回 error 而不是抛异常。

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Sign 计算 OK-ACCESS-SIGN：base64(HMAC-SHA256(secret, preHash))
//
// 对应 Python: utils.sign(utils.pre_hash(...), secretKey)
func Sign(message, secretKey string) string {
	mac := hmac.New(sha256.New, []byte(secretKey))
	mac.Write([]byte(message))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// PreHash 待签名串 = timestamp + METHOD + requestPath + body
//
// 注意 OKX 要求 method 大写，requestPath 必须带上 query string。
func PreHash(timestamp, method, requestPath, body string) string {
	return timestamp + strings.ToUpper(method) + requestPath + body
}

// Signature 一步到位：timestamp + method + path + body → 签名
//
// 对应 Python: utils.signature(...)。Python 版把 "{}" / "None" 的 body 归一成空串，
// 这里照做，避免下单时签名对不上（错误码 50113）。
func Signature(timestamp, method, requestPath, body, secretKey string) string {
	if body == "{}" || body == "None" || body == "null" {
		body = ""
	}
	return Sign(PreHash(timestamp, method, requestPath, body), secretKey)
}

// GetTimestamp 当前 UTC 时间，ISO8601 带毫秒 + Z，例如 2026-09-30T07:03:43.123Z
func GetTimestamp() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

// GetTimestampAt 指定时刻的 ISO8601 毫秒串（对时用）
func GetTimestampAt(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// Params 是参数集合。值支持 string / int / float64 / bool / nil。
//
// 顺带解决了 Python 版 parse_params_to_str 不做转义的问题：
// 这里走 url.Values，instId 之类含特殊字符也能正确编码。
type Params map[string]any

// SortKeys 返回排好序的键，保证同样的入参拼出同样的 query（便于调试比对）
func (p Params) SortKeys() []string {
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Encode 拼成 query string（不含前导 '?'）。空值 / nil 会被跳过。
//
// 与 Python 版的差异：Python 会把空字符串也拼上（instType=&instId=），
// 这里跳过空值——OKX 对“传了空参数”和“没传参数”语义不同，跳过更安全。
func (p Params) Encode() string {
	if len(p) == 0 {
		return ""
	}
	v := url.Values{}
	for _, k := range p.SortKeys() {
		s, ok := stringify(p[k])
		if !ok || s == "" {
			continue
		}
		v.Set(k, s)
	}
	return v.Encode()
}

// Body 序列化成下单用的 JSON。空集合返回 ""（不是 "{}"），
// 这样签名口径和 GET 一致。
func (p Params) Body() (string, error) {
	if len(p) == 0 {
		return "", nil
	}
	// 用 encoding/json 但手工控制：只输出非空值，免得 sort=""
	b, err := jsonMarshalNoEscape(p)
	if err != nil {
		return "", err
	}
	return b, nil
}

// stringify 把任意值转成 OKX 认识的字符串
func stringify(v any) (string, bool) {
	switch t := v.(type) {
	case nil:
		return "", false
	case string:
		return t, true
	case bool:
		if t {
			return "true", true
		}
		return "false", true
	case int:
		return strconv.Itoa(t), true
	case int64:
		return strconv.FormatInt(t, 10), true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 32), true
	case fmt.Stringer:
		return t.String(), true
	default:
		return fmt.Sprint(t), true
	}
}

// GetHeader 组装请求头。对应 Python: utils.get_header(...)
// GetHeader 带鉴权的请求头（私有接口 / 下单用）
func GetHeader(apiKey, sign, timestamp, passphrase, flag string) map[string]string {
	return map[string]string{
		ContentType:           ApplicationJSON,
		OkAccessKey:           apiKey,
		OkAccessSign:          sign,
		OkAccessTimestamp:     timestamp,
		OkAccessPassphrase:    passphrase,
		"x-simulated-trading": flag,
		"User-Agent":          "okx-go-sdk/1.0 (finally-main)",
		"Accept":              ApplicationJSON,
	}
}

// PublicHeader 免鉴权请求头：一个 OK-ACCESS-* 都不能带。
//
// 公告中心 /api/v5/support/announcements 这类接口只要收到空值的
// OK-ACCESS-KEY 头就会回 401 + 50103，必须用这套头。
func PublicHeader() map[string]string {
	return map[string]string{
		ContentType:  ApplicationJSON,
		"User-Agent": "okx-go-sdk/1.0 (finally-main)",
		"Accept":     ApplicationJSON,
	}
}

// Validate 基础参数校验（原 Python 版没有，属于新增的防呆）
func Validate(apiKey, secretKey, passphrase string) error {
	if strings.TrimSpace(apiKey) == "" {
		return errors.New("api_key 为空")
	}
	if strings.TrimSpace(secretKey) == "" {
		return errors.New("secret_key 为空")
	}
	if strings.TrimSpace(passphrase) == "" {
		return errors.New("passphrase 为空")
	}
	return nil
}

// QuoteEscape 给路径参数转义（instId 里有 '-'，本身安全，但统一处理更稳）
func QuoteEscape(s string) string { return url.QueryEscape(s) }
