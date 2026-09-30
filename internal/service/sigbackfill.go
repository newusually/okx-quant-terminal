package service

// sigbackfill.go —— 历史信号回算
//
// 问题：回补只把 K 线写进 kline 表，signals 表里没有历史信号，
// 所以「回补出来的一个月历史 K 线」图上没有 🚀 买入标记。
//
// 这里把可交易合约的已回补 K 线逐根跑一遍 ComputeSignal ——
// 和实时扫描用的同一套 8 因子代码，回测口径和实盘口径完全一致 ——
// score 够阈值的写进 signals 表（acted=0 只记信号）。
//
// 性能：ComputeSignal 每次对整段 K 线重算指标（O(n)），逐根调就是 O(n²)。
// 单合约 30 天 15m ≈ 2900 根，Go 里约 1~2 秒；两三百个合约放后台协程
// 慢慢跑，不挡网页、不挡交易（周期回调里都抢同一把 ratelimit，不碰 OKX）。
//
// 幂等：signals 有 UNIQUE(inst_id, bar, ts)，bulkUpsert 无 update 列时是
// INSERT IGNORE —— 重跑不会产生重复行。水位线（已算到的最大 ts）存内存 +
// 启动时从表里恢复，增量跑省时间。

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"finally-main/internal/perf"

	"finally-main/internal/conf"
	"finally-main/internal/model"
	"finally-main/internal/repo"
)

// 8 因子里最长的是 sma200，暖机 200 根之后 Ready 才可能为真。
const sigWarmup = 200

// sigWindow 每次喂给 ComputeSignal 的窗口长度。
//
// ComputeSignal 是「整段重算指标」的实现（O(n)），逐根调就是 O(n²)：
// 1m 周期 30 天有 4.3 万根，全段算一遍再逐根调 = 上亿次乘加，一台小机器
// 跑不完。但 8 因子最长的窗口只有 sma200 —— 截到最后 700 根，指标结果
// 与全段等价（还留了 500 根余量给 atr96 / macd / TD 链），快两个数量级。
const sigWindow = 700

var (
	sigBfMu   sync.Mutex
	sigBfSpan = map[string]repo.SignalScanSpan{} // "inst|bar" -> 已扫描的 K 线区间
	sigBfBusy atomic.Bool                        // 防止两轮回算叠跑
)

// LoadSignalScanSpans 启动时从表里恢复扫描水位线。
func LoadSignalScanSpans(db *repo.DB) {
	sp, err := db.LoadSignalScanSpans()
	if err != nil {
		return
	}
	sigBfMu.Lock()
	for k, v := range sp {
		if v.MaxTs > sigBfSpan[k].MaxTs {
			sigBfSpan[k] = v
		}
	}
	sigBfMu.Unlock()
}

// SaveSignalScanSpans 把当前水位线落盘。
func SaveSignalScanSpans(db *repo.DB) {
	sigBfMu.Lock()
	snap := make(map[string]repo.SignalScanSpan, len(sigBfSpan))
	for k, v := range sigBfSpan {
		snap[k] = v
	}
	sigBfMu.Unlock()
	_ = db.SaveSignalScanSpans(snap)
}

// BackfillSignalsFor 对单个 (合约, 周期) 回算历史信号，返回写入条数。
//
// 增量策略（关键）：
//
//	K 线回补是「从最近往老补」的，数据区间只会向左扩张：
//	  [T-1天, now] → [T-30天, now]
//	早期版本只记 MAX(ts) 当水位线，结果后补进来的老 K 线 ts 全都小于水位线，
//	被当成「算过了」跳过 —— 信号永远追不上 K 线（1m/3m/5m 卡在只有几个合约）。
//
//	现在记闭区间 [MinTs, MaxTs]：区间内的跳过，两头的增量（左边新补的老 K 线、
//	右边新生成的新 K 线）都算。落盘在 signal_scan_state 表，重启不丢。
func BackfillSignalsFor(cfg *conf.Config, db *repo.DB, instID, bar string) (int, error) {
	rows, err := db.QueryKlines(model.KlineQuery{InstID: instID, Bar: bar})
	if err != nil {
		return 0, err
	}
	if len(rows) <= sigWarmup {
		return 0, nil // 暖机都不够，每根都是 NaN，白算
	}
	// QueryKlines 已经返回升序（老→新）。这里不再反转 ——
	// 曾因为多反转一次导致整段变降序：水位线记反、指标窗口时间倒序，
	// 算出来的信号全是错的。为了不再被上游顺序变化坑到，显式再排一次。
	asc := make([]Candle, len(rows))
	for i, k := range rows {
		asc[i] = Candle{Ts: k.Ts, O: k.O, H: k.H, L: k.L, C: k.C, V: k.V}
	}
	sort.Slice(asc, func(i, j int) bool { return asc[i].Ts < asc[j].Ts })

	key := instID + "|" + bar
	sigBfMu.Lock()
	sp := sigBfSpan[key]
	sigBfMu.Unlock()

	oldest := asc[0].Ts
	newest := asc[len(asc)-1].Ts

	// 整段都扫过了 → 秒退（绝大多数轮次都是这条路）
	if sp.MaxTs > 0 && oldest >= sp.MinTs && newest <= sp.MaxTs {
		return 0, nil
	}

	th := cfg.ThresholdFor(instID)
	nowMs := time.Now().UnixMilli()
	store := repo.NewStore(cfg)

	batch := make([]repo.EngineSignalRow, 0, 256)
	total := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := store.Ingest(repo.StorePayload{Signal: batch}); err != nil {
			return err
		}
		total += len(batch)
		batch = batch[:0]
		return nil
	}

	for i := sigWarmup; i < len(asc); i++ {
		ts := asc[i].Ts
		// 已扫区间内跳过（区间外 = 新回补的老 K 线 或 新生成的新 K 线）
		if sp.MaxTs > 0 && ts >= sp.MinTs && ts <= sp.MaxTs {
			continue
		}
		// 窗口截断：只喂最近 sigWindow 根（见 sigWindow 注释）。
		// i < sigWindow 时窗口就是 [0, i]，与全段等价。
		lo := i + 1 - sigWindow
		if lo < 0 {
			lo = 0
		}
		win := asc[lo : i+1]
		sig := ComputeSignal(instID, bar, win, len(win)-1)
		if sig == nil || !sig.Ready || sig.Score < th {
			continue
		}
		batch = append(batch, repo.EngineSignalRow{
			InstID: instID, Bar: bar, Ts: sig.Ts, Close: sig.Close,
			Mask: sig.Mask, Score: sig.Score, HitList: sig.HitList,
			Pot: nan0(sig.Pot), Fri: nan0(sig.Fri), Kin: nan0(sig.Kin),
			Rsi: nan0(sig.Rsi), Td: sig.Td,
			Acted: 0, CreatedAt: nowMs,
		})
		if len(batch) >= 300 {
			if err := flush(); err != nil {
				return total, err
			}
		}
	}
	if err := flush(); err != nil {
		return total, err
	}

	// 更新水位线：区间并上本次 K 线的范围
	sigBfMu.Lock()
	cur := sigBfSpan[key]
	if cur.MaxTs == 0 {
		cur = repo.SignalScanSpan{MinTs: oldest, MaxTs: newest}
	} else {
		if oldest < cur.MinTs {
			cur.MinTs = oldest
		}
		if newest > cur.MaxTs {
			cur.MaxTs = newest
		}
	}
	cur.Scanned = int64(len(asc))
	sigBfSpan[key] = cur
	sigBfMu.Unlock()
	return total, nil
}

// nan0 NaN 归零：MySQL 的 DOUBLE 不收 NaN，写进去整条 INSERT 会炸。
func nan0(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// RunSignalBackfillOnce 对全部可交易合约回算一轮，返回 (合约数, 信号数)。
// 只算 cfg.Bar（主周期）；其它周期等回补齐了再说，一次别贪多。
func RunSignalBackfillOnce(cfg *conf.Config, db *repo.DB, bar string,
	logf func(string, ...any)) (int, int) {

	if !sigBfBusy.CompareAndSwap(false, true) {
		return 0, 0
	}
	defer sigBfBusy.Store(false)

	insts, err := db.TradeableInstIDs()
	if err != nil {
		logf("历史信号回算：读可交易合约失败：%v", err)
		return 0, 0
	}
	if len(insts) == 0 {
		return 0, 0
	}

	var totalSig int64
	var doneCnt int64
	jobs := make(chan string)
	var wg sync.WaitGroup
	workers := 3 // 2 逻辑核，回算以 CPU 为主但夹杂 MySQL 写，压到 3 路能快一些
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for inst := range jobs {
				n, err := BackfillSignalsFor(cfg, db, inst, bar)
				if err != nil {
					logf("历史信号回算 %s %s 失败：%v", inst, bar, err)
					continue
				}
				atomic.AddInt64(&totalSig, int64(n))
				atomic.AddInt64(&doneCnt, 1)
			}
		}()
	}
	for _, inst := range insts {
		jobs <- inst
	}
	close(jobs)
	wg.Wait()

	// 水位线落盘（重启后接着跑，不重算）
	SaveSignalScanSpans(db)

	n, c := int(doneCnt), int(atomic.LoadInt64(&totalSig))
	if n > 0 {
		logf("历史信号回算完成：%s 周期 %d 个合约，新增信号 %d 条", bar, n, c)
	}
	return n, c
}

// StartSignalBackfillLoop 周期性回算：
//
//	启动后先等 backfillWarmup（让第一波 K 线回补落库），跑第一轮全量；
//	之后每 sigBfEvery 增量跑一轮（新回补进来的 K 线也有信号）。
//	每个周期按 cfg.SignalBars 依次跑（默认 1m/3m/5m/15m/1H/4H 全部）——
//	交易只认 bars_enabled，但图上的 🚀 用户想看哪个周期就看哪个周期。
//	ctx 结束自然退出。
func StartSignalBackfillLoop(ctx context.Context, db *repo.DB,
	logf func(string, ...any)) {

	go func() {
		defer func() {
			if r := recover(); r != nil {
				logf("历史信号回算协程退出：%v", r)
			}
		}()
		LoadSignalScanSpans(db)

		// 先等数据：回补第一波要一两分钟
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Second):
		}

		for {
			cfg := conf.LoadConfig()
			if cfg != nil && cfg.Enabled {
				bars := normalizeSignalBars(cfg)
				for _, bar := range bars {
					select {
					case <-ctx.Done():
						return
					default:
					}
					// 打点：这一轮「信号回算」是全项目的 CPU 大户（478 合约 × N 周期），
					// 没有打点就只能靠猜。perf 日志里看 signal.recalc 的耗时占比即可。
					perf.Count1("signal.recalc.round")
					done := perf.Track("signal.recalc")
					RunSignalBackfillOnce(cfg, db, bar, logf)
					done()
				}
			}
			// 一轮跑完马上开下一轮（增量轮基本秒回），
			// 但别空转：留 30 秒让 K 线回补插新数据。
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
		}
	}()
}

// normalizeSignalBars 取信号回算周期列表，并按周期从大到小排。
//
// 大周期（4H/1H）K 线根数少、算得快，先跑完 —— 用户切到这些周期马上
// 就能看到补齐的 🚀；1m 根数最多放最后，不会把整轮时间全占了。
func normalizeSignalBars(cfg *conf.Config) []string {
	bars := cfg.SignalBars
	if len(bars) == 0 {
		bars = []string{cfg.Bar}
	}
	out := make([]string, 0, len(bars))
	for _, b := range bars {
		b = strings.TrimSpace(b)
		if b == "" {
			continue
		}
		out = append(out, b)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return barRank(out[i]) > barRank(out[j])
	})
	return out
}

// barRank 周期的「大→小」排序权重（越大越长的周期放前面）。
func barRank(bar string) int {
	switch strings.ToLower(strings.TrimSpace(bar)) {
	case "1d":
		return 900
	case "12h":
		return 800
	case "6h":
		return 700
	case "4h":
		return 600
	case "2h":
		return 500
	case "1h":
		return 400
	case "30m":
		return 300
	case "15m":
		return 200
	case "5m":
		return 100
	case "3m":
		return 50
	case "1m":
		return 1
	}
	return 0
}

// signalBarEnabled 信号回算周期白名单判定（大小写不敏感）
func signalBarEnabled(bars []string, bar string) bool {
	for _, b := range bars {
		if strings.EqualFold(strings.TrimSpace(b), bar) {
			return true
		}
	}
	return false
}
