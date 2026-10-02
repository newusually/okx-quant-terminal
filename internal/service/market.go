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
	"finally-main/internal/ratelimit"
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
//
// 实现在 internal/ratelimit 里，而且是「进程内共享」的：
// 数据服务（回补）和策略引擎用的是两套不同的 HTTP 客户端，
// 各自持一把限速器的话速率会叠加，直接把 OKX 的 20 次/2 秒 打爆。
// 所以两边都从 ratelimit 取同一把闸门，见那个包的注释。

// ---------------------------------------------------------------------------
// 客户端
// ---------------------------------------------------------------------------

type OKXClient struct {
	cfg         *conf.Config
	http        *http.Client
	candleLimit *ratelimit.Limiter
	tradeLimit  *ratelimit.Limiter

	mu        sync.Mutex
	base      string
	timeOffMs int64
	ready     bool

	cacheMu       sync.Mutex
	instruments   map[string]Instrument
	instLoadedAt  time.Time
	lastQuoteVols map[string]float64

	// 账户持仓模式（net_mode / long_short_mode）。启动后探一次就缓存。
	//
	// 单向模式下 posSide 传 net 或不传；双向模式下**必须**传 long/short，
	// 否则 OKX 直接回 51000 Parameter posSide error —— 下单、设杠杆全被拒，
	// 表现就是「有信号但一条买入记录都没有」。
	posModeOnce sync.Once
	posMode     string
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
		candleLimit: ratelimit.Candle(), // 行情：与回补共用同一把闸门
		tradeLimit:  ratelimit.Trade(),  // 交易：独立、更保守
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

func (c *OKXClient) request(method, path string, body []byte, signed bool, lim *ratelimit.Limiter) (json.RawMessage, error) {
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
	var lim *ratelimit.Limiter
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

	// 逐仓（isolated）模式下，/account/balance 顶层的 upl 恒为 0 ——
	// OKX 只在全仓/跨币种保证金下才回填这个字段，逐仓的未实现盈亏
	// 只存在于持仓明细里。所以顶层为 0 时必须自己去 /account/positions
	// 把每条持仓的 upl 加总，否则顶栏「浮盈」永远是 0（有仓也不动）。
	if acc.Upl == 0 {
		if pos, err := c.Positions(); err == nil && len(pos) > 0 {
			sum := 0.0
			for _, p := range pos {
				sum += toF(p.Upl)
			}
			acc.Upl = sum
			acc.PositionList = pos
			acc.PosCount = len(pos)
		}
	}
	return acc, nil
}

// PosMode 探测账户持仓模式（net_mode / long_short_mode），只探一次。
//
// 用户在 OKX 后台改过持仓模式的话，这里探测到的值决定后面所有下单参数 ——
// 不写死 "net"，省得账户一切模式整条链路就哑掉。
func (c *OKXClient) PosMode() string {
	c.posModeOnce.Do(func() {
		c.posMode = "net_mode" // 探测失败时的保守默认
		raw, err := c.Get("/api/v5/account/config", true)
		if err != nil {
			logx.Logf("WARN", "读 OKX 账户配置失败，按单向持仓（posSide=net）处理：%v", err)
			return
		}
		var rows []struct {
			PosMode string `json:"posMode"`
		}
		if json.Unmarshal(raw, &rows) == nil && len(rows) > 0 && rows[0].PosMode != "" {
			c.posMode = rows[0].PosMode
		}
		// 这里不能调 EffectivePosSide（它内部又会进 PosMode → sync.Once 自锁）
		ps := "net"
		if c.posMode == "long_short_mode" {
			ps = "long"
		}
		logx.Logf("INFO", "OKX 账户持仓模式=%s → 下单 posSide 采用 %q", c.posMode, ps)
	})
	return c.posMode
}

// EffectivePosSide 把配置里写的 posSide 翻译成当前账户模式下合法的值。
//
//	双向持仓（long_short_mode）：只做多 → long；传 net 会被 OKX 拒（51000）
//	单向持仓（net_mode）：net
func (c *OKXClient) EffectivePosSide(want string) string {
	if c.PosMode() == "long_short_mode" {
		if want == "" || want == "net" {
			return "long"
		}
		return want
	}
	return "net"
}

func (c *OKXClient) SetLeverage(instID string, lever int, mgnMode string) error {
	// ★ 只读板块硬闸：NQ 这类外部数据合约连杠杆都不该去动
	if IsReadonlyInst(instID) {
		return fmt.Errorf("合约 %s 属于只读板块（只展示不交易），已拒绝设置杠杆", instID)
	}
	if mgnMode == "" {
		mgnMode = "isolated"
	}
	p := map[string]string{
		"instId":  instID,
		"lever":   strconv.Itoa(lever),
		"mgnMode": mgnMode,
	}
	// 双向持仓模式下 posSide 是必填项，缺了 OKX 直接回 51000
	if c.PosMode() == "long_short_mode" {
		p["posSide"] = "long"
	}
	_, err := c.Post("/api/v5/account/set-leverage", p)
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
	// ★★ 只读板块硬闸 ★★
	//
	// 这里是所有自动下单路径的**唯一咽喉**：开仓（trader.runEntries）、
	// 平仓（closeOne）、加仓（runAddons）最后都落到这个方法。
	//
	// 上游（扫描/准入）本来就不会选中只读合约，但那是「靠上游不出错」。
	// 在这里再拦一道，是为了将来有人新增一条任务直接调下单时也不会漏 ——
	// 「只展示不交易」是产品约束，必须在最靠近交易所的那一层兜住。
	if IsReadonlyInst(instID) {
		return nil, fmt.Errorf("合约 %s 属于只读板块（只展示不交易），已拒绝下单", instID)
	}
	payload := map[string]string{
		"instId":  instID,
		"tdMode":  tdMode,
		"side":    side,
		"ordType": ordType,
		"sz":      sz,
	}
	// posSide 要按账户实际持仓模式翻译：
	// 双向模式传 net 会被拒，必须传 long（本策略只做多；平仓时 long + reduceOnly）
	if ps := c.EffectivePosSide(posSide); ps != "" && ps != "net" {
		payload["posSide"] = ps
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

// ---------------------------------------------------------------------------
// 交易链路自检
// ---------------------------------------------------------------------------

// ProbeTrading 交易链路自检：探测账户持仓模式，并试设一次杠杆（不下单）。
//
// 给命令行 / 运维用：不开网页、不下单也能确认「下单参数会不会被 OKX 拒」。
// 起因是账户切到双向持仓后 posSide=net 被 OKX 直接拒（51000 Parameter
// posSide error），表现就是「图上有信号、后台也在扫，但一条买入记录都没有」。
// 这个自检就是防这种「参数不对但没人知道」的情况。
func ProbeTrading(instID string, lever int) (string, error) {
	cfg := conf.LoadConfig()
	if cfg == nil {
		return "", errors.New("读不到配置（configs/okx_strategy.json）")
	}
	cli, err := newOKXClient(cfg)
	if err != nil {
		return "", err
	}
	if err := cli.EnsureReady(); err != nil {
		return "", fmt.Errorf("连接 OKX 失败：%w", err)
	}
	mode := cli.PosMode()
	if instID == "" {
		return mode, nil
	}
	if lever <= 0 {
		lever = cfg.Entry.Leverage
	}
	if err := cli.SetLeverage(instID, lever, cfg.Entry.TdMode); err != nil {
		return mode, fmt.Errorf("设杠杆失败：%w", err)
	}
	return mode, nil
}

// ProbeAccountDiag 只读诊断：把 OKX 那边和「权益 / 浮盈」相关的真实字段全打出来。
//
// 为什么需要它：OKX 的「全仓 / 逐仓」和「账户级 upl 有没有值」不是一回事 ——
// /account/balance 顶层的 upl 只在跨币种保证金（acctLv=3/4）下才回填，
// 单币种保证金（acctLv=2）哪怕仓位是全仓，顶层 upl 也恒为 0，
// 真实浮盈只存在于 /account/positions 的每条持仓里。
// 不看这几个原始字段光猜，会一直在「为什么浮盈是 0」上面打转。
func ProbeAccountDiag() ([]string, error) {
	cfg := conf.LoadConfig()
	if cfg == nil {
		return nil, errors.New("读不到配置（configs/okx_strategy.json）")
	}
	cli, err := newOKXClient(cfg)
	if err != nil {
		return nil, err
	}
	if err := cli.EnsureReady(); err != nil {
		return nil, fmt.Errorf("连接 OKX 失败：%w", err)
	}
	out := []string{}

	// ① 账户配置：保证金模式 acctLv + 持仓模式 posMode
	acctLv, posMode := "", ""
	if raw, err := cli.Get("/api/v5/account/config", true); err == nil {
		var rows []struct {
			AcctLv  string `json:"acctLv"`
			PosMode string `json:"posMode"`
		}
		if json.Unmarshal(raw, &rows) == nil && len(rows) > 0 {
			acctLv, posMode = rows[0].AcctLv, rows[0].PosMode
		}
	}
	lvName := map[string]string{
		"1": "简单交易模式", "2": "单币种保证金模式",
		"3": "跨币种保证金模式", "4": "组合保证金模式",
	}[acctLv]
	out = append(out, fmt.Sprintf("账户保证金模式 acctLv=%s（%s）", acctLv, lvName))
	out = append(out, fmt.Sprintf("持仓模式 posMode=%s", posMode))

	// ② 账户余额顶层 upl（跨币种模式下才有值）
	var topUpl, topEq float64
	if raw, err := cli.Get("/api/v5/account/balance?ccy=USDT", true); err == nil {
		var rows []struct {
			TotalEq string `json:"totalEq"`
			Upl     string `json:"upl"`
		}
		if json.Unmarshal(raw, &rows) == nil && len(rows) > 0 {
			topEq, topUpl = toF(rows[0].TotalEq), toF(rows[0].Upl)
		}
	}
	out = append(out, fmt.Sprintf("账户余额 totalEq=%.6f 顶层 upl=%.6f  ← 单币种保证金模式下这里恒为 0", topEq, topUpl))

	// ③ 持仓明细：这才是浮盈的真实来源
	pos, err := cli.Positions()
	if err != nil {
		return out, fmt.Errorf("读持仓失败：%w", err)
	}
	sum := 0.0
	out = append(out, fmt.Sprintf("持仓 %d 条：", len(pos)))
	for _, p := range pos {
		upl := toF(p.Upl)
		sum += upl
		out = append(out, fmt.Sprintf("  · %s mgnMode=%s posSide=%s pos=%s avgPx=%s markPx=%s upl=%.6f imr=%s",
			p.InstID, p.MgnMode, p.PosSide, p.Pos, p.AvgPx, p.MarkPx, upl, p.Imr))
	}
	out = append(out, fmt.Sprintf("持仓汇总 upl=%.6f  ← 顶栏「浮盈」就该用这个值", sum))

	// ④ 爆仓风险巡检（★ 2026-10-02 四期新增，回答用户「不准爆仓」）
	//
	// 先说结论：**交易所的强平是 OKX 单方面执行的，程序改不了它的开关** ——
	// 任何「不准爆仓」都只能是「让爆仓概率低到事实上不发生」，而不是「关掉它」。
	// 能做的就是把这个距离量化出来，别靠感觉。
	//
	// 全仓（cross）下强平看的是**账户整体**：调整后权益 adjEq 跌破维持保证金 mmr
	// 才触发，跟单个仓位亏多少没有直接关系。
	//   ★ 安全垫 = adjEq − mmr
	//   ★ 名义敞口 ≈ Σ(imr × lever)，安全垫 ÷ 名义敞口 = 「全部持仓同时反向跌多少 % 才吃光它」
	var adjEq, availEq, mmr, mgnRatioRaw float64
	if raw, err := cli.Get("/api/v5/account/balance?ccy=USDT", true); err == nil {
		var rows []struct {
			AdjEq   string `json:"adjEq"`
			AvailEq string `json:"availEq"`
			MMR     string `json:"mmr"`
			TotalEq string `json:"totalEq"`
			Details []struct {
				Ccy      string `json:"ccy"`
				AvailBal string `json:"availBal"`
				AvailEq  string `json:"availEq"`
				EqUsd    string `json:"eqUsd"`
				MMR      string `json:"mmr"`
				MgnRatio string `json:"mgnRatio"`
			} `json:"details"`
		}
		if json.Unmarshal(raw, &rows) == nil && len(rows) > 0 {
			r := rows[0]
			adjEq, availEq = toF(r.AdjEq), toF(r.AvailEq)
			mmr = toF(r.MMR)
			// ★ 单币种保证金模式（acctLv=2）下**顶层 adjEq / availEq / mmr 全是空串**，
			//   真正有值的是 details[USDT] 里的同名（或 availBal / eqUsd）字段。
			//   不在这里兜底的话，上面几个数字会整排显示 0 —— 看上去像「权益 0、
			//   时刻会爆仓」，比不显示更吓人，也更误导。
			for _, d := range r.Details {
				if d.Ccy != "USDT" {
					continue
				}
				if adjEq <= 0 {
					adjEq = toF(d.EqUsd)
				}
				if availEq <= 0 {
					availEq = toF(d.AvailEq)
				}
				if availEq <= 0 {
					availEq = toF(d.AvailBal)
				}
				if mmr <= 0 {
					mmr = toF(d.MMR)
				}
				mgnRatioRaw = toF(d.MgnRatio)
				break
			}
			if adjEq <= 0 {
				adjEq = toF(r.TotalEq)
			}
		}
	}
	imrSum, notional := 0.0, 0.0
	for _, p := range pos {
		imr := toF(p.Imr)
		imrSum += imr
		if lev := toF(p.Lever); lev > 0 {
			notional += imr * lev
		}
	}
	out = append(out, "")
	out = append(out, "== 爆仓风险巡检（全仓 cross：adjEq 跌破 mmr 才强平）==")
	out = append(out, fmt.Sprintf("  调整后权益 adjEq=%.4f  维持保证金 mmr=%.4f  可用 availEq=%.4f  （OKX 原始 mgnRatio=%.4f）",
		adjEq, mmr, availEq, mgnRatioRaw))
	cover := adjEq - mmr
	out = append(out, fmt.Sprintf("  ★ 安全垫 = adjEq − mmr = %.4fU（> 0 就没爆仓，越厚越安全）", cover))
	if notional > 0 {
		out = append(out, fmt.Sprintf("  名义敞口 ≈ %.2fU（Σ imr×lever）→ 全部持仓**同时**反向跌 %.2f%% 才会吃光安全垫",
			notional, cover/notional*100))
	}
	if adjEq > 0 {
		out = append(out, fmt.Sprintf("  维持保证金占权益 %.2f%%（越低越安全；100%% 才强平）", mmr/adjEq*100))
	}
	if imrSum > 0 && adjEq > 0 {
		out = append(out, fmt.Sprintf("  占用保证金 %.4fU / 权益 %.4fU = %.2f%%（对应风控 risk.max_total_margin_pct）",
			imrSum, adjEq, imrSum/adjEq*100))
	}
	liqLines := []string{}
	for _, p := range pos {
		if lp := toF(p.LiqPx); lp > 0 {
			liqLines = append(liqLines, fmt.Sprintf("%s@%.4f", p.InstID, lp))
		}
	}
	if len(liqLines) > 0 {
		out = append(out, "  单仓强平价："+strings.Join(liqLines, "  "))
	} else {
		out = append(out, "  单仓强平价：无（全仓 cross 下 OKX 不返回单仓 liqPx，只看账户整体）")
	}
	return out, nil
}
