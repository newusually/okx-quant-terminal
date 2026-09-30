package service

// okxclient.go —— OKX REST 客户端（只用 Go 标准库，不引任何第三方包）
//
// 公共接口：域名探测、对时、合约列表、行情、K 线
// 签名接口：持仓、余额、设杠杆、下单、平仓（HMAC-SHA256，OKX V5 口径）
//
// 内置：令牌桶限速 + 429/50011 指数退避重试。

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"finally-main/internal/conf"
	"finally-main/internal/logx"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const okxRespOK = "0"

// ---------------------------------------------------------------------------
// 错误
// ---------------------------------------------------------------------------

type OKXError struct {
	Code string
	Msg  string
	HTTP int
	Path string
}

func (e *OKXError) Error() string {
	return fmt.Sprintf("OKX code=%s http=%d msg=%s path=%s", e.Code, e.HTTP, e.Msg, e.Path)
}

func (e *OKXError) Retryable() bool {
	switch e.Code {
	case "429", "50011", "50013", "50026":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// 限速
// ---------------------------------------------------------------------------

type rateLimiter struct {
	mu    sync.Mutex
	times []time.Time
	max   int
	per   time.Duration
}

func newRateLimiter(max int, per time.Duration) *rateLimiter {
	if max <= 0 {
		max = 1
	}
	return &rateLimiter{max: max, per: per}
}

func (l *rateLimiter) Wait() {
	for {
		l.mu.Lock()
		now := time.Now()
		cut := 0
		for cut < len(l.times) && now.Sub(l.times[cut]) >= l.per {
			cut++
		}
		if cut > 0 {
			l.times = append([]time.Time(nil), l.times[cut:]...)
		}
		if len(l.times) < l.max {
			l.times = append(l.times, now)
			l.mu.Unlock()
			return
		}
		sleep := l.per - now.Sub(l.times[0])
		l.mu.Unlock()
		if sleep < 5*time.Millisecond {
			sleep = 5 * time.Millisecond
		}
		time.Sleep(sleep)
	}
}

// ---------------------------------------------------------------------------
// 客户端
// ---------------------------------------------------------------------------

type OKXClient struct {
	cfg         *conf.Config
	http        *http.Client
	candleLimit *rateLimiter
	tradeLimit  *rateLimiter

	mu        sync.Mutex
	base      string
	timeOffMs int64
	ready     bool

	cacheMu       sync.Mutex
	instruments   map[string]Instrument
	instLoadedAt  time.Time
	lastQuoteVols map[string]float64
}

func newOKXClient(cfg *conf.Config) (*OKXClient, error) {
	timeout := time.Duration(cfg.RequestTimeoutSec) * time.Second
	tr := &http.Transport{
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	if cfg.OKX != nil {
		if p := strings.TrimSpace(cfg.OKX.Proxy); p != "" {
			pu, err := url.Parse(p)
			if err != nil {
				return nil, fmt.Errorf("okx.proxy 地址无法解析：%v", err)
			}
			tr.Proxy = http.ProxyURL(pu)
		}
	}
	return &OKXClient{
		cfg:         cfg,
		http:        &http.Client{Transport: tr, Timeout: timeout},
		candleLimit: newRateLimiter(20, 2*time.Second), // /market/candles 限 40/2s，取一半更稳
		tradeLimit:  newRateLimiter(8, 2*time.Second),  // 交易类接口更保守
	}, nil
}

func (c *OKXClient) baseURL() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.base == "" {
		return strings.TrimRight(c.cfg.OKX.BaseURL, "/")
	}
	return c.base
}

func (c *OKXClient) nowMs() int64 {
	return time.Now().UnixNano()/int64(time.Millisecond) + atomic.LoadInt64(&c.timeOffMs)
}

func (c *OKXClient) candidates() []string {
	var list []string
	if c.cfg.OKX != nil {
		if s := strings.TrimSpace(c.cfg.OKX.BaseURL); s != "" {
			list = append(list, s)
		}
		list = append(list, c.cfg.OKX.FallbackURLs...)
	}
	if len(list) == 0 {
		list = []string{"https://www.okx.com", "https://aws.okx.com", "https://okx.com"}
	}
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

// EnsureReady 挑一个能通的域名 + 对时（只做一次，后续复用）
func (c *OKXClient) EnsureReady() error {
	c.mu.Lock()
	if c.ready {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	var lastErr error
	for _, base := range c.candidates() {
		ts, err := c.probeTime(base)
		if err != nil {
			lastErr = err
			logx.Logf("WARN", "OKX 域名不可用 %s：%v", base, err)
			continue
		}
		off := ts - time.Now().UnixNano()/int64(time.Millisecond)
		atomic.StoreInt64(&c.timeOffMs, off)
		c.mu.Lock()
		c.base = base
		c.ready = true
		c.mu.Unlock()
		logx.Logf("INFO", "OKX 已连接 %s（本机时钟偏移 %d ms）", base, off)
		if off > 2000 || off < -2000 {
			logx.Logf("WARN", "本机时间与 OKX 相差 %d ms，超过 2 秒会导致下单报 -1021，请对时（w32tm /resync）", off)
		}
		return nil
	}
	if lastErr == nil {
		lastErr = errors.New("没有可用的 OKX 域名（可能被墙，请配置 okx.proxy）")
	}
	return lastErr
}

func (c *OKXClient) probeTime(base string) (int64, error) {
	req, err := http.NewRequest("GET", base+"/api/v5/public/time", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "okx-go-strategy/1.0")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("http=%d %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code string `json:"code"`
		Data []struct {
			Ts string `json:"ts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return 0, fmt.Errorf("响应不是 JSON：%s", truncate(string(raw), 120))
	}
	if env.Code != okxRespOK || len(env.Data) == 0 {
		return 0, fmt.Errorf("code=%s %s", env.Code, truncate(string(raw), 120))
	}
	ms, err := strconv.ParseInt(env.Data[0].Ts, 10, 64)
	if err != nil {
		return 0, err
	}
	return ms, nil
}

// ---------------------------------------------------------------------------
// 请求
// ---------------------------------------------------------------------------

func (c *OKXClient) request(method, path string, body []byte, signed bool, lim *rateLimiter) (json.RawMessage, error) {
	if err := c.EnsureReady(); err != nil {
		return nil, err
	}
	full := c.baseURL() + path

	var lastErr error
	backoff := 2 * time.Second
	for attempt := 0; attempt < 5; attempt++ {
		if lim != nil {
			lim.Wait()
		}
		req, err := http.NewRequest(strings.ToUpper(method), full, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "okx-go-strategy/1.0")
		if signed {
			if err := c.sign(req, method, path, body); err != nil {
				return nil, err
			}
		}
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Second)
			continue
		}
		raw, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr != nil {
			lastErr = rerr
			time.Sleep(time.Second)
			continue
		}
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			lastErr = &OKXError{Code: strconv.Itoa(resp.StatusCode), Msg: truncate(string(raw), 160), HTTP: resp.StatusCode, Path: path}
			time.Sleep(backoff)
			backoff *= 2
			continue
		}
		var env struct {
			Code string          `json:"code"`
			Msg  string          `json:"msg"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, fmt.Errorf("响应不是 JSON（http=%d）：%s", resp.StatusCode, truncate(string(raw), 160))
		}
		if env.Code != okxRespOK {
			oe := &OKXError{Code: env.Code, Msg: env.Msg, HTTP: resp.StatusCode, Path: path}
			if oe.Retryable() {
				lastErr = oe
				time.Sleep(backoff)
				backoff *= 2
				continue
			}
			return nil, oe
		}
		return env.Data, nil
	}
	if lastErr == nil {
		lastErr = errors.New("请求失败（重试已耗尽）")
	}
	return nil, lastErr
}

func (c *OKXClient) sign(req *http.Request, method, path string, body []byte) error {
	o := c.cfg.OKX
	if o == nil || o.APIKey == "" || o.SecretKey == "" || o.Passphrase == "" {
		return errors.New("OKX API Key / Secret / Passphrase 未配置（见 runtime/okx_strategy.json 的 okx 段）")
	}
	ts := time.Unix(0, c.nowMs()*int64(time.Millisecond)).UTC().Format("2006-01-02T15:04:05.000Z")
	pre := ts + strings.ToUpper(method) + path + string(body)
	mac := hmac.New(sha256.New, []byte(o.SecretKey))
	mac.Write([]byte(pre))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	req.Header.Set("OK-ACCESS-KEY", o.APIKey)
	req.Header.Set("OK-ACCESS-SIGN", sig)
	req.Header.Set("OK-ACCESS-TIMESTAMP", ts)
	req.Header.Set("OK-ACCESS-PASSPHRASE", o.Passphrase)
	if o.Simulated {
		req.Header.Set("x-simulated-trading", "1")
	}
	return nil
}

func (c *OKXClient) Get(path string, signed bool) (json.RawMessage, error) {
	var lim *rateLimiter
	if strings.Contains(path, "/market/candles") || strings.Contains(path, "/market/history-candles") {
		lim = c.candleLimit
	}
	return c.request("GET", path, nil, signed, lim)
}

func (c *OKXClient) Post(path string, payload interface{}) (json.RawMessage, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return c.request("POST", path, body, true, c.tradeLimit)
}

// ---------------------------------------------------------------------------
// 行情 / 合约
// ---------------------------------------------------------------------------

type Instrument struct {
	InstID    string  `json:"instId"`
	CtVal     float64 `json:"ctVal"`
	CtMult    float64 `json:"ctMult"`
	LotSz     float64 `json:"lotSz"`
	LotSzDec  int     `json:"-"`
	MinSz     float64 `json:"minSz"`
	TickSz    float64 `json:"tickSz"`
	Lever     int     `json:"lever"`
	SettleCcy string  `json:"settleCcy"`
	State     string  `json:"state"`
	ListTime  int64   `json:"listTime"`
	// InstCategory OKX 品种分类：1=加密 3=美股/ETF 4=商品
	InstCategory string `json:"instCategory"`
}

type Ticker struct {
	InstID    string
	Last      float64
	VolCcy24h float64 // 24h 成交量（币）
	QuoteVol  float64 // 24h 成交额（USDT）≈ VolCcy24h * Last
}

func toF(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v
}

// toI64 字符串转 int64（空串 / 非法值返回 0）
func toI64(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func decimalsOf(s string) int {
	i := strings.IndexByte(s, '.')
	if i < 0 {
		return 0
	}
	d := len(s) - i - 1
	if d < 0 {
		d = 0
	}
	if d > 10 {
		d = 10
	}
	return d
}

// Instruments 拉全量合约（缓存 6 小时）
func (c *OKXClient) Instruments(force bool) (map[string]Instrument, error) {
	c.cacheMu.Lock()
	if !force && c.instruments != nil && time.Since(c.instLoadedAt) < 6*time.Hour {
		out := c.instruments
		c.cacheMu.Unlock()
		return out, nil
	}
	c.cacheMu.Unlock()

	raw, err := c.Get("/api/v5/public/instruments?instType=SWAP", false)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		InstID       string `json:"instId"`
		CtVal        string `json:"ctVal"`
		CtMult       string `json:"ctMult"`
		LotSz        string `json:"lotSz"`
		MinSz        string `json:"minSz"`
		TickSz       string `json:"tickSz"`
		Lever        string `json:"lever"`
		SettleCcy    string `json:"settleCcy"`
		State        string `json:"state"`
		ListTime     string `json:"listTime"`
		InstCategory string `json:"instCategory"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	m := make(map[string]Instrument, len(rows))
	for _, r := range rows {
		if !strings.HasSuffix(r.InstID, "-USDT-SWAP") {
			continue
		}
		m[r.InstID] = Instrument{
			InstID:       r.InstID,
			CtVal:        toF(r.CtVal),
			CtMult:       toF(r.CtMult),
			LotSz:        toF(r.LotSz),
			LotSzDec:     decimalsOf(r.LotSz),
			MinSz:        toF(r.MinSz),
			TickSz:       toF(r.TickSz),
			Lever:        int(toF(r.Lever)),
			SettleCcy:    r.SettleCcy,
			State:        r.State,
			ListTime:     toI64(r.ListTime),
			InstCategory: r.InstCategory,
		}
	}
	if len(m) == 0 {
		return nil, errors.New("合约列表为空")
	}
	c.cacheMu.Lock()
	c.instruments = m
	c.instLoadedAt = time.Now()
	c.cacheMu.Unlock()
	logx.Logf("INFO", "合约列表已更新：%d 个 USDT 永续", len(m))
	return m, nil
}

// Tickers 一次拿全市场行情
func (c *OKXClient) Tickers() (map[string]Ticker, error) {
	raw, err := c.Get("/api/v5/market/tickers?instType=SWAP", false)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		InstID    string `json:"instId"`
		Last      string `json:"last"`
		VolCcy24h string `json:"volCcy24h"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	m := make(map[string]Ticker, len(rows))
	for _, r := range rows {
		t := Ticker{InstID: r.InstID, Last: toF(r.Last), VolCcy24h: toF(r.VolCcy24h)}
		t.QuoteVol = t.VolCcy24h * t.Last
		m[r.InstID] = t
	}
	return m, nil
}

// Candles 拉 K 线，返回时间升序（老 → 新）
func (c *OKXClient) Candles(instID, bar string, limit int) ([]Candle, error) {
	if limit <= 0 || limit > 300 {
		limit = 300
	}
	path := "/api/v5/market/candles?instId=" + url.QueryEscape(instID) +
		"&bar=" + url.QueryEscape(bar) + "&limit=" + strconv.Itoa(limit)
	raw, err := c.Get(path, false)
	if err != nil {
		return nil, err
	}
	return parseCandles(raw)
}

// HistoryCandles 往更早翻页。afterMs = 当前最老那根的 ts，返回更早的（仍为升序）
func (c *OKXClient) HistoryCandles(instID, bar string, afterMs int64, limit int) ([]Candle, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	path := "/api/v5/market/history-candles?instId=" + url.QueryEscape(instID) +
		"&bar=" + url.QueryEscape(bar) + "&after=" + strconv.FormatInt(afterMs, 10) +
		"&limit=" + strconv.Itoa(limit)
	raw, err := c.Get(path, false)
	if err != nil {
		return nil, err
	}
	return parseCandles(raw)
}

func parseCandles(raw json.RawMessage) ([]Candle, error) {
	var rows [][]string
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	out := make([]Candle, 0, len(rows))
	for _, r := range rows {
		if len(r) < 6 {
			continue
		}
		cd := Candle{
			Ts: toInt64(r[0]),
			O:  toF(r[1]),
			H:  toF(r[2]),
			L:  toF(r[3]),
			C:  toF(r[4]),
			V:  toF(r[5]),
		}
		// confirm 在第 9 个字段（下标 8），拿不到就当已收盘
		if len(r) > 8 {
			cd.Confirm = strings.TrimSpace(r[8]) == "1"
		} else {
			cd.Confirm = true
		}
		out = append(out, cd)
	}
	// OKX 返回降序 → 反转成升序
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// CandlesEnough 抓够 minCandles 根（自动翻历史页）
func (c *OKXClient) CandlesEnough(instID, bar string, minCandles int) ([]Candle, error) {
	cands := make([]Candle, 0, minCandles+100)
	latest, err := c.Candles(instID, bar, c.cfg.CandleLimit)
	if err != nil {
		return nil, err
	}
	cands = append(cands, latest...)

	pages := c.cfg.HistoryPages
	for p := 0; p < pages && len(cands) < minCandles; p++ {
		if len(cands) == 0 {
			break
		}
		older, herr := c.HistoryCandles(instID, bar, cands[0].Ts, 100)
		if herr != nil {
			// 翻历史失败不算致命：有多少用多少
			logx.Logf("WARN", "%s %s 翻历史失败：%v", instID, bar, herr)
			break
		}
		if len(older) == 0 {
			break
		}
		cands = append(older, cands...)
	}
	return dedupeSort(cands), nil
}

func dedupeSort(in []Candle) []Candle {
	if len(in) == 0 {
		return in
	}
	seen := make(map[int64]bool, len(in))
	out := make([]Candle, 0, len(in))
	for _, cd := range in {
		if seen[cd.Ts] {
			continue
		}
		seen[cd.Ts] = true
		out = append(out, cd)
	}
	// 插入排序（数据量小，稳）
	for i := 1; i < len(out); i++ {
		v := out[i]
		j := i - 1
		for j >= 0 && out[j].Ts > v.Ts {
			out[j+1] = out[j]
			j--
		}
		out[j+1] = v
	}
	return out
}

// ---------------------------------------------------------------------------
// 账户 / 交易
// ---------------------------------------------------------------------------

type Position struct {
	InstID  string `json:"instId"`
	PosSide string `json:"posSide"`
	Pos     string `json:"pos"`
	AvgPx   string `json:"avgPx"`
	Imr     string `json:"imr"`
	Upl     string `json:"upl"`
	Lever   string `json:"lever"`
	MgnMode string `json:"mgnMode"`
	MarkPx  string `json:"markPx"`
	LiqPx   string `json:"liqPx"`
	CTime   string `json:"cTime"`
	UTime   string `json:"uTime"`
}

type Account struct {
	TotalEq      float64
	AvailEq      float64
	Upl          float64
	PosCount     int
	PositionList []Position
}

func (c *OKXClient) Positions() ([]Position, error) {
	raw, err := c.Get("/api/v5/account/positions?instType=SWAP", true)
	if err != nil {
		return nil, err
	}
	var rows []Position
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	out := make([]Position, 0, len(rows))
	for _, p := range rows {
		if toF(p.Pos) == 0 {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func (c *OKXClient) Balance() (*Account, error) {
	raw, err := c.Get("/api/v5/account/balance?ccy=USDT", true)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		TotalEq string `json:"totalEq"`
		Upl     string `json:"upl"`
		Details []struct {
			Ccy      string `json:"ccy"`
			AvailEq  string `json:"availEq"`
			AvailBal string `json:"availBal"`
			Eq       string `json:"eq"`
		} `json:"details"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, errors.New("余额返回为空")
	}
	acc := &Account{TotalEq: toF(rows[0].TotalEq), Upl: toF(rows[0].Upl)}
	for _, d := range rows[0].Details {
		if d.Ccy == "USDT" {
			acc.AvailEq = toF(d.AvailEq)
			if acc.AvailEq == 0 {
				acc.AvailEq = toF(d.AvailBal)
			}
			if acc.AvailEq == 0 {
				acc.AvailEq = toF(d.Eq)
			}
			break
		}
	}
	return acc, nil
}

func (c *OKXClient) SetLeverage(instID string, lever int, mgnMode string) error {
	if mgnMode == "" {
		mgnMode = "isolated"
	}
	_, err := c.Post("/api/v5/account/set-leverage", map[string]string{
		"instId":  instID,
		"lever":   strconv.Itoa(lever),
		"mgnMode": mgnMode,
	})
	return err
}

type OrderResult struct {
	OrdID   string
	ClOrdID string
	SCode   string
	SMsg    string
}

// PlaceOrder 市价下单（只做多 / 平多）
func (c *OKXClient) PlaceOrder(instID, tdMode, side, posSide, ordType, sz string, reduceOnly bool) (*OrderResult, error) {
	payload := map[string]string{
		"instId":  instID,
		"tdMode":  tdMode,
		"side":    side,
		"ordType": ordType,
		"sz":      sz,
	}
	if posSide != "" && posSide != "net" {
		payload["posSide"] = posSide
	}
	if reduceOnly {
		payload["reduceOnly"] = "true"
	}
	raw, err := c.Post("/api/v5/trade/order", payload)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		OrdID   string `json:"ordId"`
		ClOrdID string `json:"clOrdId"`
		SCode   string `json:"sCode"`
		SMsg    string `json:"sMsg"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, errors.New("下单返回为空")
	}
	r := &OrderResult{OrdID: rows[0].OrdID, ClOrdID: rows[0].ClOrdID, SCode: rows[0].SCode, SMsg: rows[0].SMsg}
	if r.SCode != "" && r.SCode != okxRespOK {
		return r, fmt.Errorf("下单被拒 sCode=%s %s", r.SCode, r.SMsg)
	}
	return r, nil
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func truncate(s string, n int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func toInt64(s string) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return v
}
