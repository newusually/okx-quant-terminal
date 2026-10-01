package service

// trader.go —— 策略引擎：扫描 → 闸门 → 下单 → 出场（对应文案 §7）
//
// 一轮 Run() 做的事：
//  1. 连接 OKX（自动挑域名 + 对时）
//  2. 出场检查（每轮都做，跟周期无关）
//  3. 若当前周期在 bars_enabled 里 → 扫描全市场 + 8 因子共振 + 开仓闸门
//  4. 落库（信号 / 成交 / 权益 / 日志）
//
// dry_run=true 时只算信号、只记库，绝不发任何真单。

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"finally-main/internal/conf"
	"finally-main/internal/logx"
	"finally-main/internal/perf"
	"finally-main/internal/repo"
)

type engineState struct {
	mu           sync.Mutex
	cli          *OKXClient
	cliKey       string
	storeInit    bool
	cycles       int64
	apiErrStreak int
	pausedUntil  time.Time
	pauseReason  string

	// ghostSeen 记录「本地在持、但 OKX 实时持仓列表里看不到」的连续轮次。
	//
	// 只由 reconcilePositions 读写，且它的两个调用方（engineRunBars / exitPass）
	// 都在 eng.mu 里，所以不用再单独加锁。
	ghostSeen map[string]int
}

var eng = &engineState{}

// klineReaderOf 从 store 取「本地库读能力」。拿不到就返回 nil ——
// 所有调用方都把 nil 当作「退回纯网络」，所以 DB 挂掉只是变慢，不会不出场。
//
// ★ 注意别踩 Go 的 typed-nil 陷阱：接口里塞一个 nil 指针，接口本身不等于 nil，
// 后面的 `db != nil` 会判成 true 然后解引用炸掉。所以这里显式挡一层。
func klineReaderOf(store *repo.Store) KlineReader {
	if store == nil {
		return nil
	}
	db, err := store.DB()
	if err != nil || db == nil {
		return nil
	}
	return db
}

func (e *engineState) client(cfg *conf.Config) (*OKXClient, error) {
	key := cfg.OKX.BaseURL + "|" + cfg.OKX.Proxy + "|" +
		strconv.FormatBool(cfg.OKX.Simulated) + "|" + cfg.OKX.APIKey
	if e.cli != nil && e.cliKey == key {
		return e.cli, nil
	}
	c, err := newOKXClient(cfg)
	if err != nil {
		return nil, err
	}
	e.cli = c
	e.cliKey = key
	return c, nil
}

// paused 必须在持有 eng.mu 的情况下调用
func (e *engineState) paused() (bool, string) {
	if time.Now().Before(e.pausedUntil) {
		return true, e.pauseReason
	}
	return false, ""
}

func (e *engineState) pause(d time.Duration, reason string) {
	e.pausedUntil = time.Now().Add(d)
	e.pauseReason = reason
	logx.Logf("WARN", "策略暂停 %s：%s", d, reason)
}

// ---------------------------------------------------------------------------
// 主入口
// ---------------------------------------------------------------------------

// EngineRun 跑一轮**单个周期**。minute 取值与 OKX 一致：1m 3m 5m 15m 30m 1H 2H 4H 6H 12H 1D
//
// 单个周期是给 CLI / 手工触发用的；实时引擎走 EngineRunEnabled（见下），
// 一轮把所有启用周期都扫掉，只做一次出场巡检与加仓判定。
func EngineRun(minute string) error {
	return engineRunBars([]string{normalizeBar(minute)})
}

// EngineRunEnabled 跑一轮「所有启用周期」的扫描：出场 + 加仓只做一次，扫描逐个周期做。
//
// ★ 2026-10-01 二期 ★
// 用户口径：「四个周期（1m/3m/5m/15m）都参与开仓」，而不是只让 15m 交易。
//
// 为什么不直接循环调 EngineRun(bar)：
//   每次 EngineRun 都会重新拉一次账户余额 + 持仓（约 300~400ms 的 OKX RTT）、
//   重新读一遍在持仓与当日统计、重跑一遍出场巡检与加仓判定。
//   四个周期各调一次 = 这些工作白做 4 遍，等于每轮多打 6~8 次 OKX 接口
//   —— 在「2 核 / 限频 20 次每秒」的机器上，这是纯粹的浪费与排队。
//   所以改成：**公共部分（账户/持仓/出场/加仓）做一次，只有扫描按周期循环。**
//   效果与「四次 EngineRun」在交易语义上等价，但外部请求数不变。
//
// 周期顺序取 cfg.BarsEnabled 的书写顺序（配置文件里是 1m,3m,5m,15m）。
func EngineRunEnabled() error {
	cfg := conf.LoadConfig()
	if cfg == nil || !cfg.Enabled {
		return nil
	}
	bars := make([]string, 0, len(cfg.BarsEnabled))
	for _, b := range cfg.BarsEnabled {
		if s := normalizeBar(b); s != "" {
			bars = append(bars, s)
		}
	}
	if len(bars) == 0 {
		bars = []string{normalizeBar(cfg.Bar)}
	}
	return engineRunBars(bars)
}

// engineRunBars 一轮引擎的完整实现，bars 里每个周期各跑一次「扫描 + 开仓」。
func engineRunBars(bars []string) error {
	cfg := conf.LoadConfig()
	if cfg == nil || !cfg.Enabled {
		return nil
	}
	if len(bars) == 0 {
		return nil
	}
	// bar 只作「默认周期」用：超时平仓的根数口径、日志前缀都读它。
	bar := bars[0]

	eng.mu.Lock()
	defer eng.mu.Unlock()

	start := time.Now()
	eng.cycles++

	// ★ 阶段打点（2026-10-01）：一轮 EngineRun 实测在 18~62 秒之间剧烈波动，
	// 光看总时长没法判断该改哪一段。下面把每个阶段单独计时，
	// perf 日志里就会出现 eng.account / eng.exits / eng.scan 等条目，
	// 直接看谁是大头。打点本身开销是纳秒级，不影响交易。
	ph := func(name string) func() { return perf.Track(name) }

	pSetup := ph("eng.setup")
	cli, err := eng.client(cfg)
	if err != nil {
		pSetup()
		return err
	}
	if err := cli.EnsureReady(); err != nil {
		eng.apiErrStreak++
		eng.maybePauseOnErrors(cfg)
		pSetup()
		return err
	}
	pSetup()

	pStore := ph("eng.store")
	store := repo.NewStore(cfg)
	if !eng.storeInit {
		if err := store.Init(); err != nil {
			logx.Logf("WARN", "MySQL 初始化失败（不影响交易）：%v", err)
		} else {
			eng.storeInit = true
			logx.Logf("INFO", "MySQL 就绪：mysql://%s@%s:%d/%s",
				cfg.Store.User, cfg.Store.Host, cfg.Store.Port, cfg.Store.Database)
		}
	}

	// 在持仓 + 当日统计
	openPos, err := store.OpenPositions()
	if err != nil {
		logx.Logf("WARN", "读在持仓失败：%v", err)
		openPos = nil
	}
	ctr, err := store.Counters(todayStartMs())
	if err != nil {
		logx.Logf("WARN", "读当日统计失败：%v", err)
		ctr = &repo.Counters{LastEntryTs: map[string]int64{}}
	}
	pStore()

	// 账户信息（配了 Key 才有）
	pAcct := ph("eng.account")
	var account *Account
	markPrices := map[string]float64{}
	// 对账依据：两个接口都成功才算「OKX 侧信息可信」
	//   · 只信余额成功而持仓失败 → 会把「持仓接口挂了」误读成「OKX 上没仓」
	//   · 反过来同理
	balOK, posOK := false, false
	liveIDs := map[string]bool{}
	if hasKeys(cfg) {
		if acc, aerr := cli.Balance(); aerr == nil {
			account = acc
			balOK = true
		} else {
			eng.apiErrStreak++
			logx.Logf("WARN", "取账户余额失败：%v", aerr)
		}
		if ps, perr := cli.Positions(); perr == nil {
			posOK = true
			liveIDs = livePositionIDs(ps)
			account = accountOrNew(account)
			account.PosCount = len(ps)
			account.PositionList = ps
			for _, p := range ps {
				if mp := toF(p.MarkPx); mp > 0 {
					markPrices[p.InstID] = mp
				}
			}
		} else {
			logx.Logf("WARN", "取持仓失败：%v", perr)
		}
	}
	pAcct()

	// 对账：剔除「本地在持、OKX 上已经没了」的幽灵仓。
	// 必须在出场巡检之前 —— 否则出场会继续对着不存在的仓位发平仓单，
	// 一路 All operations failed 白吃限频令牌（见 reconcilePositions 注释）。
	openPos = reconcilePositions(cfg, store, balOK && posOK, liveIDs, markPrices, openPos)

	// 本地库读能力：出场巡检、加仓判定、入场扫描三处共用一份。
	// 拿不到就传 nil —— 三处都会退回原来的纯网络行为，DB 挂了只是慢，不会不动。
	kdb := klineReaderOf(store)

	// ① 出场（每轮都查）
	pExits := ph("eng.exits")
	closed, closedIDs := runExits(cfg, cli, store, kdb, openPos, markPrices, bar)
	openPos = dropClosed(openPos, closedIDs)
	pExits()

	// ①.5 加仓（摊薄均价）：条件与买入**完全一致** ——
	//      该仓位自己周期上最后一根已收盘 K 线满足 SignalQualified（Score ≥ 阈值 且涨幅 > 0.5%）
	//      → 补原持仓保证金的 1/3（ratio）。次数不限（max_times = 0）。
	//      必须在出场之后（刚平的仓不加）、入场之前（总保证金按新值算）。
	//
	//      ★ 旧的「15m 先跌 0.5% 后转涨」口径已于 2026-10-01 二期下线；
	//        「加满 N 次就强制平仓」那条规则也已删除 —— 出场只剩
	//        +0.3% 止盈 / 1 小时超时（布林上轨与止损关闭），没有任何一条看加仓次数。
	//        所以 runAddons 的第二个返回值（本轮平掉的仓位）**恒为空**，
	//        这里保留合并动作只是为了不动调用结构。
	pAddons := ph("eng.addons")
	added, addonClosed := runAddons(cfg, cli, store, kdb, openPos, markPrices)
	closed += len(addonClosed)
	openPos = dropClosed(openPos, addonClosed)
	closedIDs = mergeIDs(closedIDs, addonClosed)
	pAddons()

	// ② 入场：**逐个启用周期**扫一遍
	//
	//    账户 / 持仓 / 出场巡检 / 加仓判定都在上面做完了，这里只剩「扫描 + 下单」
	//    按周期重复 —— 这是「四个周期都参与开仓」的落地点。
	//
	//    ★ 新开的仓必须立刻并进 openPos 再喂给下一个周期 ★
	//    否则同一个合约会在同一轮里被 4 个周期各开一次（1m 开了、3m 又开、
	//    5m 再开、15m 再来一遍）—— 那不是「四个周期共振」，是重复下单。
	//    同时 ctr.OrdersToday 也要跟着加，后面的周期看到的当日笔数才是真的。
	openedTotal := 0
	lines := make([]string, 0, len(bars))
	for _, b := range bars {
		if !cfg.BarEnabled(b) {
			lines = append(lines, fmt.Sprintf("周期 %s 未启用扫描（bars_enabled 未包含）", b))
			continue
		}

		pScan := ph("eng.scan")
		res, serr := Scan(cfg, cli, b, kdb)
		if serr != nil {
			pScan()
			eng.apiErrStreak++
			eng.maybePauseOnErrors(cfg)
			return serr
		}
		eng.apiErrStreak = 0

		// 先只记信号（acted=0），下单情况再回来补
		if len(res.Signals) > 0 {
			rows := make([]repo.EngineSignalRow, 0, len(res.Signals))
			nowMs := time.Now().UnixMilli()
			for _, s := range res.Signals {
				rows = append(rows, repo.EngineSignalRow{
					InstID: s.InstID, Bar: s.Bar, Ts: s.Ts, Close: s.Close,
					Mask: s.Mask, Score: s.Score, HitList: s.HitList,
					Pot: s.Pot, Fri: s.Fri, Kin: s.Kin, Rsi: s.Rsi, Td: s.Td,
					CreatedAt: nowMs,
				})
			}
			if err := store.Ingest(repo.StorePayload{Signal: rows}); err != nil {
				logx.Logf("WARN", "写信号失败：%v", err)
			}
		}
		pScan()

		pEntries := ph("eng.entries")
		n, updates, extra := runEntries(cfg, cli, store, res, ctr, openPos, account, b)
		if len(updates) > 0 {
			if err := store.Ingest(repo.StorePayload{SignalUpdate: updates}); err != nil {
				logx.Logf("WARN", "更新信号状态失败：%v", err)
			}
		}
		pEntries()

		// 本周期新开的仓 → 立刻让后面的周期看见（防重复开仓 + 当日笔数准确）
		openPos = append(openPos, extra...)
		ctr.OrdersToday += n
		openedTotal += n

		lines = append(lines, fmt.Sprintf("%s：全市场 %d / 候选 %d / 实算 %d / 信号 %d / 开仓 %d / K线本地 %d 网络 %d",
			b, res.Universe, res.Candidates, res.Scanned, len(res.Signals), n, res.FromDB, res.FromNet))
	}
	scanInfo := strings.Join(lines, "；")
	opened := openedTotal

	pTail := ph("eng.tail")
	// 只要这一轮至少扫了一个周期就记一行 —— 日志前缀列出实际参与扫描的周期，
	// 这样「1m/3m/5m/15m 到底在不在跑」看一眼日志就知道，不用去翻配置。
	if closed > 0 || added > 0 || len(lines) > 0 {
		logx.Logf("INFO", "[%s] %s；在持 %d 仓，本轮开仓 %d、平仓 %d、加仓 %d，用时 %s",
			strings.Join(bars, ","), scanInfo, len(openPos), opened, closed, added,
			time.Since(start).Round(time.Millisecond))
	}

	// ③ 权益快照
	if account != nil {
		if err := store.Ingest(repo.StorePayload{Equity: []repo.EquityRow{{
			Ts: time.Now().UnixMilli(), TotalEq: account.TotalEq,
			Avail: account.AvailEq, Upl: account.Upl, PosCount: account.PosCount,
		}}}); err != nil {
			logx.Logf("WARN", "写权益快照失败：%v", err)
		}
	}

	// ④ 日志落库
	if logs := logx.TakeRunLogs(); len(logs) > 0 {
		store.Ingest(repo.StorePayload{Runlog: logs}) // 失败不再写日志，避免递归
	}

	// ⑤ 每 20 轮滚动清理一次
	if eng.cycles%20 == 0 {
		if err := store.Cleanup(); err != nil {
			logx.Logf("WARN", "滚动清理失败：%v", err)
		}
	}
	pTail()
	return nil
}

func (e *engineState) maybePauseOnErrors(cfg *conf.Config) {
	if cfg.Risk == nil || cfg.Risk.PauseOnAPIError <= 0 {
		return
	}
	if e.apiErrStreak >= cfg.Risk.PauseOnAPIError {
		e.pause(30*time.Minute, fmt.Sprintf("连续 %d 次 API 错误", e.apiErrStreak))
		e.apiErrStreak = 0
	}
}

// ---------------------------------------------------------------------------
// 出场（对应文案 §7.3）
// ---------------------------------------------------------------------------

// runExits 出场巡检。kdb 非 nil 时布林上轨判定读本地库（见 LatestSignal 注释），
// 传 nil 退回纯网络。两条路的口径逐位一致，只是快慢差两个数量级。
func runExits(cfg *conf.Config, cli *OKXClient, store *repo.Store, kdb KlineReader,
	openPos []repo.OpenPos, markPrices map[string]float64, bar string) (int, map[int64]bool) {

	closedIDs := map[int64]bool{}
	if len(openPos) == 0 {
		return 0, closedIDs
	}
	// 拿不到标记价就退回行情
	needQuote := false
	for _, p := range openPos {
		if markPrices[p.InstID] <= 0 {
			needQuote = true
			break
		}
	}
	if needQuote {
		if tk, err := cli.Tickers(); err == nil {
			for _, p := range openPos {
				if markPrices[p.InstID] <= 0 {
					if t, ok := tk[p.InstID]; ok && t.Last > 0 {
						markPrices[p.InstID] = t.Last
					}
				}
			}
		}
	}

	closed := 0
	nowMs := time.Now().UnixMilli()
	// 布林上轨判定要逐仓拉 K 线（LatestSignal → CandlesEnough → 每仓 2 次 OKX HTTP），
	// 是 runExits 里唯一的网络大户，单独计时。
	var bollCalls, bollFromDB int
	pBoll := perf.Track("runExits.boll")
	for _, p := range openPos {
		px := markPrices[p.InstID]
		if px <= 0 || p.EntryPx <= 0 {
			continue
		}
		pnlPct := (px/p.EntryPx - 1) * 100
		reason := ""

		if cfg.Exit.TakeProfitPct > 0 && pnlPct >= cfg.Exit.TakeProfitPct {
			reason = fmt.Sprintf("止盈 %+.2f%%", pnlPct)
		} else if cfg.Exit.StopLossPct > 0 && pnlPct <= -cfg.Exit.StopLossPct {
			reason = fmt.Sprintf("止损 %.2f%%", pnlPct)
		} else if cfg.Exit.MaxHoldMinutes > 0 {
			// 超时平仓（按分钟）。四期默认 60 = 1 小时。
			//
			// ★ 四期口径：止盈 +1%（上面第一条）**保留**，布林上轨关闭。
			//   所以这是「没摸到止盈线时的兜底离场」，不是唯一通道。
			// 用「分钟」而不是「根」是因为 1H 图和 15m 图的 4 根完全不是一个时长。
			// 实时巡检每 exit_sec（默认 3 秒）跑一次，到点立刻市价出，不等下一根 K 线收盘。
			if nowMs-p.OpenTs >= int64(cfg.Exit.MaxHoldMinutes)*60000 {
				// 措辞用「到点自动平仓」而不是旧的「未止盈」：
				// 超时的定义是「持有到期」，不是「没到止盈线」。
				reason = fmt.Sprintf("超时 %s 到点自动平仓", HoldText(cfg.Exit.MaxHoldMinutes))
			}
		} else if cfg.Exit.MaxHoldBars > 0 {
			dur := BarDurationMs(p.Bar)
			if dur <= 0 {
				dur = BarDurationMs(cfg.Bar)
			}
			if dur > 0 && nowMs-p.OpenTs >= int64(cfg.Exit.MaxHoldBars)*dur {
				reason = fmt.Sprintf("超时 %d 根", cfg.Exit.MaxHoldBars)
			}
		}
		if reason == "" && cfg.Exit.BollUpperExit {
			bollCalls++
			sig, _, fromDB, err := LatestSignal(cfg, cli, kdb, p.InstID, p.Bar)
			if fromDB {
				bollFromDB++
			}
			if err == nil && sig != nil &&
				!isNaN(sig.BollUp) && sig.Close > sig.BollUp {
				reason = "布林上轨（均值回归出场）"
			}
		}
		if reason == "" {
			continue
		}

		if err := closeOne(cfg, cli, store, p, px, reason); err != nil {
			logx.Logf("ERROR", "%s 平仓失败，下一轮重试：%v", p.InstID, err)
			continue
		}
		closed++
		closedIDs[p.ID] = true
	}
	pBoll()
	if bollCalls > 0 {
		perf.Count("runExits.bollCalls", int64(bollCalls))
		perf.Count("runExits.bollFromDB", int64(bollFromDB))
	}
	return closed, closedIDs
}

// closeOne 平掉一个仓位：下单（或 dry_run 跳过）+ 写回平仓记录 + 记日志。
//
// 出场有两条路会调它：常规出场（runExits）和「加仓加满仍在亏」（runAddons）。
// 抽出来是为了两边的下单口径、滑点处理、落库字段完全一致 —— 复制一遍迟早改漏。
func closeOne(cfg *conf.Config, cli *OKXClient, store *repo.Store,
	p repo.OpenPos, px float64, reason string) error {

	if px <= 0 || p.EntryPx <= 0 {
		return fmt.Errorf("价格缺失（px=%.6f entryPx=%.6f）", px, p.EntryPx)
	}
	pnlPct := (px/p.EntryPx - 1) * 100
	pnl := p.Margin * float64(p.Leverage) * pnlPct / 100

	ordID := "(dry_run)"
	if !cfg.DryRun {
		ord, err := cli.PlaceOrder(p.InstID, cfg.Entry.TdMode, "sell", cfg.Entry.PosSide,
			"market", fmtSz(p.Sz, 10), true)
		if err != nil {
			return err
		}
		ordID = ord.OrdID
	}

	row := repo.CloseRow{
		ID: p.ID, ExitPx: px, Pnl: pnl, PnlPct: pnlPct,
		Reason: reason, CloseTs: time.Now().UnixMilli(), OrdID: ordID,
	}
	if err := store.Ingest(repo.StorePayload{
		CloseTrade: &row,
		// 平仓流水：图上标「平仓」并带出平仓价与盈亏美金。
		Event: []repo.TradeEventRow{{
			InstID: p.InstID, Kind: "close", Ts: row.CloseTs, Px: px,
			Sz: p.Sz, Margin: p.Margin, Leverage: p.Leverage,
			Pnl: pnl, PnlPct: pnlPct, Reason: reason, OrdID: ordID, TradeID: p.ID,
		}},
	}); err != nil {
		logx.Logf("WARN", "写平仓记录失败：%v", err)
	}
	logx.Logf("SIGNAL", "平仓 %s 张数=%s 开仓价=%.6f 平仓价=%.6f 盈亏=%+.4fU(%+.2f%%) 原因=%s",
		p.InstID, fmtSz(p.Sz, 10), p.EntryPx, px, pnl, pnlPct, reason)
	return nil
}

// ---------------------------------------------------------------------------
// 持仓对账：OKX 实时持仓 ↔ 本地「在持仓」
// ---------------------------------------------------------------------------

// ghostConfirmRounds 连续几轮在 OKX 持仓列表里看不到，才认定本地那行是「幽灵仓」。
//
// 出场巡检 3 秒一次 → 3 轮 ≈ 9 秒。宁可多等两轮，也不能把真仓位误判掉。
const ghostConfirmRounds = 3

// ghostDecision 纯判定（不碰网络、不碰库，方便单测）。
//
// 输入：上一轮遗留的缺席计数 seen、本轮 OKX 持仓集合 liveIDs、本地在持仓 openPos。
// 输出：本轮仍要保留的仓位、已确认的幽灵仓、更新后的缺席计数。
//
// 计数规则：
//   - 出现在 liveIDs 里 → 计数清零（仓位回来了，或本来就活着）
//   - 不在 → 计数 +1；累计到 ghostConfirmRounds 就判为幽灵并**从计数里移除**。
//     外层写库失败会把它放回 openPos，下一轮从 0 重新数 —— 这是刻意的：
//     写库一直失败时宁可从零再等 3 轮，也不要每轮都重复判一次、把 WARN 刷爆。
//
// 这个函数决定了「什么样的仓位会被引擎放弃管理」，是全链路里风险最不对称的一段，
// 所以单独抽出来用单测覆盖（见 reconcile_test.go）。
func ghostDecision(seen map[string]int, liveIDs map[string]bool,
	openPos []repo.OpenPos) (keep, ghosts []repo.OpenPos, next map[string]int) {

	next = make(map[string]int, len(seen))
	for k, v := range seen {
		next[k] = v
	}
	// 已经不在 openPos 里的陈旧计数顺手清掉，别让它无限长。
	alive := make(map[string]bool, len(openPos))
	for _, p := range openPos {
		alive[p.InstID] = true
	}
	for k := range next {
		if !alive[k] {
			delete(next, k)
		}
	}

	keep = make([]repo.OpenPos, 0, len(openPos))
	for _, p := range openPos {
		if liveIDs[p.InstID] {
			delete(next, p.InstID)
			keep = append(keep, p)
			continue
		}
		next[p.InstID]++
		if next[p.InstID] < ghostConfirmRounds {
			keep = append(keep, p)
			continue
		}
		delete(next, p.InstID)
		ghosts = append(ghosts, p)
	}
	return keep, ghosts, next
}

// reconcilePositions 用 OKX 的实时持仓列表校对本地「在持仓」，剔除幽灵仓。
//
// ★ 为什么必须做 ★
//
// 本地 trade 表是「开仓时插入、平仓时改写 closed=1」的。只要出现过一次
// 「OKX 侧其实已经没这个仓了，但本地没改」的情形，本地就会**永远**留着一行在持仓：
//
//	· 在 OKX App 里手工平掉 / 被强平；
//	· 平仓在 OKX 侧成功、但写回本地库失败（DB 抖动、进程被杀）；
//	· 早期 dry_run 期留下的空仓。
//
// 之后引擎每 3 秒去平一个**不存在**的仓位 → OKX 回
// `code=1 All operations failed` → 无限重试。实测后果：
// 日志每 3 秒刷一条 ERROR、白吃限频令牌（20 次/2 秒是扫描/回补/刷新/出场**共用**的
// 一把闸门）、出场巡检被拖到 24 秒 —— 直接饿死四个周期的入场扫描。
//
// ★ 判据保守到近乎多疑，因为误判的代价不对称 ★
//
//	误判成幽灵（真仓位其实还开着）→ 本地不再管它，**等于没有任何出场保护**，
//	在「不设止损」的策略下就是敞口裸奔；
//	漏判（继续重试）→ 只是浪费几次接口调用。
//
// 所以三个条件**同时**满足才认定：
//
//	① OKX 持仓接口调用成功（失败就什么都不动，并清空计数）；
//	② 账户余额接口也成功（能证明 key / 网络是活的，不是半死状态返回空列表）；
//	③ 同一个合约**连续 ghostConfirmRounds 轮**都不在 OKX 持仓列表里。
//
// 另外 dry_run 必须整段跳过 —— 模拟模式下本来就一个真仓都不会有，
// 不挡的话对账会把所有 dry_run 持仓当成幽灵清掉，dry_run 就没法用了。
//
// 本函数**只改本地库，绝不向 OKX 发任何交易请求**。
//
// 返回：剔除幽灵仓之后的在持仓列表。
func reconcilePositions(cfg *conf.Config, store *repo.Store,
	okxAlive bool, liveIDs map[string]bool, markPrices map[string]float64,
	openPos []repo.OpenPos) []repo.OpenPos {

	if eng.ghostSeen == nil {
		eng.ghostSeen = map[string]int{}
	}
	// dry_run / 没配 key：没有任何可对账的依据，原样返回。
	if cfg == nil || cfg.DryRun || !hasKeys(cfg) || store == nil {
		return openPos
	}
	// 接口没成功：清空计数并原样返回。**不清计数最危险** ——
	// 偶尔一次空返回 + 上一轮的计数会把真仓位凑够 3 轮给误判掉。
	if !okxAlive {
		eng.ghostSeen = map[string]int{}
		return openPos
	}
	if len(openPos) == 0 {
		eng.ghostSeen = map[string]int{}
		return openPos
	}

	keep, ghosts, next := ghostDecision(eng.ghostSeen, liveIDs, openPos)
	eng.ghostSeen = next
	if len(ghosts) == 0 {
		return keep
	}

	for _, p := range ghosts {
		px := markPrices[p.InstID]
		if px <= 0 {
			px = p.EntryPx
		}
		// 盈亏**故意记 0**：真实成交价本地无从得知，硬套标记价会往
		// totalPnl / winRate 里灌假数据（用户会拿三方流水核对，宁缺勿假）。
		// 真实盈亏以 OKX 账单 / 仓位历史为准，这里只负责把本地状态收干净。
		const reason = "OKX 侧已无此持仓 · 本地对账平仓（盈亏以 OKX 账单为准）"
		row := repo.CloseRow{
			ID: p.ID, ExitPx: px, Pnl: 0, PnlPct: 0,
			Reason: reason, CloseTs: time.Now().UnixMilli(),
			OrdID: "(reconcile)",
		}
		if err := store.Ingest(repo.StorePayload{
			CloseTrade: &row,
			Event: []repo.TradeEventRow{{
				InstID: p.InstID, Kind: "close", Ts: row.CloseTs, Px: px,
				Sz: p.Sz, Margin: p.Margin, Leverage: p.Leverage,
				Pnl: 0, PnlPct: 0, Reason: reason, OrdID: row.OrdID, TradeID: p.ID,
			}},
		}); err != nil {
			// 写失败就把这行留下，下一轮重试 —— 绝不能「本地删了、OKX 也没有」两头不着。
			logx.Logf("WARN", "对账平仓写库失败（%s，下一轮重试）：%v", p.InstID, err)
			keep = append(keep, p)
			continue
		}
		logx.Logf("WARN", "对账平仓 %s：连续 %d 轮不在 OKX 持仓列表，本地记为已平（张数=%s 开仓价=%.6f 保证金=%.4fU）",
			p.InstID, ghostConfirmRounds, fmtSz(p.Sz, 10), p.EntryPx, p.Margin)
	}
	return keep
}

// livePositionIDs 把 OKX 持仓列表压成「合约 → 是否有仓」的集合。
//
// 只认张数非 0 的那条：OKX 在双向持仓下会对「已平的腿」回一条 pos=0 的记录，
// 那种不算有仓。
func livePositionIDs(ps []Position) map[string]bool {
	out := make(map[string]bool, len(ps))
	for _, p := range ps {
		if toF(p.Pos) == 0 {
			continue
		}
		out[p.InstID] = true
	}
	return out
}

// mergeIDs 把 b 里的 ID 并进 a（a 为空时直接返回 b）。
//
// 一轮里可能有两条路同时平仓：常规出场（止盈/超时/布林上轨）和
// 「加仓加满仍在亏」（addon.go）。两拨平掉的仓位都要让后面的入场闸门看见，
// 否则会出现「刚平掉的仓又占着并发额度」这种诡异现象。
func mergeIDs(a, b map[int64]bool) map[int64]bool {
	if len(b) == 0 {
		return a
	}
	if a == nil {
		a = make(map[int64]bool, len(b))
	}
	for id := range b {
		a[id] = true
	}
	return a
}

// dropClosed 把本轮已平掉的仓位从在持仓列表里摘掉。
// 不做这一步的话，同一轮里刚平掉的仓还会被当成「在持仓」：
// 加仓会对已平仓的単子补仓、开仓闸门也会被无谓占用。
func dropClosed(pos []repo.OpenPos, closedIDs map[int64]bool) []repo.OpenPos {
	if len(closedIDs) == 0 {
		return pos
	}
	out := make([]repo.OpenPos, 0, len(pos))
	for _, p := range pos {
		if !closedIDs[p.ID] {
			out = append(out, p)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 入场（对应文案 §7.6 的判定顺序）
// ---------------------------------------------------------------------------

// runEntries 对某个周期的扫描结果过闸门并下单。
//
// 返回值第三个 extra 是本轮**新开的仓**（只带 InstID/Margin 这两个下游会用到的字段）。
// 调用方（engineRunBars）会把它并进 openPos 再喂给下一个周期 —— 不这么做的话，
// 1m 刚开的仓在 3m/5m/15m 那几轮里还是「没持仓」，同一个合约会被开 4 次。
//
// 注意 runEntries 内部也会就地维护 openSet 与 ctr.LastEntryTs（同周期内不重复开），
// 但那两份是**按周期重建**的，跨周期不共享，所以必须靠 extra 传出去。
func runEntries(cfg *conf.Config, cli *OKXClient, store *repo.Store, res *ScanResult, ctr *repo.Counters,
	openPos []repo.OpenPos, account *Account, bar string) (int, []repo.SignalUpdate, []repo.OpenPos) {

	updates := []repo.SignalUpdate{}
	extra := []repo.OpenPos{}
	if len(res.Signals) == 0 {
		return 0, updates, extra
	}

	insts, ierr := cli.Instruments(false)
	if ierr != nil {
		logx.Logf("WARN", "取合约信息失败，本轮不开仓：%v", ierr)
		for _, s := range res.Signals {
			updates = append(updates, repo.SignalUpdate{InstID: s.InstID, Bar: s.Bar, Ts: s.Ts,
				Acted: 2, Reason: "取合约信息失败"})
		}
		return 0, updates, extra
	}

	openSet := map[string]bool{}
	marginSum := 0.0
	for _, p := range openPos {
		openSet[p.InstID] = true
		marginSum += p.Margin
	}
	durMs := BarDurationMs(bar)
	if durMs <= 0 {
		durMs = BarDurationMs(cfg.Bar)
	}

	blockAll, blockWhy := globalRiskBlock(cfg, ctr, account, marginSum)
	if blockAll {
		logx.Logf("WARN", "全局风控拦截，本轮只记信号不下单：%s", blockWhy)
	}

	opened := 0
	for _, s := range res.Signals {
		upd := repo.SignalUpdate{InstID: s.InstID, Bar: s.Bar, Ts: s.Ts}
		skip := func(why string) {
			upd.Acted = 2
			upd.Reason = why
			updates = append(updates, upd)
		}

		// —— §7.6 闸门，顺序照抄 ——
		if blockAll {
			skip(blockWhy)
			continue
		}
		if paused, why := eng.paused(); paused {
			skip("策略暂停：" + why)
			continue
		}
		// ★ 下面两条上限都是「<= 0 = 不限」（2026-10-01，用户口径「取消限制」）★
		// 所以判定必须先 `> 0` 再看有没有超 —— 不然 0 会变成「已达上限 0」，
		// 第一笔信号就被拦掉，症状跟用户反馈的「买得太少」一模一样。
		if cfg.Entry.MaxConcurrentPositions > 0 &&
			len(openPos)+opened >= cfg.Entry.MaxConcurrentPositions {
			skip(fmt.Sprintf("持仓数已达上限 %d", cfg.Entry.MaxConcurrentPositions))
			continue
		}
		if openSet[s.InstID] {
			skip("该合约已持仓")
			continue
		}
		if last, ok := ctr.LastEntryTs[s.InstID]; ok && last > 0 && durMs > 0 &&
			s.Ts-last < int64(cfg.Entry.CooldownBars)*durMs {
			skip(fmt.Sprintf("冷却中（距上次开仓不足 %d 根）", cfg.Entry.CooldownBars))
			continue
		}
		if cfg.Entry.DailyMaxEntries > 0 && ctr.OrdersToday+opened >= cfg.Entry.DailyMaxEntries {
			skip(fmt.Sprintf("当日开仓已达上限 %d", cfg.Entry.DailyMaxEntries))
			continue
		}

		ins, ok := insts[s.InstID]
		if !ok {
			skip("合约信息缺失")
			continue
		}
		sz, marginUsed, serr := calcSize(cfg, ins, s.Close)
		if serr != nil {
			skip(serr.Error())
			continue
		}

		// —— 下单 ——
		ordID := "(dry_run)"
		if cfg.OrderVia == "legacy" || cfg.OrderVia == "python" {
			if cfg.DryRun {
				logx.Logf("INFO", "[dry_run] 应走 legacy orderbuy：%s %s", s.InstID, bar)
			} else if err := legacyOrder(cfg, s.InstID, bar); err != nil {
				skip("legacy 下单失败：" + truncate(err.Error(), 100))
				logx.Logf("ERROR", "%s legacy 下单失败：%v", s.InstID, err)
				continue
			} else {
				ordID = "(legacy)"
			}
		} else if cfg.DryRun {
			// 杠杆打「真实生效」的那个（合约上限可能低于配置），
			// 否则 10x 上限的合约在日志里会显示成 20x。
			logx.Logf("INFO", "[dry_run] 应开仓 %s 张数=%s 保证金=%.2fU 杠杆=%dx 价格=%.6f 共振=%d/8",
				s.InstID, fmtSz(sz, ins.LotSzDec), marginUsed,
				effLever(ins.Lever, cfg.Entry.Leverage), s.Close, s.Score)
		} else {
			// 杠杆收敛到合约上限（见 calcSizeWith 的注释）：
			// 对 10x 上限的合约调 20x 会被 OKX 拒（59102），然后下单直接失败。
			if lev := effLever(ins.Lever, cfg.Entry.Leverage); lev != cfg.Entry.Leverage {
				logx.Logf("INFO", "%s 合约最高 %dx < 配置 %dx，按 %dx 下单",
					s.InstID, ins.Lever, cfg.Entry.Leverage, lev)
			}
			if err := cli.SetLeverage(s.InstID, effLever(ins.Lever, cfg.Entry.Leverage), cfg.Entry.TdMode); err != nil {
				logx.Logf("WARN", "%s 设杠杆失败（继续下单）：%v", s.InstID, err)
			}
			ord, err := cli.PlaceOrder(s.InstID, cfg.Entry.TdMode, "buy", cfg.Entry.PosSide,
				cfg.Entry.OrdType, fmtSz(sz, ins.LotSzDec), false)
			if err != nil {
				skip("下单失败：" + truncate(err.Error(), 100))
				logx.Logf("ERROR", "%s 下单失败：%v", s.InstID, err)
				continue
			}
			ordID = ord.OrdID
		}

		// —— AI 解读：只对「过闸门、真开仓（含 dry_run 模拟开仓）」的信号调用 ——
		aiNote := ""
		if cfg.AI != nil && cfg.AI.Enabled {
			note, aerr := AIReview(cfg, cli, s, s.Close)
			if aerr != nil {
				aiNote = "AI 解读不可用"
				logx.Logf("WARN", "AI 解读失败（不影响交易）：%v", aerr)
			} else {
				aiNote = note
				logx.Logf("SIGNAL", "AI 点评 %s：%s", s.InstID, note)
			}
		}

		reason := fmt.Sprintf("8因子共振 %d/8（%s）", s.Score, s.HitList)
		if err := store.Ingest(repo.StorePayload{
			Trade: []repo.TradeRow{{
				InstID: s.InstID, Side: "buy", Sz: sz, EntryPx: s.Close,
				Margin: marginUsed, Leverage: cfg.Entry.Leverage,
				OpenTs: s.Ts, Score: s.Score, Bar: s.Bar, Reason: reason,
				OrdID: ordID, Status: "open", AINote: aiNote,
			}},
			// 事件流水：K 线图上的「买入」标记 + 历史交易记录详情都读它。
			// 带上 margin（买入多少美金）和张数，图上的提示框才有东西可显示。
			Event: []repo.TradeEventRow{{
				InstID: s.InstID, Kind: "open", Ts: s.Ts, Px: s.Close,
				Sz: sz, Margin: marginUsed, Leverage: cfg.Entry.Leverage,
				Score: s.Score, Reason: reason, OrdID: ordID,
			}},
		}); err != nil {
			logx.Logf("WARN", "写成交失败：%v", err)
		}

		upd.Acted = 1
		upd.Reason = reason
		upd.AINote = aiNote
		updates = append(updates, upd)

		openSet[s.InstID] = true
		ctr.LastEntryTs[s.InstID] = s.Ts
		opened++
		// 传给下一个周期：只需要 InstID（防重复开仓）与 Margin（总保证金口径）
		extra = append(extra, repo.OpenPos{InstID: s.InstID, Margin: marginUsed, EntryPx: s.Close, Bar: s.Bar})
		logx.Logf("SIGNAL", "开仓 %s 张数=%s 开仓价=%.6f 共振 %d/8 [%s] 周期=%s 订单=%s",
			s.InstID, fmtSz(sz, ins.LotSzDec), s.Close, s.Score, s.HitList, s.Bar, ordID)
	}

	return opened, updates, extra
}

// ---------------------------------------------------------------------------
// 风控（对应文案 §7.5）
// ---------------------------------------------------------------------------

func globalRiskBlock(cfg *conf.Config, ctr *repo.Counters, account *Account, marginSum float64) (bool, string) {
	r := cfg.Risk
	if r == nil {
		return false, ""
	}
	if r.ConsecutiveLossPause > 0 && ctr.ConsecutiveLosses >= r.ConsecutiveLossPause {
		return true, fmt.Sprintf("连亏 %d 笔，超过 %d 笔暂停阈值", ctr.ConsecutiveLosses, r.ConsecutiveLossPause)
	}
	if account == nil {
		// 没配 Key（例如 dry_run 观察期）→ 只做统计类风控
		return false, ""
	}
	if r.AccountEquityStop > 0 && account.TotalEq > 0 && account.TotalEq < r.AccountEquityStop {
		return true, fmt.Sprintf("权益 %.2f 低于下限 %.2f", account.TotalEq, r.AccountEquityStop)
	}
	if r.MinAvailableUSDT > 0 && account.AvailEq < r.MinAvailableUSDT {
		return true, fmt.Sprintf("可用余额 %.2f 低于下限 %.2f", account.AvailEq, r.MinAvailableUSDT)
	}
	if r.DailyLossStopPct > 0 && account.TotalEq > 0 && ctr.TodayPnl < 0 {
		lossPct := -ctr.TodayPnl / account.TotalEq * 100
		if lossPct >= r.DailyLossStopPct {
			return true, fmt.Sprintf("当日亏损 %.2f%% 超过 %.2f%%", lossPct, r.DailyLossStopPct)
		}
	}
	if r.MaxTotalMarginPct > 0 && account.TotalEq > 0 {
		if pct := marginSum / account.TotalEq * 100; pct >= r.MaxTotalMarginPct {
			return true, fmt.Sprintf("总保证金占比 %.2f%% 超过 %.2f%%", pct, r.MaxTotalMarginPct)
		}
	}
	return false, ""
}

// ---------------------------------------------------------------------------
// 手数（对应文案 §7.1）
// ---------------------------------------------------------------------------

// calcSize 张数 = 保证金 × 杠杆 / (ctVal × ctMult × price)，向下对齐 lotSz 且不小于 minSz
//
// 返回 (张数, 实际占用保证金, 错误)。
// OKX 永续的 sz 单位是「张」：BTC 一张≈600U、ETH≈300U，所以 1U×20x=20U 名义买不起它们。
// 这时按 entry.margin_policy 决定：fixed 跳过；min_one 放大到刚好买 1 张（不超过 max_margin_usdt）。
func calcSize(cfg *conf.Config, ins Instrument, price float64) (float64, float64, error) {
	return calcSizeWith(cfg, ins, price, cfg.Entry.MarginUSDT, cfg.Entry.MaxMarginUSDT, cfg.Entry.MarginPolicy)
}

// calcSizeWith 按指定保证金 / 上限 / 口径算张数（加仓复用同一套逻辑）。
//
//	price        当前价（用来算每张名义价值）
//	margin       这笔想投多少保证金
//	cap          这笔的保证金硬上限（0 = 不限）
//	policy       "fixed" 买不起就报错 / "min_one" 放大到刚好 1 张（≤ cap）
func calcSizeWith(cfg *conf.Config, ins Instrument, price, margin, cap float64,
	policy string) (float64, float64, error) {

	if price <= 0 {
		return 0, 0, fmt.Errorf("价格非法")
	}
	per := ins.CtVal * ins.CtMult * price
	if per <= 0 {
		return 0, 0, fmt.Errorf("每张名义价值非法（ctVal=%.8g ctMult=%.8g）", ins.CtVal, ins.CtMult)
	}
	lot := ins.LotSz
	if lot <= 0 {
		lot = 1
	}
	// 最小可下单位：lotSz 与 minSz 取大（并按 lotSz 对齐）
	unit := lot
	if ins.MinSz > unit {
		unit = math.Ceil(ins.MinSz/lot) * lot
	}

	// ★ 杠杆必须与「准入过滤」用同一个口径（2026-10-01）。
	//
	// universe.go 算「这笔买不买得起」时用的是 effLever(合约上限, 策略杠杆)，
	// 但这里原来直接用 cfg.Entry.Leverage —— 两条路不一致，后果是：
	// 有一批合约 OKX 上限只有 10x（成交额前 80 里就有 4 个：USELESS / CAP / ONE / PROS），
	// 准入按 10x 算「买得起」放行，下单却按 20x 去 set-leverage → code=59102 被拒 →
	// 紧接着下单 code=1 全失败。**这些合约永远买不进来，而且日志里只是一条 WARN。**
	// 现象正是用户反馈的「买得太少」。
	//
	// 收敛之后张数按真实杠杆算：单笔保证金仍然按 margin 口径（1U），
	// 只是名义价值随杠杆变小、风险更小，不会超买。
	lev := float64(effLever(ins.Lever, cfg.Entry.Leverage))
	if lev <= 0 {
		lev = 1
	}
	if margin <= 0 {
		return 0, 0, fmt.Errorf("保证金预算为 0，跳过")
	}
	sz := math.Floor(margin*lev/per/lot+1e-9) * lot

	if sz < unit {
		if strings.EqualFold(policy, "min_one") {
			needMargin := unit * per / lev
			if cap > 0 && needMargin > cap {
				return 0, 0, fmt.Errorf("买 %s 张至少要 %.2fU 保证金，超过上限 %.2fU（每张名义 %.2fU）",
					fmtSz(unit, ins.LotSzDec), needMargin, cap, per)
			}
			return unit, needMargin, nil
		}
		needMargin := unit * per / lev
		return 0, 0, fmt.Errorf("名义 %.0fU 买不起 %s 张（每张名义 %.2fU，至少要 %.2fU 保证金）"+
			"；调大 margin_usdt，或把 margin_policy 改成 \"min_one\"",
			margin*lev, fmtSz(unit, ins.LotSzDec), per, needMargin)
	}
	return sz, margin, nil
}

// fmtSz 按小数位格式化张数（OKX 的 sz 是字符串，多一位精度都会报错）
func fmtSz(sz float64, dec int) string {
	if dec < 0 {
		dec = 0
	}
	if dec > 10 {
		dec = 10
	}
	s := strconv.FormatFloat(sz, 'f', dec, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	if s == "" || s == "-" {
		s = "0"
	}
	return s
}

// ---------------------------------------------------------------------------
// 其它
// ---------------------------------------------------------------------------

func normalizeBar(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "15m"
	}
	return s
}

func hasKeys(cfg *conf.Config) bool {
	return cfg.OKX != nil && cfg.OKX.APIKey != "" && cfg.OKX.SecretKey != "" && cfg.OKX.Passphrase != ""
}

// HoldText 把「超时平仓」的分钟数说成人话：240 → "4 小时"、90 → "90 分钟"。
//
// 落库的平仓原因、日志、网页展示都用它，保证三处口径一模一样
// （以前是一处写「60 分钟」、一处写「1 小时」，对不上）。
func HoldText(minutes int) string {
	if minutes <= 0 {
		return "不超时"
	}
	if minutes%60 == 0 && minutes >= 60 {
		return fmt.Sprintf("%d 小时", minutes/60)
	}
	return fmt.Sprintf("%d 分钟", minutes)
}

// ExitText 把出场配置渲染成一句人话，给启动日志与 /api/state 共用。
//
// 四期口径（2026-10-02）下应当只输出「超时 1 小时」：
// 止盈与布林上轨都已关闭，仓位只有等满 60 分钟才市价离场。
//
// 之所以要求「关闭」的项不出现在文案里，是因为启动日志里写
// 「止盈 0.00%」会让人以为止盈开着、只是线设在 0 —— 与事实相反。
func ExitText(tpPct float64, boll bool, holdMin int) string {
	s := ""
	addp := func(x string) {
		if s != "" {
			s += " / "
		}
		s += x
	}
	if tpPct > 0 {
		addp(fmt.Sprintf("止盈 %+.2f%%", tpPct))
	}
	if boll {
		addp("布林上轨")
	}
	if holdMin > 0 {
		addp("超时 " + HoldText(holdMin))
	}
	if s == "" {
		return "无（不会自动平仓）"
	}
	return s
}

func accountOrNew(a *Account) *Account {
	if a == nil {
		return &Account{}
	}
	return a
}

func todayStartMs() int64 {
	now := time.Now()
	y, m, d := now.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, now.Location()).UnixMilli()
}

// legacyOrder 曾经是 exec.Command("python", "gorun.py", ...) 那条老路。
// 现在 gorun.py 已经删掉、逻辑全在 mvc 包里，这里直接调纯 Go 的 orderbuy，
// 一个外部进程都不再拉起来。函数名保持不动，免得动 cfg.order_via 的取值语义。
func legacyOrder(cfg *conf.Config, instID, bar string) error {
	ops, err := DefaultOps()
	if err != nil {
		return fmt.Errorf("初始化账户失败：%w", err)
	}
	msg, err := ops.OrderBuy(instID, bar, false)
	if err != nil {
		return fmt.Errorf("下单失败：%w", err)
	}
	logx.Logf("INFO", "下单完成：%s", truncate(msg, 300))
	return nil
}
