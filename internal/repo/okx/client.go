package okx

// client.go —— 原 okx/client.py 的 Go 版
//
// 对外保留 Python 版的三个入口，签名习惯一一对应：
//
//	Python                                  Go
//	Client(api_key, secret, pass, useSt, flag)  NewClient(...)
//	_request(method, path, params)           Request(method, path, params)
//	_request_without_params(method, path)    RequestWithoutParams(method, path)
//	_request_with_params(method, path, p)    RequestWithParams(method, path, p)
//	_get_timestamp()                         ServerTimestamp()
//
// 新增（Python 版没有，但生产环境必须有）：
//   - 令牌桶限速：OKX 按 IP + 接口维度限速，打太快会连续 429
//   - 指数退避重试：429 / 50011 这类可重试错误自动重试
//   - 域名兜底：www.okx.com 不通时自动切 aws.okx.com
//   - 本机时钟偏移校正：偏移超过 2 秒会导致下单报 -1021

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ClientOptions 可选项，零值即为默认
type ClientOptions struct {
	BaseURL       string        // 默认 https://www.okx.com
	FallbackURLs  []string      // 默认 [aws.okx.com]
	Proxy         string        // 如 "http://127.0.0.1:7890"，留空直连
	Timeout       time.Duration // 默认 20s
	MaxRetries    int           // 默认 4
	RatePerSecond int           // 默认 12 次/秒（公共接口远够用）
}

// Client 一个 OKX 客户端。并发安全。
type Client struct {
	APIKey        string
	APISecretKey  string
	Passphrase    string
	UseServerTime bool
	Flag          string // "1" 模拟盘 / "0" 实盘

	opts ClientOptions

	http *http.Client

	mu        sync.Mutex
	base      string
	timeOffMs int64
	probed    bool

	limMu   sync.Mutex
	limLast []time.Time
}

// NewClient 对应 Python 的 Client(...)。flag 传 "" 时默认模拟盘。
func NewClient(apiKey, apiSecretKey, passphrase string, useServerTime bool, flag string) *Client {
	return NewClientWithOptions(apiKey, apiSecretKey, passphrase, useServerTime, flag, ClientOptions{})
}

// NewClientWithOptions 带可选配置
func NewClientWithOptions(apiKey, apiSecretKey, passphrase string, useServerTime bool, flag string, opts ClientOptions) *Client {
	c := &Client{
		APIKey:        apiKey,
		APISecretKey:  apiSecretKey,
		Passphrase:    passphrase,
		UseServerTime: useServerTime,
		Flag:          strings.TrimSpace(flag),
		opts:          opts,
	}
	if c.Flag == "" {
		c.Flag = FlagDemo
	}
	if c.opts.BaseURL == "" {
		c.opts.BaseURL = APIURL
	}
	if len(c.opts.FallbackURLs) == 0 {
		c.opts.FallbackURLs = []string{APIURLAlternate}
	}
	if c.opts.Timeout <= 0 {
		c.opts.Timeout = 20 * time.Second
	}
	if c.opts.MaxRetries <= 0 {
		c.opts.MaxRetries = 4
	}
	if c.opts.RatePerSecond <= 0 {
		c.opts.RatePerSecond = 12
	}
	tr := &http.Transport{
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	if p := strings.TrimSpace(c.opts.Proxy); p != "" {
		if pu, err := url.Parse(p); err == nil {
			tr.Proxy = http.ProxyURL(pu)
		}
	}
	c.http = &http.Client{Transport: tr, Timeout: c.opts.Timeout}
	c.base = strings.TrimRight(c.opts.BaseURL, "/")
	return c
}

// NewPublicClient 免密钥的公共客户端（行情 / 合约列表），很多场景不需要 Key
func NewPublicClient() *Client {
	return NewClient("", "", "", false, FlagDemo)
}

// ---------------------------------------------------------------------------
// 域名探测 + 对时
// ---------------------------------------------------------------------------

func (c *Client) candidates() []string {
	list := []string{c.opts.BaseURL}
	list = append(list, c.opts.FallbackURLs...)
	seen := map[string]bool{}
	out := make([]string, 0, len(list))
	for _, u := range list {
		u = strings.TrimRight(strings.TrimSpace(u), "/")
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	return out
}

// EnsureReady 挑一个能通的域名并校准时钟。只会真正探测一次。
func (c *Client) EnsureReady() error {
	c.mu.Lock()
	if c.probed {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	var lastErr error
	for _, base := range c.candidates() {
		ts, err := c.probeTime(base)
		if err != nil {
			lastErr = err
			continue
		}
		c.mu.Lock()
		c.base = base
		c.timeOffMs = ts - time.Now().UnixMilli()
		c.probed = true
		c.mu.Unlock()
		return nil
	}
	if lastErr == nil {
		lastErr = &RequestError{Message: "没有可用的 OKX 域名（可能被墙，请在配置里填 proxy）"}
	}
	return lastErr
}

func (c *Client) probeTime(base string) (int64, error) {
	req, err := http.NewRequest(GET, base+ServerTimestampURL, nil)
	if err != nil {
		return 0, &RequestError{Message: err.Error(), Err: err}
	}
	req.Header.Set("User-Agent", "okx-go-sdk/1.0")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, &RequestError{Message: err.Error(), Err: err}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return 0, newAPIError(ServerTimestampURL, resp.StatusCode, raw)
	}
	var env struct {
		Code string `json:"code"`
		Data []struct {
			Ts string `json:"ts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return 0, &RequestError{Message: "时间接口返回不是 JSON：" + truncate(string(raw), 120)}
	}
	if env.Code != "0" || len(env.Data) == 0 {
		return 0, newAPIError(ServerTimestampURL, resp.StatusCode, raw)
	}
	return strconv.ParseInt(env.Data[0].Ts, 10, 64)
}

// NowMs 带时钟偏移校正的当前毫秒时间戳
func (c *Client) NowMs() int64 {
	c.mu.Lock()
	off := c.timeOffMs
	c.mu.Unlock()
	return time.Now().UnixMilli() + off
}

// ---------------------------------------------------------------------------
// 限速
// ---------------------------------------------------------------------------

func (c *Client) waitRate() {
	per := time.Second
	max := c.opts.RatePerSecond
	for {
		c.limMu.Lock()
		now := time.Now()
		cut := 0
		for cut < len(c.limLast) && now.Sub(c.limLast[cut]) >= per {
			cut++
		}
		if cut > 0 {
			c.limLast = append([]time.Time(nil), c.limLast[cut:]...)
		}
		if len(c.limLast) < max {
			c.limLast = append(c.limLast, now)
			c.limMu.Unlock()
			return
		}
		sleep := per - now.Sub(c.limLast[0])
		c.limMu.Unlock()
		if sleep < 5*time.Millisecond {
			sleep = 5 * time.Millisecond
		}
		time.Sleep(sleep)
	}
}

// ---------------------------------------------------------------------------
// 请求
// ---------------------------------------------------------------------------

// Request 对应 Python 的 _request。返回解析后的 JSON（map）。
//
// params 为 nil 或空表示不带参数。
func (c *Client) Request(method, requestPath string, params Params) (map[string]any, error) {
	raw, err := c.Raw(method, requestPath, params)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &RequestError{Message: "响应不是 JSON：" + truncate(string(raw), 160)}
	}
	return out, nil
}

// RequestWithoutParams 对应 Python 的 _request_without_params
func (c *Client) RequestWithoutParams(method, requestPath string) (map[string]any, error) {
	return c.Request(method, requestPath, nil)
}

// RequestWithParams 对应 Python 的 _request_with_params
func (c *Client) RequestWithParams(method, requestPath string, params Params) (map[string]any, error) {
	return c.Request(method, requestPath, params)
}

// Raw 真正发请求，返回原始字节。做签名 / 限速 / 重试 / 域名兜底都在这里。
func (c *Client) Raw(method, requestPath string, params Params) ([]byte, error) {
	method = strings.ToUpper(strings.TrimSpace(method))
	fullPath := requestPath
	var body string
	if method == GET {
		if qs := params.Encode(); qs != "" {
			if strings.Contains(fullPath, "?") {
				fullPath += "&" + qs
			} else {
				fullPath += "?" + qs
			}
		}
	} else {
		b, err := jsonMarshalNoEscape(params)
		if err != nil {
			return nil, &ParamsError{Message: "参数序列化失败：" + err.Error()}
		}
		body = b
	}
	return c.do(method, fullPath, body)
}

// RequestJSON 用自定义 JSON body 发请求（批量下单 / 批量撤单那种数组 body）。
func (c *Client) RequestJSON(method, requestPath string, payload any) (map[string]any, error) {
	method = strings.ToUpper(strings.TrimSpace(method))
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, &ParamsError{Message: "参数序列化失败：" + err.Error()}
	}
	raw, err := c.do(method, requestPath, string(b))
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &RequestError{Message: "响应不是 JSON：" + truncate(string(raw), 160)}
	}
	return out, nil
}

// RequestPublic 免鉴权公开请求。
//
// 有些 OKX 公开接口（典型的是 /api/v5/support/announcements 公告中心）
// 只要看到 HTTP 头里出现空的 OK-ACCESS-KEY / OK-ACCESS-SIGN，
// 就会直接回 401 + code 50103「Request header OK-ACCESS-KEY can not be empty」，
// 所以这类接口必须一个鉴权头都不带。
func (c *Client) RequestPublic(method, requestPath string, params Params) (map[string]any, error) {
	method = strings.ToUpper(strings.TrimSpace(method))
	fullPath := requestPath
	var body string
	if method == GET {
		if qs := params.Encode(); qs != "" {
			if strings.Contains(fullPath, "?") {
				fullPath += "&" + qs
			} else {
				fullPath += "?" + qs
			}
		}
	} else {
		b, err := jsonMarshalNoEscape(params)
		if err != nil {
			return nil, &ParamsError{Message: "参数序列化失败：" + err.Error()}
		}
		body = b
	}
	raw, err := c.doWith(method, fullPath, body, false)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &RequestError{Message: "响应不是 JSON：" + truncate(string(raw), 160)}
	}
	return out, nil
}

// do 是真正的网络层：签名 / 限速 / 重试 / 域名兜底
func (c *Client) do(method, fullPath, body string) ([]byte, error) {
	return c.doWith(method, fullPath, body, true)
}

// doWith auth=false 时发「免鉴权」请求（不带任何 OK-ACCESS-* 头）
func (c *Client) doWith(method, fullPath, body string, auth bool) ([]byte, error) {
	if err := c.EnsureReady(); err != nil {
		return nil, err
	}

	c.mu.Lock()
	base := c.base
	c.mu.Unlock()
	full := base + fullPath

	var header map[string]string
	if auth {
		// 签名（用本机时间；UseServerTime 时先跟服务器对时）
		tsStr := GetTimestamp()
		if c.UseServerTime {
			if s, err := c.ServerTimestamp(); err == nil && s != "" {
				tsStr = msToISO(s)
			}
		}
		sign := Signature(tsStr, method, fullPath, body, c.APISecretKey)
		header = GetHeader(c.APIKey, sign, tsStr, c.Passphrase, c.Flag)
	} else {
		header = PublicHeader()
	}

	var lastErr error
	backoff := 1500 * time.Millisecond
	for attempt := 0; attempt <= c.opts.MaxRetries; attempt++ {
		c.waitRate()
		var rdr io.Reader
		if body != "" {
			rdr = bytes.NewReader([]byte(body))
		}
		req, err := http.NewRequest(method, full, rdr)
		if err != nil {
			return nil, &RequestError{Message: err.Error(), Err: err}
		}
		for k, v := range header {
			req.Header.Set(k, v)
		}

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = &RequestError{Message: err.Error(), Err: err}
			time.Sleep(backoff)
			backoff *= 2
			continue
		}
		raw, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr != nil {
			lastErr = &RequestError{Message: rerr.Error(), Err: rerr}
			time.Sleep(backoff)
			backoff *= 2
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			ae := newAPIError(fullPath, resp.StatusCode, raw)
			lastErr = ae
			if ae.Retryable() {
				time.Sleep(backoff)
				backoff *= 2
				continue
			}
			return nil, ae
		}

		var env struct {
			Code string `json:"code"`
			Msg  string `json:"msg"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			lastErr = &RequestError{Message: "响应不是 JSON：" + truncate(string(raw), 160)}
			return nil, lastErr
		}
		if env.Code != "0" {
			ae := &APIError{
				Code: env.Code, Message: env.Msg,
				StatusCode: resp.StatusCode, Path: fullPath,
				Raw: truncate(string(raw), 300),
			}
			lastErr = ae
			if ae.Retryable() {
				time.Sleep(backoff)
				backoff *= 2
				continue
			}
			return nil, ae
		}
		return raw, nil
	}
	if lastErr == nil {
		lastErr = &RequestError{Message: "请求失败（重试已耗尽）"}
	}
	return nil, lastErr
}

// ServerTimestamp 对应 Python 的 _get_timestamp：返回服务器毫秒时间戳（字符串）
func (c *Client) ServerTimestamp() (string, error) {
	raw, err := c.Raw(GET, ServerTimestampURL, nil)
	if err != nil {
		return "", err
	}
	var env struct {
		Code string `json:"code"`
		Data []struct {
			Ts string `json:"ts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", &RequestError{Message: err.Error()}
	}
	if len(env.Data) == 0 {
		return "", &RequestError{Message: "时间接口返回为空"}
	}
	return env.Data[0].Ts, nil
}

// BaseURL 当前实际使用的域名
func (c *Client) BaseURL() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.base
}

// ---------------------------------------------------------------------------
// 通用小工具（Go 侧新增，方便把 map[string]any 用起来）
// ---------------------------------------------------------------------------

// Data 取 data 数组
func Data(resp map[string]any) []map[string]any {
	raw, ok := resp["data"]
	if !ok || raw == nil {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(arr))
	for _, v := range arr {
		if m, ok := v.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// Str 安全取字符串字段（OKX 所有数值都是字符串）
func Str(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// F 安全取 float64（OKX 的数值字段都是字符串，这里一步到位转好）
func F(m map[string]any, key string) float64 {
	return ToFloat(Str(m, key))
}

// I 安全取 int
func I(m map[string]any, key string) int {
	v, err := strconv.Atoi(strings.TrimSpace(Str(m, key)))
	if err != nil {
		return 0
	}
	return v
}

// ToFloat 字符串转 float64，失败返回 0（OKX 空字段就是 ""）
func ToFloat(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v
}

// ToInt64 字符串转 int64
func ToInt64(s string) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// msToISO 毫秒时间戳串 → ISO8601，签名需要
func msToISO(ms string) string {
	v, err := strconv.ParseInt(strings.TrimSpace(ms), 10, 64)
	if err != nil || v <= 0 {
		return GetTimestamp()
	}
	return time.UnixMilli(v).UTC().Format("2006-01-02T15:04:05.000Z")
}

// jsonMarshalNoEscape 序列化参数：跳过空值，不转义 < > &
func jsonMarshalNoEscape(p Params) (string, error) {
	if len(p) == 0 {
		return "", nil
	}
	clean := make(map[string]any, len(p))
	for k, v := range p {
		if v == nil {
			continue
		}
		if s, ok := v.(string); ok && s == "" {
			continue
		}
		clean[k] = v
	}
	if len(clean) == 0 {
		return "", nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(clean); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
