package service

// nq_intraday.go —— NQ（纳斯达克100）**当天盘中**数据源：Yahoo Finance ^NDX。
//
// ---------------------------------------------------------------------------
// 为什么要有这个文件（2026-10-03 二十一期）
// ---------------------------------------------------------------------------
// Dukascopy 的日文件（BID_candles_min_1.bi5）在 UTC 日切之前拿不到：
// 实测一整天里「当天」要么 503/挂起、要么 404（被 SyncNQOnce 计成休市），
// 结果库里永远停在「昨天 23:55 UTC」，网页上的 NQ 图永远慢一天 ——
// 用户凌晨看盘，图上最新一根是早上 7:55（北京时间），看起来像「数据没补」。
//
// 修复：加一条**独立**的盘中刷新线，数据源 Yahoo chart API 的 ^NDX
// （纳斯达克100 现货指数，与 Dukascopy 的 USATECHIDXUSD 同标的同价位口径，
//  实测 10-01 两侧都在 30500 区间，可无缝衔接）。每 5 分钟拉一次 1 分钟线，
// 只写「UTC 今天」的 3m/5m/15m —— 已完成的历史天仍由 Dukascopy 负责，
// UTC 日切后自然交接，两个源不会互相覆盖打架。
//
// 注意：Yahoo 不带浏览器 UA 会被 Edge CDN 限流（实测 HTTP 429 "Edge: Too
// Many Requests"），带上 UA 实测稳定 200。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"finally-main/internal/model"
	"finally-main/internal/repo"
)

const (
	// nqIntradayInterval 盘中刷新间隔。1 分钟粒度的数据 5 分钟刷一次足够看盘用，
	// 对 Yahoo 的请求压力约 288 次/天，远低于它的容忍度。
	nqIntradayInterval = 5 * time.Minute

	// nqIntradayURL Yahoo chart API：^NDX 1 分钟线，最近 2 天，含盘前盘后。
	nqIntradayURL = "https://query1.finance.yahoo.com/v8/finance/chart/%5ENDX?interval=1m&range=2d&includePrePost=true"

	// nqIntradayUA 不带 UA 会被 Edge CDN 429（实测），伪装成浏览器。
	nqIntradayUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"

	// nqIntradayFailLogGap 失败日志的节流间隔：失败每 5 分钟都会发生，
	// 全打会刷屏，30 分钟一条足够定位问题。
	nqIntradayFailLogGap = 30 * time.Minute
)

// yahooChart Yahoo chart API 的响应子集（只取用到的字段）。
type yahooChart struct {
	Chart struct {
		Result []struct {
			Meta struct {
				RegularMarketTime int64 `json:"regularMarketTime"`
			} `json:"meta"`
			Timestamp []int64 `json:"timestamp"`
			Indicators struct {
				Quote []struct {
					Open   []*float64 `json:"open"`
					High   []*float64 `json:"high"`
					Low    []*float64 `json:"low"`
					Close  []*float64 `json:"close"`
					Volume []*float64 `json:"volume"`
				} `json:"quote"`
			} `json:"indicators"`
		} `json:"result"`
		Error *struct {
			Code string `json:"code"`
			Desc string `json:"description"`
		} `json:"error"`
	} `json:"chart"`
}

// fetchYahooNDXMinutes 拉最近 2 天的 ^NDX 1 分钟线，按「UTC 天零点(秒)」分组返回。
//
// 同一分钟可能出现两条（一条全 null 一条有值，实测见过），取后一条（last-wins）；
// 缺字段 / 不自洽（h<l、非正数）的直接丢，和 decodeBi5 同一套校验底线。
func fetchYahooNDXMinutes(ctx context.Context) (map[int64][]dukaMin, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, nqIntradayURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", nqIntradayUA)
	req.Header.Set("Accept", "application/json")

	hc := &http.Client{Timeout: 20 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body) // 读完才好复用连接
		return nil, fmt.Errorf("Yahoo HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	return parseYahooNDXMinutes(body)
}

// parseYahooNDXMinutes 把 chart API 的响应体解析成「UTC 天零点(秒) → 1 分钟记录」。
// 抽成纯函数是为了能直接用真实响应的截屏做单测（见 nq_intraday_test.go）。
func parseYahooNDXMinutes(body []byte) (map[int64][]dukaMin, error) {
	var yc yahooChart
	if err := json.Unmarshal(body, &yc); err != nil {
		return nil, fmt.Errorf("JSON 解析失败：%w", err)
	}
	if yc.Chart.Error != nil {
		return nil, fmt.Errorf("Yahoo 错误：%s %s", yc.Chart.Error.Code, yc.Chart.Error.Desc)
	}
	if len(yc.Chart.Result) == 0 || len(yc.Chart.Result[0].Timestamp) == 0 {
		return nil, fmt.Errorf("Yahoo 返回空结果")
	}
	r := yc.Chart.Result[0]
	if len(r.Indicators.Quote) == 0 {
		return nil, fmt.Errorf("Yahoo 返回没有 quote")
	}
	q := r.Indicators.Quote[0]

	out := make(map[int64][]dukaMin) // key: UTC 天零点(秒)
	seen := make(map[int64]int)      // 分钟起点 ts → 该天切片里的下标（同分钟 last-wins）
	for i, rawTs := range r.Timestamp {
		ts := rawTs / 60 * 60 // 归整到分钟起点（实测同一分钟会出现两条，秒级 ts 不同）
		o, h, l, c := q.Open[i], q.High[i], q.Low[i], q.Close[i]
		if o == nil || h == nil || l == nil || c == nil {
			continue
		}
		if *o <= 0 || *h <= 0 || *l <= 0 || *c <= 0 || *h < *l {
			continue
		}
		daySec := (ts / 86400) * 86400 // UTC 天零点（秒）
		rec := dukaMin{
			Off: int(ts - daySec),
			O:   *o, H: *h, L: *l, C: *c,
		}
		if q.Volume[i] != nil {
			rec.V = *q.Volume[i]
		}
		list := out[daySec]
		if idx, dup := seen[ts]; dup && len(list) > idx {
			list[idx] = rec // 同一分钟重复：后值覆盖
			continue
		}
		seen[ts] = len(list)
		out[daySec] = append(list, rec)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("Yahoo 返回 %d 根但没有一条自洽", len(r.Timestamp))
	}
	return out, nil
}

// SyncNQIntradayOnce 盘中刷新一轮：Yahoo ^NDX 1m → 聚合 3m/5m/15m → 只写 UTC 今天。
//
// 只写「今天」的原因：已完成的历史天由 Dukascopy 全权负责（更权威、覆盖全天 24h），
// 如果 Yahoo 也写历史天，两个源会每 5 分钟互相覆盖同一批 K 线；
// UTC 日切后 Yahoo 自然停写昨天，Dukascopy 下一轮把昨天补成完整天 —— 无缝交接。
func SyncNQIntradayOnce(ctx context.Context, db *repo.DB, logf func(string, ...any)) (map[string]int, int64, error) {
	byDay, err := fetchYahooNDXMinutes(ctx)
	if err != nil {
		return nil, 0, err
	}

	now := time.Now().UTC()
	todaySec := (now.Unix() / 86400) * 86400
	recs, ok := byDay[todaySec]
	if !ok || len(recs) == 0 {
		// 今天还没有任何 1m 数据（美东盘前凌晨属正常），安静返回
		return nil, 0, nil
	}

	dayStartMs := todaySec * 1000
	var maxTs int64
	rows := make([]model.Kline, 0, len(recs)*3)
	for _, bar := range NQBars {
		ks := aggregateNQDay(recs, dayStartMs, bar)
		if len(ks) == 0 {
			continue
		}
		rows = append(rows, ks...)
		if last := ks[len(ks)-1].Ts; last > maxTs {
			maxTs = last
		}
	}
	if len(rows) == 0 {
		return nil, 0, nil
	}
	wrote := make(map[string]int, len(NQBars))
	for _, bar := range NQBars {
		part := make([]model.Kline, 0, 512)
		for _, k := range rows {
			if k.Bar == bar {
				part = append(part, k)
			}
		}
		if len(part) == 0 {
			continue
		}
		n, err := db.UpsertKlines(part)
		if err != nil {
			logf("盘中 %s 写库失败：%v", bar, err)
			continue
		}
		wrote[bar] = n
	}
	return wrote, maxTs, nil
}

// StartNQIntraday 启动 NQ 当天盘中刷新（每 5 分钟一轮，独立于 Dukascopy 同步线）。
//
// 日志纪律：数据有推进（最新一根 ts 变新）才记 INFO；失败 30 分钟最多记一条
// —— 5 分钟一轮的循环如果每次都写日志，一天 288 条全是噪音。
func StartNQIntraday(ctx context.Context, db *repo.DB, logf func(string, ...any)) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logf("盘中刷新协程退出：%v", r)
			}
		}()

		// 错开启动高峰：合约同步 / K 线回补 / Dukascopy 首轮回补都在抢网络
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Second):
		}

		var lastMaxTs int64
		var lastFailLog time.Time

		for {
			func() {
				defer func() {
					if r := recover(); r != nil {
						logf("盘中刷新单轮 panic：%v", r)
					}
				}()
				wrote, maxTs, err := SyncNQIntradayOnce(ctx, db, logf)
				if err != nil {
					if ctx.Err() == nil && time.Since(lastFailLog) >= nqIntradayFailLogGap {
						lastFailLog = time.Now()
						logf("盘中刷新失败（下轮重试）：%v", err)
					}
					return
				}
				if maxTs > lastMaxTs {
					lastMaxTs = maxTs
					logf("盘中刷新：写入 %v，最新 %s（Yahoo ^NDX）", wrote,
						time.UnixMilli(maxTs).UTC().Format("01-02 15:04 UTC"))
				}
			}()

			select {
			case <-ctx.Done():
				return
			case <-time.After(nqIntradayInterval):
			}
		}
	}()
}
