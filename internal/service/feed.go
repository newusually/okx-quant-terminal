package service

// feed.go —— OKX 公共行情接入（走本项目自己的 okx Go SDK）
//
// 这里刻意不用 python 版的 okx 包，也不用 market.go 里的私有实现，
// 而是直接用 internal/repo/okx 这套 Go SDK —— 顺便当成它的集成测试。

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"finally-main/internal/model"
	"finally-main/internal/repo/okx"
)

// DataFeed OKX 公共行情客户端
type DataFeed struct {
	market *okx.MarketAPI
	pub    *okx.PublicAPI
	proxy  string
}

// NewDataFeed proxy 形如 "http://127.0.0.1:7890" / "socks5://127.0.0.1:1080"，留空直连
//
// 【重要】这里必须用 FlagLive（实盘标志）。
// 之前误用 FlagDemo，OKX 会带上 x-simulated-trading:1 头，
// 结果是只返回「模拟盘」的 189 个合约（USDT-SWAP 仅 153 个），
// 而不是实盘的 493 个（USDT-SWAP 478 个）—— 合约列表会少一大半。
// 模拟盘标志只该用于账户/交易接口，公共行情一律实盘。
func NewDataFeed(proxy string) *DataFeed {
	// RatePerSecond 必须 <= OKX 最紧的那档限频：history-candles 是 20 次 / 2 秒，
	// 也就是 10 次/秒。这里压到 9，留一点余量给 tickers / 实时续 K 线。
	// 回补一个 (合约,周期) 要翻几百页，跑快了会被 429，反而更慢。
	opts := okx.ClientOptions{Proxy: proxy, Timeout: 25 * time.Second, MaxRetries: 5, RatePerSecond: 9}
	return &DataFeed{
		market: okx.NewMarketAPIWith("", "", "", false, okx.FlagLive, opts),
		pub:    okx.NewPublicAPIWith("", "", "", false, okx.FlagLive, opts),
		proxy:  proxy,
	}
}

// ---------------------------------------------------------------------------
// 合约列表
// ---------------------------------------------------------------------------

// FetchInstruments 拉全部 USDT 永续合约。对应 /api/v5/public/instruments?instType=SWAP
func (f *DataFeed) FetchInstruments() ([]model.Instrument, error) {
	resp, err := f.pub.GetInstruments(okx.InstTypeSwap, "", "")
	if err != nil {
		return nil, err
	}
	rows := okx.Data(resp)
	out := make([]model.Instrument, 0, len(rows))
	for _, r := range rows {
		instID := okx.Str(r, "instId")
		// 只要 USDT 本位永续，和策略引擎的扫描范围保持一致
		if !strings.HasSuffix(instID, "-USDT-SWAP") {
			continue
		}
		it := model.Instrument{
			InstID:    instID,
			BaseCcy:   okx.Str(r, "baseCcy"),
			QuoteCcy:  okx.Str(r, "quoteCcy"),
			SettleCcy: okx.Str(r, "settleCcy"),
			CtVal:     okx.F(r, "ctVal"),
			CtMult:    okx.F(r, "ctMult"),
			LotSz:     okx.F(r, "lotSz"),
			MinSz:     okx.F(r, "minSz"),
			TickSz:    okx.F(r, "tickSz"),
			Lever:     okx.I(r, "lever"),
			State:     okx.Str(r, "state"),
			ListTime:  okx.ToInt64(okx.Str(r, "listTime")),
			// instCategory: 1=加密 3=美股/ETF 4=商品。「不买 ETF 和美股」靠它过滤
			InstCategory: okx.Str(r, "instCategory"),
		}
		if it.BaseCcy == "" {
			it.BaseCcy = strings.TrimSuffix(instID, "-USDT-SWAP")
		}
		if it.QuoteCcy == "" {
			it.QuoteCcy = "USDT"
		}
		out = append(out, it)
	}
	if len(out) == 0 {
		return nil, errors.New("合约列表为空")
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 行情
// ---------------------------------------------------------------------------

// FetchTickers 一次拿全市场行情，对应 /api/v5/market/tickers?instType=SWAP
func (f *DataFeed) FetchTickers() ([]model.Ticker, error) {
	resp, err := f.market.GetTickers(okx.InstTypeSwap, "")
	if err != nil {
		return nil, err
	}
	rows := okx.Data(resp)
	now := time.Now().UnixMilli()
	out := make([]model.Ticker, 0, len(rows))
	for _, r := range rows {
		instID := okx.Str(r, "instId")
		if !strings.HasSuffix(instID, "-USDT-SWAP") {
			continue
		}
		t := model.Ticker{
			InstID:    instID,
			Ts:        now,
			Last:      okx.F(r, "last"),
			Open24h:   okx.F(r, "open24h"),
			High24h:   okx.F(r, "high24h"),
			Low24h:    okx.F(r, "low24h"),
			Vol24h:    okx.F(r, "vol24h"),
			VolCcy24h: okx.F(r, "volCcy24h"),
		}
		if t.Last > 0 {
			t.QuoteVol24h = t.VolCcy24h * t.Last
		}
		if t.Open24h > 0 {
			t.ChgPct = (t.Last/t.Open24h - 1) * 100
		}
		out = append(out, t)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// K 线
// ---------------------------------------------------------------------------

// SupportedBars 前端下拉用的周期（顺序即按钮顺序）
var SupportedBars = []string{"1m", "3m", "5m", "15m", "1H", "4H"}

// BarDuration 周期 → 时长
func BarDuration(bar string) time.Duration {
	b := strings.TrimSpace(bar)
	if len(b) < 2 {
		return 0
	}
	unit := b[len(b)-1]
	num := 0
	for i := 0; i < len(b)-1; i++ {
		if b[i] < '0' || b[i] > '9' {
			return 0
		}
		num = num*10 + int(b[i]-'0')
	}
	if num <= 0 {
		return 0
	}
	switch unit {
	case 'm':
		return time.Duration(num) * time.Minute
	case 'H', 'h':
		return time.Duration(num) * time.Hour
	case 'D', 'd':
		return time.Duration(num) * 24 * time.Hour
	}
	return 0
}

// IsSupportedBar 校验周期
func IsSupportedBar(bar string) bool {
	for _, b := range SupportedBars {
		if b == bar {
			return true
		}
	}
	return false
}

// parseKlineRows 把 OKX 的 [[ts,o,h,l,c,vol,volCcy,volCcyQuote,confirm], ...] 解析成 []model.Kline
//
// OKX 返回是降序（新 → 老），这里统一翻成升序（老 → 新）。
func parseKlineRows(instID, bar string, rows [][]string) []model.Kline {
	out := make([]model.Kline, 0, len(rows))
	for _, r := range rows {
		if len(r) < 6 {
			continue
		}
		out = append(out, model.Kline{
			InstID: instID,
			Bar:    bar,
			Ts:     okx.ToInt64(r[0]),
			O:      okx.ToFloat(r[1]),
			H:      okx.ToFloat(r[2]),
			L:      okx.ToFloat(r[3]),
			C:      okx.ToFloat(r[4]),
			V:      okx.ToFloat(r[5]),
		})
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// toRows 从 okx SDK 返回的 map 里抠出 data 里的字符串数组
func toRows(resp map[string]any) [][]string {
	raw, ok := resp["data"]
	if !ok || raw == nil {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([][]string, 0, len(arr))
	for _, item := range arr {
		inner, ok := item.([]any)
		if !ok {
			continue
		}
		row := make([]string, 0, len(inner))
		for _, v := range inner {
			if s, ok := v.(string); ok {
				row = append(row, s)
			} else {
				row = append(row, fmt.Sprint(v))
			}
		}
		out = append(out, row)
	}
	return out
}

// FetchCandles 最新 K 线，最多 300 根。对应 /api/v5/market/candles
func (f *DataFeed) FetchCandles(instID, bar string, limit int) ([]model.Kline, error) {
	if limit <= 0 || limit > 300 {
		limit = 300
	}
	resp, err := f.market.GetCandlesticks(instID, "", "", bar, fmt.Sprint(limit))
	if err != nil {
		return nil, err
	}
	return parseKlineRows(instID, bar, toRows(resp)), nil
}

// FetchHistoryCandles 翻历史页。afterMs 传当前最老那根的 ts，返回更早的一批。
//
// 对应 /api/v5/market/history-candles?after=<ts>。OKX 这个接口单次最多 100 根。
func (f *DataFeed) FetchHistoryCandles(instID, bar string, afterMs int64, limit int) ([]model.Kline, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	resp, err := f.market.GetHistoryCandlesticks(instID, fmt.Sprint(afterMs), "", bar, fmt.Sprint(limit))
	if err != nil {
		return nil, err
	}
	return parseKlineRows(instID, bar, toRows(resp)), nil
}

// Ping 连通性自检
func (f *DataFeed) Ping() (int64, error) {
	resp, err := f.pub.GetSystemTime()
	if err != nil {
		return 0, err
	}
	rows := okx.Data(resp)
	if len(rows) == 0 {
		return 0, errors.New("时间接口返回为空")
	}
	return okx.ToInt64(okx.Str(rows[0], "ts")), nil
}
