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

// sigWindow 旧「逐根滑窗」实现每次喂给 ComputeSignal 的窗口长度。
//
// ⚠️ 2026-10-01 起生产路径已改成 ComputeSeries（整段算一次），本常量只被
// backfillSignalWindowed（测试对照）使用。保留它是为了守住口径：
// 改造后必须证明新旧两条路在离散字段上零差异。
//
// 历史背景（为什么当初要截 700）：
// ComputeSignal 是「整段重算指标」的实现，逐根调就是 O(n²)。1m 周期 30 天有
// 4.3 万根，全段算一遍再逐根调 = 上亿次乘加，一台小机器跑不完，所以当时截到最后
// 700 根，把每次调用压成 O(700)。**这个截断是欠收敛的**（period=96 的 Wilder
// 平滑残差约 7e-4），整段法反而更准 —— 详见 BackfillSignalsFor 里的说明。
const sigWindow = 700

// sigReadBars 回算一轮**最多读多少根** K 线（不再把每个合约的全部历史读出来）。
//
// 为什么需要它：把计算改成「整段算一次」之后，`signal.recalc` 一轮仍是 40~51 秒，
// 而纯计算只要 0.3 秒 —— 时间全花在读库。175 个合约 × 3.5 万行 ≈ 每轮 600 万行，
// 而真正要算的往往只有新到的那一两根。
//
// 取值依据（暖机够不够）：
// · 最慢的递归指标是 period=96 的 Wilder ATR，残差 (95/96)^N
// · N=2000 → 9e-10，N=3000 → 3e-14
// 取 2000 已经比旧的 700 根窗口（残差 6.6e-04）收敛 6 个数量级，
// 同时把单合约读取量从 3.5 万行压到 2000 行（约 17 倍）。
//
// ⚠️ 它只影响「从哪儿开始读」，不影响窗口语义：整段法本来就按读到的第一根做种子，
// 而 sigReadBars 比旧窗口长得多，所以换过来是**更准**，不是更糊。
const sigReadBars = 2000

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

// ForgetSignalScanSpans 把某个合约的「已扫区间」作废（内存 + 落盘），
// 下次回算会把它当从没扫过、从头再算一遍。
//
// ★ 为什么需要这个函数（2026-10-02 十三期）★
//
// 水位线的语义是「区间内已经算过了，跳过」。这个前提只在一个条件下成立：
// **判定口径不变**。一旦改了门槛（比如给 NQ 换成「共振≥4 且收阴」），
// 已扫区间里那些"当时不合格"的 K 线不会重算 —— 现象就是「改了配置毫无反应」，
// 而且不报任何错。这是本项目反复踩的同一类坑（见 SKILL: config-change-effect-audit）。
//
// 所以口径变更时**必须**同时作废水位线 + 删掉旧口径写下的信号行。
func ForgetSignalScanSpans(db *repo.DB, instID string) error {
	prefix := instID + "|"
	sigBfMu.Lock()
	for k := range sigBfSpan {
		if strings.HasPrefix(k, prefix) {
			delete(sigBfSpan, k)
		}
	}
	sigBfMu.Unlock()
	return db.DeleteSignalScanSpansForInst(instID)
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
//
// ★ 2026-10-02 十三期：加了第二个入口 BackfillSignalsForReadonly ★
//
//	只读板块（NQ）不能用全市场口径（指数波动尺度小，-0.7% 永不触发），
//	所以判定那一步要能换。两个入口共用下面的 backfillSignals 主体 ——
//	读库、水位线、批量写、诊断字段这些**只有一份实现**，不会走岔。
func BackfillSignalsFor(cfg *conf.Config, db *repo.DB, instID, bar string) (int, error) {
	return backfillSignals(cfg, db, instID, bar, ReadonlySignalRule{})
}

// BackfillSignalsForReadonly 用只读板块专属口径回算（rule 未启用时自动退回通用口径）。
func BackfillSignalsForReadonly(cfg *conf.Config, db *repo.DB, instID, bar string, rule ReadonlySignalRule) (int, error) {
	return backfillSignals(cfg, db, instID, bar, rule)
}

func backfillSignals(cfg *conf.Config, db *repo.DB, instID, bar string, rule ReadonlySignalRule) (int, error) {
	key := instID + "|" + bar
	sigBfMu.Lock()
	sp := sigBfSpan[key]
	sigBfMu.Unlock()

	dur := BarDurationMs(bar)
	if dur <= 0 {
		dur = 900_000
	}

	// ★ 先问「本轮到底有没有事可做」（2026-10-01）★
	//
	// 改造之前这里靠下面那句 `oldest >= sp.MinTs && newest <= sp.MaxTs` 秒退，
	// 之所以能触发，是因为那时每次都把**整个合约的全部历史**读出来，
	// oldest/newest 就是库里的首尾。改成只读一段之后，读回来的最早一根
	// 总是 < sp.MinTs，那句秒退就**再也触发不了** —— 变成每轮给每个合约
	// 白读 2000 根、白算一遍整段指标。175 个合约就是每轮 35 万行。
	//
	// 这里用两次**轻量探针**把秒退找回来（都走 (inst_id,bar,ts) 复合主键，毫秒级）：
	//   ① 右边：库里最新一根是否 <= 水位线上界？（MAX(ts)）
	//   ② 左边：水位线下界左边是否还有没算过的老 K 线？（LIMIT 1）
	// 两边都没有新数据 → 区间 [MinTs, MaxTs] 外一根都没有 → 直接返回。
	//
	// ⚠️ 任一探针出错（DB 抖动）就**不返回**，继续往下走慢路 ——
	// 宁可多算一遍，也不能因为一次查询失败就把新 K 线漏掉。
	if sp.MaxTs > 0 && sp.MinTs > 0 {
		newest, nerr := db.LastKlineTs(instID, bar)
		if nerr == nil && newest > 0 && newest <= sp.MaxTs {
			older, oerr := db.QueryKlines(model.KlineQuery{
				InstID: instID, Bar: bar, ToTs: sp.MinTs - 1, Limit: 1,
			})
			if oerr == nil && len(older) == 0 {
				// 无事可做，但顺手把诊断字段 scanned 修正到真实根数 ——
				// 上一个版本在快路上把它写成了「本次窗口长度」（2002），
				// 需要一次机会自愈（含义见下面写 cur.Scanned 处的注释）。
				sigBfMu.Lock()
				if c, ok := sigBfSpan[key]; ok {
					c.Scanned = (c.MaxTs-c.MinTs)/dur + 1
					sigBfSpan[key] = c
				}
				sigBfMu.Unlock()
				return 0, nil
			}
		}
	}

	// ★ 只读「真的要算的那一段」（2026-10-01）★
	//
	// 原来是 QueryKlines 不带任何限制 → 每个合约把**全部历史**读出来再解析
	// （BTC 有 3.5 万根），175 个合约一轮就是 600 万行。实测 signal.recalc
	// 一轮 40~51 秒，而把计算改成整段算一次以后，**计算只占 0.3 秒** ——
	// 也就是说剩下的时间全花在这次读库 + 解析上。
	//
	// 实际上要算的只有两块：
	//   ① 水位线右边的新 K 线              (sp.MaxTs, ∞)
	//   ② 水位线左边的老 K 线（新回补的）   (-∞, sp.MinTs)
	// 两块各自往前多取 sigReadBars 根做暖机即可；中间的 [MinTs, MaxTs] 已经算过，
	// 一根都不用读。
	//
	// 但 ① 和 ② 是**两段不连续的时间**，拼在一起会让指标跨时间断层计算 ——
	// 所以只在「①单独够用」时走快路：用一次 LIMIT 1 的探针确认左边确实没有
	// 没算过的老数据。探针走 (inst_id,bar,ts) 复合主键的反向索引，毫秒级。
	// 只有真的存在左边缺口（历史回补正在往老处加数据）才退回全读。
	readSpanMs := int64(sigReadBars) * dur

	var rows []model.Kline
	var err error

	if sp.MaxTs == 0 {
		// 从没扫过这个 (合约,周期)：只能全读一次，否则历史全丢
		rows, err = db.QueryKlines(model.KlineQuery{InstID: instID, Bar: bar})
	} else {
		from := sp.MaxTs - readSpanMs
		if from < 0 {
			from = 0
		}
		rows, err = db.QueryKlines(model.KlineQuery{InstID: instID, Bar: bar, FromTs: from})
		if err == nil && sp.MinTs > 0 {
			// 探针：水位线左边还有没有没算过的老 K 线？
			older, oerr := db.QueryKlines(model.KlineQuery{
				InstID: instID, Bar: bar, ToTs: sp.MinTs - 1, Limit: 1,
			})
			if oerr == nil && len(older) > 0 {
				// 有 → 左边那段也得算。两段不连续，只能老老实实全读。
				rows, err = db.QueryKlines(model.KlineQuery{InstID: instID, Bar: bar})
			}
		}
	}
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

	oldest := asc[0].Ts
	newest := asc[len(asc)-1].Ts

	// 兜底秒退：正常情况下上面两次探针已经拦掉了「无事可做」的轮次，
	// 这里只是最后一道保险（探针出错时会走到这儿）。
	// 注意它**在快路上基本不会触发** —— 快路读回来的最早一根是
	// sp.MaxTs - sigReadBars 根，通常 < sp.MinTs，所以 oldest >= sp.MinTs 不成立。
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

	// ★ 性能改造（2026-10-01）：逐根滑窗 → 整段算一次 ★
	//
	// 旧写法：
	//     for i := ...; i++ {
	//         win := asc[lo:i+1]                       // 700 根
	//         sig := ComputeSignal(instID, bar, win, len(win)-1)
	//     }
	// ComputeSignal 每次都要把整个窗口的指标重算一遍 → 整体 O(根数 × 700)。
	// 实测 175 个合约一轮 46 秒，而且只吃满一个核也提不上去（纯串行 CPU）。
	//
	// 新写法：ComputeSeries 把整段指标算一遍（O(根数)），之后 series.At(i) 是 O(1)。
	// 同一份 2900 根数据，内层工作量降了约 700 倍。
	//
	// ★ 口径变化，必须说清楚 ★
	// 递归指标（ATR / RSI / EMA 的 Wilder 平滑）**依赖平滑的起始种子位置**：
	//     · 逐根滑窗：第 i 根的种子落在下标 i-699
	//     · 整段一次：种子落在下标 0
	// 两者不逐位相同。实测（testdata 里 3 个主力合约 × 3000 根**真实** K 线，
	// 逐根比对 8400 个下标）：
	//     · 浮点最大相对偏差 6.6e-04，只出现在 Fri（= atrp14 / atrp96）
	//       —— 与理论吻合：Wilder 周期 96 的残差是 (95/96)^700 ≈ 7e-04
	//     · **mask / score / Td / Ready 不一致的根数 = 0**
	//       即：没有一个买卖判定因此改变
	// 而且整段法的值**更准**：700 根窗口对 period=96 的 Wilder 平滑是欠收敛的，
	// 整段法是单一种子、没有截断痕迹。
	//
	// 回归门（两条都必须保持零差异，否则测试直接红）：
	//     TestWindowedVsFullSeriesOnRealData     真实 K 线
	//     TestWindowedVsFullSeriesEquivalence    合成 K 线
	//
	// 另一个必须知道的事实：signals 表是 UNIQUE(inst_id,bar,ts) + INSERT IGNORE，
	// 所以**库里已有的历史行不会被这次改造改写**，重算只影响新写入的行。
	// 想让全表统一到新口径，需要先清表再让水位线从头跑一遍。
	series := ComputeSeries(instID, bar, asc)
	defer series.Release()

	for i := sigWarmup; i < len(asc); i++ {
		ts := asc[i].Ts
		// 已扫区间内跳过（区间外 = 新回补的老 K 线 或 新生成的新 K 线）
		if sp.MaxTs > 0 && ts >= sp.MinTs && ts <= sp.MaxTs {
			continue
		}
		sig := series.At(i)
		// ★ 入库判据必须与买入/加仓**同一个函数**（2026-10-02 三期）★
		//
		// 改之前这里只判 `sig == nil || !sig.Ready || sig.Score < th`，
		// 三期给买入加了「涨跌幅过 min_bar_rise_pct 门槛」（六期：负值 = 必须真跌 < -0.7%）之后，
		// 若这里不跟着改，图上标的 🚀 会包含「分数够但涨跌幅不过门槛、实盘根本不会下单」
		// 的根 —— 图和实盘口径不一致，而且没有任何报错。
		// （SignalQualified 内部已含 nil / Ready 判断。）
		//
		// ★ 十三期：只读板块（NQ）换成它自己那套口径。两套判据都收敛在
		//   SignalQualified / ReadonlySignalRule.Qualify 里，这里不自己拼条件。
		if rule.Enabled() {
			if !rule.Qualify(sig) {
				continue
			}
		} else if !SignalQualified(sig, th, cfg.MinBarRisePct(), cfg.MaxBarDropPct()) {
			continue
		}
		batch = append(batch, repo.EngineSignalRow{
			InstID: instID, Bar: bar, Ts: sig.Ts, Close: sig.Close,
			Mask: sig.Mask, Score: sig.Score, HitList: sig.HitList,
			RisePct: nan0(sig.RisePct),
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
	// Scanned 是纯诊断字段（全项目没有任何代码读它做决策，只落库留痕），
	// 含义是「这个 (合约,周期) 已经覆盖到的 K 线根数」。
	//
	// ⚠️ 改成快路之后不能再写 len(asc)：那只是**本次读回来的窗口长度**
	// （约 2002），会把一年的 35058 根写成 2002，看起来像「只扫了 2002 根」。
	// 这里用已扫区间 [MinTs, MaxTs] 除以周期长度反推：
	// 加密货币永续合约的 K 线是连续的（没有休市），所以反推值和 COUNT(*) 吻合
	// —— 实测 BTC 15m：2025-10-01 13:15 ~ 2026-10-01 17:45 → 35059，
	// 库里 COUNT(*) 也是 35059。
	// 区间只会向外扩张（MinTs 只减、MaxTs 只增），所以这个值单调不减。
	if dur > 0 {
		cur.Scanned = (cur.MaxTs-cur.MinTs)/dur + 1
	} else {
		cur.Scanned = int64(len(asc))
	}
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

// backfillSignalWindowed 旧的「逐根滑窗」实现，**只作等价性测试的对照**，
// 已不在生产路径上（生产走 ComputeSeries）。
//
// ⚠️ 不要在生产代码里调用它：它是 O(窗口) 一次的，循环调用就是 O(根数 × 窗口)，
// 正是这轮要消灭的那个复杂度。
//
// 之所以还留着：口径这种东西不能靠「我看着差不多」来保证。
// TestWindowedVsFullSeriesOnRealData / TestWindowedVsFullSeriesEquivalence
// 用真实和合成两组数据，把新旧两条路的离散字段（mask / score / Td / Ready）
// 逐根比对 —— 那两条测试调用的就是这个函数，而不是测试里另抄一份，
// 这样只要生产实现被改动，测试立刻会红。
func backfillSignalWindowed(instID, bar string, asc []Candle, i int) *Signal {
	lo := i + 1 - sigWindow
	if lo < 0 {
		lo = 0
	}
	win := asc[lo : i+1]
	return ComputeSignal(instID, bar, win, len(win)-1)
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
