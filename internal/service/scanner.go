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

	// FromDB / FromNet 这一轮 K 线分别来自本地库 / OKX 网络。
	// 用来在日志和 /api/perf 里确认「本地优先」是不是真的生效了
	// —— 只有 FromDB 占绝大多数，这次改造才算成功。
	FromDB  int
	FromNet int
}

// KlineReader 本地 K 线读取能力（由 *repo.DB 实现）。
//
// 用接口而不是直接依赖 *repo.DB：① 服务层不必知道仓储层的具体类型；
// ② 传 nil 就能一键退回「全部走网络」的旧行为，出问题时降级成本为零。
type KlineReader interface {
	QueryKlines(q model.KlineQuery) ([]model.Kline, error)
}

// loadCandles 取一段用于算信号的 K 线：**优先读本地库，只有本地不够新/不够长才回退网络**。
//
// 为什么值得这么改（2026-10-01 实测）：
//
//	库里已经有 136 万行 K 线，而老实现每轮对每个合约都重新从 OKX 拉 400 根
//	（CandlesEnough = 1 次 /market/candles + 1 次 /market/history-candles）。
//	单合约实测约 130ms/次网络往返 → 176 个可交易合约一轮 ≈ 53 秒，
//	而同样 400 根本地索引查询只要 1~5ms。
//
// ★ 安全边界（这是本函数存在的全部意义）★
//
// 判定「本地够新」的口径：本地最新一根的 ts 必须 ≥ 当前**应该刚收盘**的那根的开盘时间。
//
//	周期 dur，时刻 now → 正在走的这根开盘于 floor(now/dur)*dur
//	                 → 最后一根已收盘的 K 线开盘于 floor(now/dur)*dur − dur
//
// 拿不到这一根，说明 DB 落后了（refreshLatest 还没跑到），**必须回网络**。
// 否则就会拿着上一根 K 线算信号 —— 在交易系统里这等于漏单或重复下单。
//
// 只要有任何一项对不上（库不可用 / 行数不足 / 数据落后），一律回退到原来的
// cli.CandlesEnough，行为与改造前完全一致。
//
// 另一个容易忽略的点：递归指标（ATR / RSI / EMA / TD9）的值**依赖窗口起始位置**，
// 所以本地取的根数必须和网络路径一致，否则同一根 K 线会算出不同信号。
// 网络路径是 `300 根 + 翻 1 页 100 根 = 400 根`（minCandles 一满足就停），
// 所以这里也取 minCandles 根 —— 两边窗口等长，结果才可比。
// localCandlesFresh 判断「本地库这批 K 线能不能直接拿来算信号」。
//
// 抽成纯函数是有意为之：这是决定要不要相信本地数据的**唯一判据**，
// 判错就会拿着上一根 K 线算信号（在交易系统里等于漏单/重复下单）。
// 纯函数才能被穷举单测 —— 见 scanner_local_test.go。
//
// 口径：本地最新一根的 ts 必须 ≥ 当前**应该刚收盘**的那根的开盘时间。
//
//	周期 dur，时刻 now → 正在走的那根开盘于 floor(now/dur)*dur
//	                 → 最后一根已收盘的 K 线开盘于 floor(now/dur)*dur − dur
//
// 等于或晚于它，说明「该有的那根已经有了」；早于它，说明 DB 落后了。
func localCandlesFresh(rows []model.Kline, bar string, minCandles int, nowMs int64) bool {
	if minCandles <= 0 || len(rows) < minCandles {
		return false
	}
	dur := BarDurationMs(bar)
	if dur <= 0 {
		return false
	}
	expectedLastClosed := (nowMs/dur)*dur - dur
	return rows[len(rows)-1].Ts >= expectedLastClosed
}

// klinesToCandles 仓储行 → 指标计算用的 K 线。
//
// Confirm 故意留 false：让 IndexOfLastClosed 走「按周期 + 时间推算」那条分支。
// 库里可能存着正在走的那根（refreshLatest 会把它写进来），时间推算正好能排除掉它。
func klinesToCandles(rows []model.Kline) []Candle {
	out := make([]Candle, len(rows))
	for i, r := range rows {
		out[i] = Candle{Ts: r.Ts, O: r.O, H: r.H, L: r.L, C: r.C, V: r.V}
	}
	return out
}

func loadCandles(db KlineReader, cli *OKXClient, instID, bar string, minCandles int, nowMs int64) ([]Candle, bool, error) {
	if db != nil {
		rows, err := db.QueryKlines(model.KlineQuery{InstID: instID, Bar: bar, Limit: minCandles})
		if err == nil && localCandlesFresh(rows, bar, minCandles, nowMs) {
			return klinesToCandles(rows), true, nil
		}
	}
	// 回退：老路子。任何一项对不上都走这里，行为与改造前完全一致。
	cands, err := cli.CandlesEnough(instID, bar, minCandles)
	return cands, false, err
}

// Scan 执行一轮扫描
func Scan(cfg *conf.Config, cli *OKXClient, bar string, kdb KlineReader) (*ScanResult, error) {
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
				// 优先本地库；本地落后/不足才回网络（详见 loadCandles 的注释）
				cands, fromDB, err := loadCandles(kdb, cli, j.ins.InstID, bar, cfg.MinCandles, nowMs)
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
				if fromDB {
					res.FromDB++
				} else {
					res.FromNet++
				}
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

	// 数据来源占比：只有 FromDB 占绝大多数，才说明「本地优先」真的在生效。
	// FromNet 偏多不是 bug —— 那是 DB 落后时的正常回退（比如刚重启、回补还没跑到）。
	if res.Scanned > 0 {
		logx.Logf("INFO", "扫描 K 线来源：本地库 %d，网络回退 %d（共 %d）",
			res.FromDB, res.FromNet, res.FromDB+res.FromNet)
	}

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
