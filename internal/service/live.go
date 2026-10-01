package service

// live.go —— 实盘实时引擎：秒级的「止盈巡检」+ 分钟级的「买入信号扫描」
//
// 和 handler/cron.go 的区别（两条路可以同时存在，互不冲突）：
//
//	cron   是「到点跑一轮」，最短 1 分钟，且一轮里「扫描 + 出场」捆在一起；
//	live   拆成两条独立心跳：
//
//	  · 止盈巡检（默认 3 秒）—— 只看在持仓，浮盈 ≥ 止盈线立刻市价平掉。
//	    为什么要这么快：持仓盈亏是实时在跑的，可能这一秒 +1.2%、下一秒
//	    就掉回 -0.3%。1 分钟去看一眼，等于把到手的钱又还回去。
//
//	  · 信号扫描（默认 60 秒）—— 全市场扫一遍买入信号，有就下单。
//	    这个贵（要拉几十个合约的 K 线，受 OKX 限频约束），频率不能高。
//
// 两条心跳都抢同一把 eng.mu，天然串行：不会出现「正扫着，仓位被平了」。

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"finally-main/internal/conf"
	"finally-main/internal/logx"
	"finally-main/internal/perf"
	"finally-main/internal/repo"
)

// ---------------------------------------------------------------------------
// 参数
// ---------------------------------------------------------------------------

// LiveOptions 实时引擎参数
type LiveOptions struct {
	ExitEvery  time.Duration // 止盈巡检间隔
	EntryEvery time.Duration // 买入信号扫描间隔
	Bar        string        // 用哪个周期做买卖判断
	Log        func(format string, args ...any)
}

func (o *LiveOptions) normalize() {
	if o.ExitEvery <= 0 {
		o.ExitEvery = 3 * time.Second
	}
	if o.EntryEvery <= 0 {
		o.EntryEvery = 60 * time.Second
	}
	if strings.TrimSpace(o.Bar) == "" {
		o.Bar = "15m"
	}
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	// 巡检不可能比 1 秒还勤，OKX 也不需要
	if o.ExitEvery < time.Second {
		o.ExitEvery = time.Second
	}
}

// LiveStatus 运行状态（给 /api/state 显示）
type LiveStatus struct {
	Running      bool    `json:"running"`
	ExitEverySec float64 `json:"exitEverySec"`
	EntryEverySec float64 `json:"entryEverySec"`
	Bar          string  `json:"bar"`
	Ticks        int64   `json:"ticks"`         // 止盈巡检跑了几轮
	Scans        int64   `json:"scans"`         // 信号扫描跑了几轮
	Exits        int64   `json:"exits"`         // 累计自动平仓笔数
	Opens        int64   `json:"opens"`         // 累计自动开仓笔数
	LastTickTs   int64   `json:"lastTickTs"`    // 上次巡检时间
	LastScanTs   int64   `json:"lastScanTs"`    // 上次扫描时间
	LastTickMs   int64   `json:"lastTickMs"`    // 上次巡检耗时（毫秒）
	LastScanMs   int64   `json:"lastScanMs"`    // 上次扫描耗时（毫秒）
	LastError    string  `json:"lastError"`     // 最近的错误（空=没有）
	LastNote     string  `json:"lastNote"`      // 最近一轮的结果摘要
	Equity       float64 `json:"equity"`        // 最近一次拿到的账户权益
	Avail        float64 `json:"avail"`         // 可用余额
	Upl          float64 `json:"upl"`           // 未实现盈亏
	PosCount     int     `json:"posCount"`      // 在持仓数
	EquityTs     int64   `json:"equityTs"`      // 权益快照时间
	DryRun       bool    `json:"dryRun"`
	Simulated    bool    `json:"simulated"`
	HasKeys      bool    `json:"hasKeys"`
}

var (
	liveMu     sync.RWMutex
	liveRun    bool
	liveOpts   LiveOptions
	liveCnt    liveCounters
	liveSnap   LiveStatus
)

type liveCounters struct {
	ticks  atomic.Int64
	scans  atomic.Int64
	exits  atomic.Int64
	opens  atomic.Int64
}

// LiveRunning 实时引擎在跑吗
func LiveRunning() bool {
	liveMu.RLock()
	defer liveMu.RUnlock()
	return liveRun
}

// LiveStatusSnapshot 取一份状态快照（带累计计数）
func LiveStatusSnapshot() LiveStatus {
	liveMu.RLock()
	defer liveMu.RUnlock()
	s := liveSnap
	s.Running = liveRun
	s.Bar = liveOpts.Bar
	s.ExitEverySec = liveOpts.ExitEvery.Seconds()
	s.EntryEverySec = liveOpts.EntryEvery.Seconds()
	s.Ticks = liveCnt.ticks.Load()
	s.Scans = liveCnt.scans.Load()
	s.Exits = liveCnt.exits.Load()
	s.Opens = liveCnt.opens.Load()
	return s
}

func setLiveNote(format string, args ...any) {
	liveMu.Lock()
	liveSnap.LastNote = fmt.Sprintf(format, args...)
	liveMu.Unlock()
}

func setLiveErr(err error) {
	liveMu.Lock()
	if err == nil {
		liveSnap.LastError = ""
	} else {
		liveSnap.LastError = err.Error()
	}
	liveMu.Unlock()
}

// ---------------------------------------------------------------------------
// 启动 / 停止
// ---------------------------------------------------------------------------

// StartLive 起实时引擎。ctx 结束就自然退出（不阻塞）。
func StartLive(ctx context.Context, opt LiveOptions) {
	opt.normalize()

	liveMu.Lock()
	if liveRun {
		liveMu.Unlock()
		return
	}
	liveRun = true
	liveOpts = opt
	liveMu.Unlock()

	cfg := conf.LoadConfig()
	if cfg != nil {
		liveMu.Lock()
		liveSnap.DryRun = cfg.DryRun
		liveSnap.Simulated = cfg.OKX.Simulated
		liveSnap.HasKeys = hasKeys(cfg)
		liveMu.Unlock()
	}

	go liveTickLoop(ctx, opt)
	go liveScanLoop(ctx, opt)

	opt.Log("实时引擎已启动：止盈巡检 %s / 信号扫描 %s / 周期 %s",
		opt.ExitEvery, opt.EntryEvery, opt.Bar)
}

func StopLive() {
	liveMu.Lock()
	liveRun = false
	liveMu.Unlock()
}

func liveTickLoop(ctx context.Context, opt LiveOptions) {
	// 先跑一次，别让用户等一个间隔才看到数字
	liveTickOnce(opt)
	t := time.NewTicker(opt.ExitEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			liveTickOnce(opt)
		}
	}
}

func liveScanLoop(ctx context.Context, opt LiveOptions) {
	t := time.NewTicker(opt.EntryEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			liveScanOnce(opt)
		}
	}
}

// liveTickOnce 跑一轮「止盈巡检」，顺带把账户快照写进 liveSnap（顶栏实时用）
func liveTickOnce(opt LiveOptions) {
	start := time.Now()
	liveCnt.ticks.Add(1)
	defer perf.Track("live.exitPass")()

	n, acct, err := exitPass()
	ms := time.Since(start).Milliseconds()

	liveMu.Lock()
	liveSnap.LastTickTs = time.Now().UnixMilli()
	liveSnap.LastTickMs = ms
	if acct != nil {
		liveSnap.Equity = acct.TotalEq
		liveSnap.Avail = acct.AvailEq
		liveSnap.Upl = acct.Upl
		liveSnap.PosCount = acct.PosCount
		liveSnap.EquityTs = time.Now().UnixMilli()
	}
	liveMu.Unlock()

	if err != nil {
		setLiveErr(err)
		return
	}
	setLiveErr(nil)
	if n > 0 {
		liveCnt.exits.Add(int64(n))
		setLiveNote("止盈巡检：平掉 %d 笔", n)
		opt.Log("⚡ 自动止盈：本轮平掉 %d 笔", n)
	}
}

// liveScanOnce 跑一轮买入信号扫描
func liveScanOnce(opt LiveOptions) {
	start := time.Now()
	cfg := conf.LoadConfig()
	if cfg == nil || !cfg.Enabled {
		return
	}
	bar := opt.Bar
	if bar == "" {
		bar = cfg.Bar
	}

	before := currentPosCount()
	if err := EngineRun(bar); err != nil {
		setLiveErr(err)
		return
	}
	setLiveErr(nil)
	after := currentPosCount()

	liveCnt.scans.Add(1)
	liveMu.Lock()
	liveSnap.LastScanTs = time.Now().UnixMilli()
	liveSnap.LastScanMs = time.Since(start).Milliseconds()
	liveSnap.PosCount = int(after)
	liveMu.Unlock()

	if after > before {
		liveCnt.opens.Add(after - before)
		setLiveNote("信号扫描：新开仓 %d 笔（在持 %d）", after-before, after)
		opt.Log("🚀 买入信号触发：新开仓 %d 笔", after-before)
	}
}

func currentPosCount() int64 {
	cfg := conf.LoadConfig()
	if cfg == nil {
		return 0
	}
	st := repo.NewStore(cfg)
	if err := st.Init(); err != nil {
		return 0
	}
	ps, err := st.OpenPositions()
	if err != nil {
		return 0
	}
	return int64(len(ps))
}

// ---------------------------------------------------------------------------
// 出场巡检（只做出场，不扫描，所以几秒一次也不贵）
// ---------------------------------------------------------------------------

// exitPass 一次出场巡检：
//
//	① 拿账户（权益 / 持仓 / 标记价）—— 一次 Balance + 一次 Positions
//	② 标记价缺失的用行情最新价兜底
//	③ 交给 runExits 判止盈 / 布林上轨 / 止损
//	④ 把权益快照写库，顶栏就能实时显示
//
// 返回：本轮平掉几笔 + 账户快照。
//
// ★ 打点说明（2026-10-01）★
// 外层的 live.exitPass 曾经是「等锁 + 干活」混在一起的一个数，实测 13~65 秒，
// 看上去像出场巡检本身很慢。但 ExitEvery 只有 3 秒、这里又只做几次 HTTP，
// 不可能要一分钟 —— 真正的原因是 eng.mu 被 EngineRun（一轮 18~62 秒）占着。
// 现在拆成 live.exitWait（等锁）与 live.exitWork（干活）两个计时器，
// 再加 exit.acct / exit.tickers / exit.store 三个子项，谁是大头一目了然。
func exitPass() (int, *Account, error) {
	cfg := conf.LoadConfig()
	if cfg == nil || !cfg.Enabled {
		return 0, nil, nil
	}

	// 和 EngineRun 抢同一把锁：保证「扫描」和「巡检」不会同时动同一个仓位
	waitDone := perf.Track("live.exitWait")
	eng.mu.Lock()
	waitDone()
	workDone := perf.Track("live.exitWork")
	defer eng.mu.Unlock()
	defer workDone()

	cli, err := eng.client(cfg)
	if err != nil {
		return 0, nil, err
	}
	if err := cli.EnsureReady(); err != nil {
		eng.apiErrStreak++
		eng.maybePauseOnErrors(cfg)
		return 0, nil, err
	}
	eng.apiErrStreak = 0

	// 探一次账户持仓模式（只探一次，之后走缓存）。
	// 首次启动就在这里打出「持仓模式=xxx → posSide 采用 xxx」，
	// 不用等到有信号才发现 posSide 参数不对。
	cli.PosMode()

	pStore := perf.Track("exit.store")
	store := repo.NewStore(cfg)
	if !eng.storeInit {
		if err := store.Init(); err != nil {
			logx.Logf("WARN", "MySQL 初始化失败（不影响交易）：%v", err)
		} else {
			eng.storeInit = true
		}
	}
	pStore()

	pAcct := perf.Track("exit.acct")
	var account *Account
	markPrices := map[string]float64{}

	if hasKeys(cfg) {
		if acc, aerr := cli.Balance(); aerr == nil {
			account = acc
		}
		if ps, perr := cli.Positions(); perr == nil {
			account = accountOrNew(account)
			account.PosCount = len(ps)
			account.PositionList = ps
			for _, p := range ps {
				if mp := toF(p.MarkPx); mp > 0 {
					markPrices[p.InstID] = mp
				}
			}
		}
	}
	pAcct()

	openPos, err := store.OpenPositions()
	if err != nil {
		logx.Logf("WARN", "读在持仓失败：%v", err)
		return 0, account, nil
	}

	// 标记价没拿到的，用行情最新价补
	//
	// ★ 注意这里和 runExits 里各有一份一模一样的兜底（2026-10-01 记录）：
	// 只要「库里有、OKX 持仓列表里没有」的仓位存在（dry_run 仓、被外部平掉的仓），
	// markPrices 就会永远缺，于是每一轮都去拉一次**全市场**行情（480 个合约、约 20KB）。
	// 3 秒一次、连着拉，是实打实的浪费。已在 next 轮改成「只补缺的那几个」。
	missing := false
	for _, p := range openPos {
		if markPrices[p.InstID] <= 0 {
			missing = true
			break
		}
	}
	if missing {
		pTk := perf.Track("exit.tickers")
		if tk, terr := cli.Tickers(); terr == nil {
			for _, p := range openPos {
				if markPrices[p.InstID] <= 0 {
					if t, ok := tk[p.InstID]; ok && t.Last > 0 {
						markPrices[p.InstID] = t.Last
					}
				}
			}
		}
		pTk()
	}

	closed := 0
	if len(openPos) > 0 {
		closed, _ = runExits(cfg, cli, store, klineReaderOf(store), openPos, markPrices, cfg.Bar)
	}

	// 权益快照落库：顶栏的「账户权益 / 可用 / 浮盈」就是从这张表读的
	if account != nil {
		_ = store.Ingest(repo.StorePayload{Equity: []repo.EquityRow{{
			Ts: time.Now().UnixMilli(), TotalEq: account.TotalEq,
			Avail: account.AvailEq, Upl: account.Upl, PosCount: account.PosCount,
		}}})
	}
	return closed, account, nil
}
