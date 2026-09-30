package service

// scanner.go —— 全合约扫描（对应文案 §6 三阶段流程）
//
//	阶段 1  拉合约列表（缓存 6 小时）
//	阶段 2  一次 /market/tickers 拿全市场行情，按 24h 成交额粗筛出前 N 名
//	阶段 3  只对候选逐个拉 K 线算 8 因子

import (
	"sort"
	"sync"
	"time"

	"finally-main/internal/conf"
	"finally-main/internal/logx"
	"finally-main/internal/model"
)

// ScanResult 一轮扫描的结果
type ScanResult struct {
	Universe   int       // 全市场 USDT 永续合约数
	Candidates int       // 粗筛后的候选数
	Scanned    int       // 真正算了信号的合约数
	Signals    []*Signal // score >= 阈值 的信号（按分数、时间倒序）
	Failed     int       // 抓 K 线失败的合约数
}

// Scan 执行一轮扫描
func Scan(cfg *conf.Config, cli *OKXClient, bar string) (*ScanResult, error) {
	res := &ScanResult{Signals: []*Signal{}}

	insts, err := cli.Instruments(false)
	if err != nil {
		return res, err
	}
	tickers, err := cli.Tickers()
	if err != nil {
		return res, err
	}
	res.Universe = len(insts)

	exclude := map[string]bool{}
	for _, e := range cfg.ExcludeInst {
		exclude[e] = true
	}

	// 转成 model.Instrument 后统一走 universe.go 的准入过滤：
	//   不买美股/ETF/商品 · 不买 30 天内新上线 · 不买要下线的 · 0.1U 必须买得起
	mList := make([]model.Instrument, 0, len(insts))
	for _, ins := range insts {
		mList = append(mList, model.Instrument{
			InstID:       ins.InstID,
			BaseCcy:      SymbolOf(ins.InstID),
			SettleCcy:    ins.SettleCcy,
			CtVal:        ins.CtVal,
			CtMult:       ins.CtMult,
			LotSz:        ins.LotSz,
			MinSz:        ins.MinSz,
			TickSz:       ins.TickSz,
			Lever:        ins.Lever,
			State:        ins.State,
			ListTime:     ins.ListTime,
			InstCategory: ins.InstCategory,
		})
	}
	mTickers := make(map[string]model.Ticker, len(tickers))
	for id, tk := range tickers {
		mTickers[id] = model.Ticker{InstID: id, Last: tk.Last, QuoteVol24h: tk.QuoteVol}
	}

	// 下线名单（有缓存就走缓存，不重复打公告接口）
	var delistSet map[string]DelistEntry
	if cfg.ExcludeDelisting {
		uni := make(map[string]bool, len(mList))
		for _, it := range mList {
			uni[it.BaseCcy] = true
		}
		delistSet = DelistSymbolSet(LoadDelistList(cli.AnnouncementFetcher(), nil, uni, cfg.ExcludeNewListingDays+30))
	}

	policy := UniversePolicy{
		ExcludeStockETF:       cfg.ExcludeStockETF,
		ExcludeNewListingDays: cfg.ExcludeNewListingDays,
		ExcludeDelisting:      cfg.ExcludeDelisting,
		MarginUSDT:            cfg.Entry.MarginUSDT,
		Leverage:              cfg.Entry.Leverage,
		MaxMarginUSDT:         cfg.OrderMarginCap(),
		MinQuoteVolume24h:     cfg.MinQuoteVolume24h,
		ExtraExclude:          cfg.ExcludeInst,
	}
	kept, fst := FilterUniverse(mList, mTickers, delistSet, policy)
	if fst.DroppedCategory+fst.DroppedNew+fst.DroppedDelist+fst.DroppedNotional > 0 {
		logx.Logf("INFO", "合约准入过滤：%d → %d（美股ETF -%d，新上线 -%d，待下线 -%d，资金不够 -%d，非live -%d）",
			fst.Total, fst.Kept, fst.DroppedCategory, fst.DroppedNew,
			fst.DroppedDelist, fst.DroppedNotional, fst.DroppedState)
	}

	type cand struct {
		inst Instrument
		vol  float64
	}
	cands := make([]cand, 0, len(kept))
	for _, it := range kept {
		ins, ok := insts[it.InstID]
		if !ok {
			continue
		}
		if exclude[it.InstID] {
			continue
		}
		tk, ok := tickers[it.InstID]
		if !ok || tk.Last <= 0 {
			continue
		}
		cands = append(cands, cand{inst: ins, vol: tk.QuoteVol})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].vol > cands[j].vol })
	if cfg.TopNByVolume > 0 && len(cands) > cfg.TopNByVolume {
		cands = cands[:cfg.TopNByVolume]
	}
	res.Candidates = len(cands)

	nowMs := cli.nowMs()
	workers := cfg.Workers
	if workers < 1 {
		workers = 1
	}
	if workers > len(cands) && len(cands) > 0 {
		workers = len(cands)
	}

	type job struct {
		ins Instrument
	}
	jobs := make(chan job)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				cands, err := cli.CandlesEnough(j.ins.InstID, bar, cfg.MinCandles)
				if err != nil {
					mu.Lock()
					res.Failed++
					mu.Unlock()
					logx.Logf("WARN", "%s 抓 K 线失败：%v", j.ins.InstID, err)
					continue
				}
				if len(cands) < cfg.MinCandles {
					mu.Lock()
					res.Failed++
					mu.Unlock()
					continue
				}
				idx := IndexOfLastClosed(cands, bar, nowMs)
				if idx < 0 {
					mu.Lock()
					res.Failed++
					mu.Unlock()
					continue
				}
				sig := ComputeSignal(j.ins.InstID, bar, cands, idx)
				if !sig.Ready {
					mu.Lock()
					res.Failed++
					mu.Unlock()
					continue
				}
				mu.Lock()
				res.Scanned++
				if sig.Score >= cfg.ThresholdFor(j.ins.InstID) {
					res.Signals = append(res.Signals, sig)
				}
				mu.Unlock()
			}
		}()
	}
	for _, c := range cands {
		jobs <- job{ins: c.inst}
	}
	close(jobs)
	wg.Wait()

	sort.Slice(res.Signals, func(i, j int) bool {
		if res.Signals[i].Score != res.Signals[j].Score {
			return res.Signals[i].Score > res.Signals[j].Score
		}
		return res.Signals[i].Ts > res.Signals[j].Ts
	})

	return res, nil
}

// LatestSignal 只为某个合约算一次最新信号（出场判定用）
func LatestSignal(cfg *conf.Config, cli *OKXClient, instID, bar string) (*Signal, []Candle, error) {
	cands, err := cli.CandlesEnough(instID, bar, cfg.MinCandles)
	if err != nil {
		return nil, nil, err
	}
	if len(cands) == 0 {
		return nil, nil, nil
	}
	idx := IndexOfLastClosed(cands, bar, cli.nowMs())
	if idx < 0 {
		return nil, cands, nil
	}
	return ComputeSignal(instID, bar, cands, idx), cands, nil
}

// LastClosed 返回最后一根已收盘 K 线（没有则返回 nil）
func LastClosed(cands []Candle, bar string, nowMs int64) *Candle {
	idx := IndexOfLastClosed(cands, bar, nowMs)
	if idx < 0 {
		return nil
	}
	cd := cands[idx]
	return &cd
}

// sleepInterval 把「每轮扫描间隔」补足（扫描很快时别把 CPU/请求打满）
func sleepInterval(start time.Time, sec int) {
	if sec <= 0 {
		return
	}
	want := time.Duration(sec) * time.Second
	used := time.Since(start)
	if used < want {
		time.Sleep(want - used)
	}
}
