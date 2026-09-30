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
	"sync"
	"sync/atomic"
	"time"

	"finally-main/internal/conf"
	"finally-main/internal/model"
	"finally-main/internal/repo"
)

// 8 因子里最长的是 sma200，暖机 200 根之后 Ready 才可能为真。
const sigWarmup = 200

var (
	sigBfMu     sync.Mutex
	sigBfWater  = map[string]int64{} // "inst|bar" -> 已回算到的最大 ts
	sigBfBusy   atomic.Bool          // 防止两轮回算叠跑
)

// LoadSignalWatermarks 启动时从表里恢复水位线。
func LoadSignalWatermarks(db *repo.DB) {
	wm, err := db.SignalWatermarks()
	if err != nil {
		return
	}
	sigBfMu.Lock()
	for k, v := range wm {
		if v > sigBfWater[k] {
			sigBfWater[k] = v
		}
	}
	sigBfMu.Unlock()
}

// BackfillSignalsFor 对单个 (合约, 周期) 回算历史信号，返回写入条数。
func BackfillSignalsFor(cfg *conf.Config, db *repo.DB, instID, bar string) (int, error) {
	rows, err := db.QueryKlines(model.KlineQuery{InstID: instID, Bar: bar})
	if err != nil {
		return 0, err
	}
	if len(rows) <= sigWarmup {
		return 0, nil // 暖机都不够，每根都是 NaN，白算
	}
	// QueryKlines 按 ts DESC 返回，转成 ComputeSignal 要的升序（老→新）
	asc := make([]Candle, len(rows))
	for i, k := range rows {
		asc[len(rows)-1-i] = Candle{Ts: k.Ts, O: k.O, H: k.H, L: k.L, C: k.C, V: k.V}
	}

	sigBfMu.Lock()
	last := sigBfWater[instID+"|"+bar]
	sigBfMu.Unlock()

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
		if asc[i].Ts <= last {
			continue // 水位线之前算过了
		}
		sig := ComputeSignal(instID, bar, asc, i)
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

	sigBfMu.Lock()
	if asc[len(asc)-1].Ts > sigBfWater[instID+"|"+bar] {
		sigBfWater[instID+"|"+bar] = asc[len(asc)-1].Ts
	}
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
	workers := 2 // 2 逻辑核，别抢交易的 CPU
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
//	ctx 结束自然退出。
func StartSignalBackfillLoop(ctx context.Context, db *repo.DB, bar string,
	logf func(string, ...any)) {

	go func() {
		defer func() {
			if r := recover(); r != nil {
				logf("历史信号回算协程退出：%v", r)
			}
		}()
		LoadSignalWatermarks(db)

		// 先等数据：回补第一波要一两分钟
		select {
		case <-ctx.Done():
			return
		case <-time.After(90 * time.Second):
		}

		for {
			cfg := conf.LoadConfig()
			if cfg != nil && cfg.Enabled && cfg.BarEnabled(bar) {
				RunSignalBackfillOnce(cfg, db, bar, logf)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Minute):
			}
		}
	}()
}
