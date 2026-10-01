package service

// backfill.go —— 历史数据回补（至少一个月）+ 实时数据保存
//
// 三件事：
//   1. 回补：把每个 (合约, 周期) 的 K 线从 OKX 拉到本地 SQLite，默认覆盖 30 天
//   2. 实时：每 N 秒拉一次全市场 tickers 落库，同时把最新一根 K 线续上
//   3. 进度：每个任务的状态都写 backfill_job 表，前端可以直接看

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"finally-main/internal/model"
	"finally-main/internal/perf"
	"finally-main/internal/repo"
)

// BackfillConfig 回补配置
type BackfillConfig struct {
	Days      int      // 每个 (合约,周期) 至少覆盖多少天，默认 30
	Bars      []string // 要回补的周期，默认 SupportedBars
	FocusInst []string // 启动就回补的合约（留空 = 按成交额取 TopFocusN）
	FocusN    int      // FocusInst 留空时，取成交额前 N 名，默认 8
	// OnlyTradeable 只回补「通过准入过滤」的合约（不买美股ETF/新上线/待下线/0.1U 买不起的）
	// 这是默认行为：既省磁盘也省时间，而且回补的就是真正会下单的那批
	OnlyTradeable bool
	// FocusAll 若为 true 则忽略 FocusN，回补全部（可交易）合约
	FocusAll bool

	// Scope 决定启动时回补多少东西：
	//   "plan"      默认。先给「全部 live 合约」补 15m/1H/4H（便宜，图先能用），
	//               再给「可交易合约」补 5m/3m/1m。约 1.4 GB 磁盘，图最快可用。
	//   "tradeable" 可交易合约 × 全部周期
	//   "live"      全部 live 合约 × 全部周期（约 3.6 GB，磁盘不够会自动暂停）
	//   "focus"     只回补 FocusN 个焦点合约（旧行为）
	Scope string

	// MinFreeMB 剩余磁盘低于这个数就暂停回补（默认 800MB）。
	// 数据量算得出来：一行约 107 字节，171 合约 × 6 周期 × 30 天 ≈ 1200 万行 ≈ 1.3 GB。
	MinFreeMB int
	// RootDir 用来查所在卷的剩余空间
	RootDir string

	Workers     int // 并发回补协程数，默认 6
	MaxPages    int // 单个 (合约,周期) 最多翻多少页，默认 2000（防跑飞）
	RealtimeSec int // 实时行情落库间隔（秒），默认 5
}

// DefaultBackfillConfig 默认配置
func DefaultBackfillConfig() BackfillConfig {
	return BackfillConfig{
		// 默认回补 1 年，和 K 线保留窗口（kline_retain_days=365）对齐。
		// 第一次跑要拉 479 合约 × 117 页 ≈ 5.6 万次 history-candles 调用，
		// 全局令牌桶压到 9 次/秒，实测约 2 小时跑完；期间不影响实时行情。
		Days:          365,
		Bars:          append([]string{}, SupportedBars...),
		FocusN:        8,
		OnlyTradeable: true,
		Scope:         "plan",
		MinFreeMB:     800,
		Workers:       10,
		MaxPages:      2000,
		RealtimeSec:   5,
	}
}

// BackfillTask 一个 (合约,周期) 回补任务
//
// Light=true 表示「只把最新一段拉回来」（一次请求）。回补队列第一遍全用 Light，
// 几分钟内就能让每个合约的每个周期都有近期 K 线 —— 历史信号要 200 根暖机，
// 没有这一段 1m/3m/5m 的图就是空的、信号一条都出不来。
type BackfillTask struct {
	InstID string
	Bar    string
	Light  bool
}

// jobKey 任务唯一键
func jobKey(instID, bar string) string { return instID + "|" + bar }

// BackfillManager 回补 + 实时数据管理器
type BackfillManager struct {
	db   *repo.DB
	feed *DataFeed
	cfg  BackfillConfig

	mu      sync.Mutex
	queue   chan BackfillTask // 回补任务队列（Light 与 Full 混排）
	queued  map[string]bool
	running bool

	progressMu sync.Mutex
	progress   map[string]model.BackfillJob

	// 已入库的 (合约,周期) → 最新一根 ts。实时续 K 线时用它做差量，
	// 避免每轮都对千万行的 kline 表做 GROUP BY。
	knownMu sync.Mutex
	knownTs map[string]int64

	stopCh chan struct{}
	wg     sync.WaitGroup

	// lastTickers 最近一轮行情快照。SyncVolumes 跑在 60 秒的慢节奏上，
	// 直接复用这里的数据，不用为了更新成交额再拉一次全市场行情。
	lastTickers atomic.Pointer[[]model.Ticker]

	logf func(string, ...any)
}

// NewBackfillManager 建管理器（不启动）
func NewBackfillManager(db *repo.DB, feed *DataFeed, cfg BackfillConfig, logf func(string, ...any)) *BackfillManager {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if cfg.Days <= 0 {
		cfg.Days = 30
	}
	if len(cfg.Bars) == 0 {
		cfg.Bars = append([]string{}, SupportedBars...)
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 3
	}
	if cfg.MaxPages <= 0 {
		cfg.MaxPages = 2000
	}
	if cfg.RealtimeSec <= 0 {
		cfg.RealtimeSec = 5
	}
	if cfg.Scope == "" {
		cfg.Scope = "plan"
	}
	if cfg.MinFreeMB <= 0 {
		cfg.MinFreeMB = 800
	}
	return &BackfillManager{
		db: db, feed: feed, cfg: cfg,
		queue:    make(chan BackfillTask, 8192),
		queued:   map[string]bool{},
		progress: map[string]model.BackfillJob{},
		knownTs:  map[string]int64{},
		stopCh:   make(chan struct{}),
		logf:     logf,
	}
}

// ---------------------------------------------------------------------------
// 启动
// ---------------------------------------------------------------------------

// Start 启动：合约列表 → 行情快照 → 焦点合约回补 → 实时落库协程
func (m *BackfillManager) Start(ctx context.Context) error {
	m.logf("=" + strings.Repeat("=", 62))
	m.logf("数据服务启动：回补天数=%d 周期=%v 并发=%d", m.cfg.Days, m.cfg.Bars, m.cfg.Workers)

	if ts, err := m.feed.Ping(); err != nil {
		m.logf("⚠ 连不上 OKX：%v", err)
		return err
	} else {
		m.logf("✓ OKX 已连通，服务器时间 %s", time.UnixMilli(ts).Format("2006-01-02 15:04:05"))
	}

	if err := m.SyncInstruments(); err != nil {
		m.logf("⚠ 合约列表同步失败：%v", err)
	} else {
		insts, _ := m.db.ListInstruments()
		m.logf("✓ 合约列表已入库：%d 个 USDT 永续", len(insts))
	}

	if err := m.SyncTickers(); err != nil {
		m.logf("⚠ 行情快照同步失败：%v", err)
	} else {
		m.logf("✓ 行情快照已入库")
	}
	if err := m.SyncVolumes(); err != nil {
		m.logf("⚠ 回写 24h 成交额失败：%v", err)
	}

	// 回补协程
	m.mu.Lock()
	m.running = true
	m.mu.Unlock()
	for i := 0; i < m.cfg.Workers; i++ {
		m.wg.Add(1)
		go m.worker()
	}

	// 入队：按 Scope 决定回补范围
	plan := m.buildPlan()
	m.logf("回补计划（Scope=%s）：%d 个任务，并发 %d，磁盘守卫 %dMB",
		m.cfg.Scope, len(plan), m.cfg.Workers, m.cfg.MinFreeMB)
	for _, t := range plan {
		m.enqueueTask(t)
	}
	if free := FreeDiskMB(m.cfg.RootDir); free > 0 {
		m.logf("当前剩余磁盘 %d MB", free)
	}

	// 实时落库
	m.wg.Add(1)
	go m.realtimeLoop()

	return nil
}

// ---------------------------------------------------------------------------
// 回补计划
// ---------------------------------------------------------------------------

// buildPlan 生成启动时的回补队列。
//
// 排序原则：**用户最缺的先跑**。
//
//	1m/3m/5m 的历史信号要 200 根以上 K 线才算得出来，之前队列把这三个周期
//	排在最后（15m/1H/4H 铺完全部合约才轮到），结果图一切到 1m/3m/5m 就是
//	空的、信号也没有。现在把小周期提到最前。
//
//	liveIDs() 已经按 tradeable DESC, quote_vol24h DESC 排过序 —— 能下单的
//	热门合约天然排在前面，所以「先补的」就是用户最常看的。
//
//	plan（默认）：全部 live 合约 × 5m/3m/1m  →  全部 live 合约 × 15m/1H/4H
//	tradeable  ：可交易合约 × 全部 6 个周期
//	live       ：全部 live 合约 × 全部 6 个周期
//	focus      ：只回补 FocusN 个焦点合约（旧行为）
func (m *BackfillManager) buildPlan() []BackfillTask {
	all := m.liveIDs()
	tradable := m.tradeableIDs()

	appendAll := func(dst []BackfillTask, ids []string, bars ...string) []BackfillTask {
		for _, bar := range bars {
			for _, id := range ids {
				dst = append(dst, BackfillTask{InstID: id, Bar: bar})
			}
		}
		return dst
	}

	switch m.cfg.Scope {
	case "focus":
		out := []BackfillTask{}
		for _, inst := range m.focusList() {
			out = appendAll(out, []string{inst}, m.cfg.Bars...)
		}
		return out

	case "tradeable":
		if len(tradable) == 0 {
			tradable = all
		}
		return appendAll([]BackfillTask{}, tradable, m.cfg.Bars...)

	case "live":
		return appendAll([]BackfillTask{}, all, m.cfg.Bars...)
	}

	// 默认 plan：两遍走。
	//
	//	第一遍 Light：每个 (合约,周期) 只拉最新 300 根 —— 全部任务合起来
	//	  不到三千个请求，几分钟就能让每个合约的每个周期都有近期 K 线。
	//	  历史信号要 200 根暖机，之前 1m/3m/5m 一条信号都没有，就是连
	//	  这段近期的 K 线都还没铺。
	//	第二遍 Full：再逐个往前翻满 Days 天（已铺够的走轻量路径秒过）。
	//
	// 周期顺序：5m 排最前 —— 它是唯一还需要 30 天历史的短周期。
	//
	// ★ 全库只回补 15m（2026-10-01 用户口径）：
	//   「把 4H / 1H / 5m 全部删除，只保留 15 分钟的信号和买卖点」。
	//   更早砍掉的 1m/3m 占过全表 69% 的行，且 1m 全量回补要二十多万次请求
	//   （五六个小时），把要用的周期全堵在后面。
	pick := func(dst []BackfillTask) []BackfillTask {
		dst = appendAll(dst, all, "15m")
		return dst
	}

	full := pick([]BackfillTask{})
	out := make([]BackfillTask, 0, len(full)*2)
	for _, t := range full {
		out = append(out, BackfillTask{InstID: t.InstID, Bar: t.Bar, Light: true})
	}
	return append(out, full...)
}

// liveIDs 全部 live 状态的合约
func (m *BackfillManager) liveIDs() []string {
	insts, err := m.db.ListInstruments()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(insts))
	for _, it := range insts {
		if it.State != "" && it.State != "live" {
			continue
		}
		out = append(out, it.InstID)
	}
	return out
}

// tradeableIDs 通过准入过滤的合约
func (m *BackfillManager) tradeableIDs() []string {
	ts, err := m.db.ListTradeableInstruments()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(ts))
	for _, it := range ts {
		out = append(out, it.InstID)
	}
	return out
}

// diskOK 剩余磁盘是否够用（守卫，防止把系统盘写爆）
func (m *BackfillManager) diskOK() bool {
	if m.cfg.MinFreeMB <= 0 {
		return true
	}
	free := FreeDiskMB(m.cfg.RootDir)
	if free == 0 {
		return true // 查不到就不拦
	}
	return free >= uint64(m.cfg.MinFreeMB)
}

// Stop 停止所有后台协程
func (m *BackfillManager) Stop() {
	m.mu.Lock()
	if !m.running {
		m.mu.Unlock()
		return
	}
	m.running = false
	m.mu.Unlock()
	close(m.stopCh)
	m.wg.Wait()
}

// focusList 决定启动时回补哪些合约
func (m *BackfillManager) focusList() []string {
	if len(m.cfg.FocusInst) > 0 {
		return m.cfg.FocusInst
	}
	// 优先只回补「可交易」合约（准入过滤已排除美股ETF/新上线/待下线/0.1U 买不起的）
	insts, err := m.db.ListInstruments()
	if m.cfg.OnlyTradeable {
		if ts, e := m.db.ListTradeableInstruments(); e == nil && len(ts) > 0 {
			insts, err = ts, nil
		}
	}
	if err != nil || len(insts) == 0 {
		// 兜底：没库就写死几个主流
		return []string{"BTC-USDT-SWAP", "ETH-USDT-SWAP", "SOL-USDT-SWAP", "DOGE-USDT-SWAP"}
	}
	type pair struct {
		id  string
		vol float64
	}
	ps := make([]pair, 0, len(insts))
	for _, it := range insts {
		if it.State != "" && it.State != "live" {
			continue
		}
		ps = append(ps, pair{it.InstID, it.QuoteVol24h})
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].vol > ps[j].vol })
	n := m.cfg.FocusN
	if m.cfg.FocusAll {
		n = len(ps) // 全量回补
	}
	if n <= 0 {
		n = 8
	}
	if n > len(ps) {
		n = len(ps)
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, ps[i].id)
	}
	return out
}

// ---------------------------------------------------------------------------
// 同步合约 / 行情
// ---------------------------------------------------------------------------

// SyncInstruments 拉全量合约写入 inst 表
func (m *BackfillManager) SyncInstruments() error {
	list, err := m.feed.FetchInstruments()
	if err != nil {
		return err
	}
	_, err = m.db.UpsertInstruments(list)
	return err
}

// SyncTickers 拉全市场行情写入 ticker 表
//
// ★ 2026-10-01 拆分：成交额回写（UpdateVolumes）从每轮 5 秒放宽到每 60 秒。
//   原因见 realtimeLoop：inst 上两个二级索引 + 480 行更新，实测平均 3.1 秒一轮，
//   而它只服务于「按热度排序」和「成交额 <100 万不买」这两件事，不需要 5 秒新鲜度。
func (m *BackfillManager) SyncTickers() error {
	list, err := m.feed.FetchTickers()
	if err != nil {
		return err
	}
	// 行情快照落库（480 行，一条多行 upsert）
	if _, err := m.db.UpsertTickers(list); err != nil {
		return err
	}
	// 缓存一份给 SyncVolumes 用，省掉 60 秒那一轮重复拉行情
	m.lastTickers.Store(&list)
	return nil
}

// SyncVolumes 把最近一次行情里的 24h 成交额回写到 inst 表
//
// 单独成函数是为了让它跑在更慢的节奏上（60 秒），逻辑上属于「合约静态信息刷新」
// 而不是「实时行情」。取不到上一轮行情就自己拉一次，保证启动路径也能用。
func (m *BackfillManager) SyncVolumes() error {
	var list []model.Ticker
	if p := m.lastTickers.Load(); p != nil {
		list = *p
	}
	if list == nil {
		var err error
		list, err = m.feed.FetchTickers()
		if err != nil {
			return err
		}
	}
	vols := make([]model.Instrument, 0, len(list))
	for _, t := range list {
		vols = append(vols, model.Instrument{InstID: t.InstID, QuoteVol24h: t.QuoteVol24h})
	}
	return m.db.UpdateVolumes(vols)
}

// ---------------------------------------------------------------------------
// 回补
// ---------------------------------------------------------------------------

// Enqueue 排队一个 (合约,周期) 的回补任务。已经在队列里就跳过。
func (m *BackfillManager) Enqueue(instID, bar string) bool {
	return m.enqueueTask(BackfillTask{InstID: instID, Bar: bar})
}

// enqueueTask 排队一个任务（带 Light 标记）。同 key 在队列里就跳过。
func (m *BackfillManager) enqueueTask(t BackfillTask) bool {
	if t.InstID == "" || t.Bar == "" || !IsSupportedBar(t.Bar) {
		return false
	}
	k := jobKey(t.InstID, t.Bar)
	if t.Light {
		k += "|light"
	}
	m.mu.Lock()
	if m.queued[k] {
		m.mu.Unlock()
		return false
	}
	m.queued[k] = true
	m.mu.Unlock()

	select {
	case m.queue <- t:
		// 入队阶段只记内存：几千个任务逐个写库会把启动拖死
		m.setProgressMem(t.InstID, t.Bar, "queued", "已排队")
		return true
	default:
		m.mu.Lock()
		delete(m.queued, k)
		m.mu.Unlock()
		return false
	}
}

func (m *BackfillManager) worker() {
	defer m.wg.Done()
	for {
		select {
		case <-m.stopCh:
			return
		case job := <-m.queue:
			var err error
			if job.Light {
				err = m.backfillLight(job.InstID, job.Bar)
			} else {
				err = m.backfillOne(job.InstID, job.Bar)
			}
			if err != nil {
				m.logf("✗ 回补失败 %s %s：%v", job.InstID, job.Bar, err)
			}
			k := jobKey(job.InstID, job.Bar)
			if job.Light {
				k += "|light"
			}
			m.mu.Lock()
			delete(m.queued, k)
			m.mu.Unlock()
		}
	}
}

// BackfillOne 同步回补一个 (合约,周期)，供命令行直接调用
func (m *BackfillManager) BackfillOne(instID, bar string) error {
	return m.backfillOne(instID, bar)
}

// trimToWindow 丢掉早于 K 线保留窗口的 K 线。
//
// ★ 为什么必须裁 ★
// OKX 的 history-candles / candles 单次最多只给 **300 根**，而 300 根对不同
// 周期是完全不同的时间跨度：
//
//	15m 300 根 = 3.1 天   ← 现在只有这一个周期
//	（历史教训）1H 300 根 = 12.5 天，4H 300 根 = 50 天
//
// 原来把 300 根原样写库，4H 每个合约就凭空多存 20 天，
// 然后 CleanupKlines 每轮再把它裁掉 —— 实测每次删 31,181 行、耗时 99 秒，
// 而且删完几分钟又被下一轮回补写回来，形成「写进去 → 删掉 → 再写进去」
// 的永久 churn，白烧 CPU 和磁盘（这台机器只有 2 核 2G）。
//
// ★ 窗口取谁：repo.KlineRetainDays()（默认 365 天），不是 cfg.Days ★
//
//	cfg.Days（-days 启动参数）  = 「回补往回拉多远」
//	KlineRetainDays()           = 「库里留多久」
//
// 两者以前是同一个值所以看不出来；现在保留期是 1 年，而回补可能分阶段
// 放开（比如先只拉 3 个月让图先能用），再混用就会把「还没拉到的历史」
// 当成「超窗口的垃圾」丢掉 —— 结果永远补不满一年。
// 写入端只认保留窗口，回补端自己控制拉多远。
func (m *BackfillManager) trimToWindow(rows []model.Kline) []model.Kline {
	if len(rows) == 0 {
		return rows
	}
	days := repo.KlineRetainDays()
	if days <= 0 {
		return rows
	}
	cutoff := time.Now().AddDate(0, 0, -days).UnixMilli()
	keep := 0
	for _, k := range rows {
		if k.Ts >= cutoff {
			keep++
		}
	}
	if keep == len(rows) {
		return rows // 全在窗口内，原样返回（不复制）
	}
	out := make([]model.Kline, 0, keep)
	for _, k := range rows {
		if k.Ts >= cutoff {
			out = append(out, k)
		}
	}
	return out
}

// backfillLight 只把最新一段拉回来（一次请求、300 根），不翻历史页。
//
// 队列第一遍用它：几分钟内让全部合约 × 全部周期都有近期 K 线，
// 历史信号（要 200 根暖机）立刻算得出来，不用等整轮翻页跑完。
func (m *BackfillManager) backfillLight(instID, bar string) error {
	latest, err := m.feed.FetchCandles(instID, bar, 300)
	if err != nil {
		m.setProgress(instID, bar, "error", "拉最新 K 线失败："+err.Error())
		return err
	}
	latest = m.trimToWindow(latest)
	if len(latest) == 0 {
		return nil
	}
	if _, err := m.db.UpsertKlines(latest); err != nil {
		m.setProgress(instID, bar, "error", "写库失败："+err.Error())
		return err
	}
	m.markKnown(instID, bar, latest[len(latest)-1].Ts)
	m.setProgress(instID, bar, "queued", fmt.Sprintf("已铺最新 %d 根", len(latest)))
	return nil
}

// backfillOne 单个任务：先补最新 300 根，再一路往前翻到覆盖满 Days 天
//
// 已经覆盖够天数的一律走「轻量收尾」：只把最新一段拉回来（保证图不滞后），
// 不再翻历史页。回补队列重启后是整队重放的，没有这个判断就要把一个月的数据
// 全部重拉一遍 —— 光 15m 那 442 个已完成的合约就要白跑二十多分钟。
func (m *BackfillManager) backfillOne(instID, bar string) error {
	target := time.Now().AddDate(0, 0, -m.cfg.Days)
	targetMs := target.UnixMilli()

	if cov, cerr := m.db.Coverage(instID, bar); cerr == nil &&
		cov.Count > 0 && cov.Days >= float64(m.cfg.Days)-0.5 {
		if latest, err := m.feed.FetchCandles(instID, bar, 100); err == nil && len(latest) > 0 {
			if _, uerr := m.db.UpsertKlines(m.trimToWindow(latest)); uerr != nil {
				return uerr
			}
			m.markKnown(instID, bar, latest[len(latest)-1].Ts)
		}
		m.setProgress(instID, bar, "done", fmt.Sprintf("已覆盖 %.1f 天", cov.Days))
		return nil
	}

	m.setProgress(instID, bar, "running", "开始回补")

	// 1) 最新一批（裁到窗口内再写，4H 的 300 根 = 50 天，不裁会和白名单打架）
	latest, err := m.feed.FetchCandles(instID, bar, 300)
	if err != nil {
		m.setProgress(instID, bar, "error", "拉最新 K 线失败："+err.Error())
		return err
	}
	if len(latest) > 0 {
		if _, err := m.db.UpsertKlines(m.trimToWindow(latest)); err != nil {
			m.setProgress(instID, bar, "error", "写库失败："+err.Error())
			return err
		}
	}

	total := int64(len(latest))
	oldest := int64(0)
	if len(latest) > 0 {
		oldest = latest[0].Ts
		m.markKnown(instID, bar, latest[len(latest)-1].Ts)
	}

	// 2) 往前翻页
	pages := 0
	for pages < m.cfg.MaxPages {
		select {
		case <-m.stopCh:
			m.setProgress(instID, bar, "stopped", "收到停止信号")
			return nil
		default:
		}
		// 磁盘守卫：宁可停下来，也不能把系统盘写爆导致 MySQL 起不来
		if !m.diskOK() {
			msg := fmt.Sprintf("剩余磁盘不足 %dMB，已暂停（已入库 %d 根）", m.cfg.MinFreeMB, total)
			m.setProgress(instID, bar, "paused", msg)
			m.logf("⏸ %s", msg)
			return nil
		}
		if oldest > 0 && oldest <= targetMs {
			break
		}
		if oldest == 0 {
			break
		}
		older, herr := m.feed.FetchHistoryCandles(instID, bar, oldest, 100)
		if herr != nil {
			// 翻历史失败不当致命：已经拿到的部分照用
			m.setProgress(instID, bar, "partial",
				fmt.Sprintf("翻到第 %d 页失败：%v（已入库 %d 根）", pages+1, herr, total))
			m.logf("⚠ %s %s 翻历史中断：%v", instID, bar, herr)
			return nil
		}
		if len(older) == 0 {
			break
		}
		// 只保留比当前最老还老的，防死循环
		cut := older[:0]
		for _, k := range older {
			if k.Ts < oldest {
				cut = append(cut, k)
			}
		}
		if len(cut) == 0 {
			break
		}
		if _, err := m.db.UpsertKlines(m.trimToWindow(cut)); err != nil {
			m.setProgress(instID, bar, "error", "写库失败："+err.Error())
			return err
		}
		total += int64(len(cut))
		oldest = cut[0].Ts
		pages++
		if pages%20 == 0 {
			m.setProgress(instID, bar, "running",
				fmt.Sprintf("已翻 %d 页，最老 %s，累计 %d 根",
					pages, time.UnixMilli(oldest).Format("01-02 15:04"), total))
		}
	}

	cov, _ := m.db.Coverage(instID, bar)
	m.markKnown(instID, bar, cov.MaxTs)
	m.setProgress(instID, bar, "done",
		fmt.Sprintf("覆盖 %.1f 天 / %d 根（目标 %d 天）", cov.Days, cov.Count, m.cfg.Days))
	m.logf("✓ 回补完成 %s %s：%d 根，覆盖 %.1f 天", instID, bar, cov.Count, cov.Days)
	return nil
}

// ---------------------------------------------------------------------------
// 已入库集合（实时续 K 线时用，避免每次都全表 GROUP BY）
// ---------------------------------------------------------------------------

// markKnown 记下某个 (合约,周期) 最近入库到哪一根
func (m *BackfillManager) markKnown(instID, bar string, maxTs int64) {
	m.knownMu.Lock()
	k := jobKey(instID, bar)
	prev := m.knownTs[k]
	if maxTs > prev {
		m.knownTs[k] = maxTs
	}
	m.knownMu.Unlock()
}

// knownSnapshot 当前「有数据」的 (合约,周期) 快照
func (m *BackfillManager) knownSnapshot() map[string]int64 {
	m.knownMu.Lock()
	defer m.knownMu.Unlock()
	out := make(map[string]int64, len(m.knownTs))
	for k, v := range m.knownTs {
		out[k] = v
	}
	return out
}

// setProgress 更新并持久化进度
func (m *BackfillManager) setProgress(instID, bar, status, msg string) {
	j := model.BackfillJob{InstID: instID, Bar: bar, Status: status, Msg: msg, UpdatedAt: time.Now().UnixMilli()}
	if cov, err := m.db.Coverage(instID, bar); err == nil {
		j.Rows = cov.Count
		j.FromTs = cov.MinTs
		j.ToTs = cov.MaxTs
	}
	m.progressMu.Lock()
	prev, had := m.progress[jobKey(instID, bar)]
	m.progress[jobKey(instID, bar)] = j
	m.progressMu.Unlock()

	// 节流落库：同一个任务「状态没变 + 距上次写库不到 30 秒」就直接丢掉。
	//
	// 前端读进度走的是内存里的 Progress()，不看 backfill_job 表，所以
	// 少写几次库对界面毫无影响；但启动时几千个任务挨个写库会把服务
	// 启动拖成好几分钟（每次 INSERT ... ON DUPLICATE 都是一次磁盘事务）。
	if had && prev.Status == status && j.UpdatedAt-prev.UpdatedAt < 30000 {
		return
	}
	_ = m.db.SaveJob(j)
}

// setProgressMem 只写内存，完全不碰数据库。
//
// 给「排队中」这种一次性、高频、且每条都要标一遍的瞬时状态用：
// 启动时几千个任务如果每个都读一次 Coverage + 写一次 backfill_job，
// 光是入队就要几分钟 —— 表现就是「服务启动卡死、网页半天打不开」。
func (m *BackfillManager) setProgressMem(instID, bar, status, msg string) {
	m.progressMu.Lock()
	m.progress[jobKey(instID, bar)] = model.BackfillJob{
		InstID: instID, Bar: bar, Status: status, Msg: msg,
		UpdatedAt: time.Now().UnixMilli(),
	}
	m.progressMu.Unlock()
}

// Progress 当前所有任务进度
func (m *BackfillManager) Progress() map[string]model.BackfillJob {
	m.progressMu.Lock()
	defer m.progressMu.Unlock()
	out := make(map[string]model.BackfillJob, len(m.progress))
	for k, v := range m.progress {
		out[k] = v
	}
	return out
}

// Feed 暴露行情接入（准入过滤需要用它抓公告）
func (m *BackfillManager) Feed() *DataFeed { return m.feed }

// QueueLen 队列长度（前端显示用）
func (m *BackfillManager) QueueLen() int { return len(m.queue) }

// Config 只读快照（接口层要拿回补天数这类参数时不碰私有字段）
func (m *BackfillManager) Config() BackfillConfig { return m.cfg }

// ---------------------------------------------------------------------------
// 实时
// ---------------------------------------------------------------------------

// realtimeLoop 定时拉行情落库 + 续最新 K 线
func (m *BackfillManager) realtimeLoop() {
	defer m.wg.Done()
	tick := time.NewTicker(time.Duration(m.cfg.RealtimeSec) * time.Second)
	defer tick.Stop()

	seconds := 0
	// volEvery 把「每 60 秒回写成交额」换算成「每多少轮」，
	// 用轮数而不是 seconds%60 判断：RealtimeSec 未必整除 60，
	// 用余数判断会出现「永远不触发」的静默 bug。
	volEvery := 60 / m.cfg.RealtimeSec
	if volEvery < 1 {
		volEvery = 1
	}
	volTick := 0

	for {
		select {
		case <-m.stopCh:
			return
		case <-tick.C:
			seconds += m.cfg.RealtimeSec
			volTick++
			perf.Count1("feed.realtime.tick")
			{
				done := perf.Track("feed.syncTickers")
				if err := m.SyncTickers(); err != nil {
					m.logf("⚠ 实时行情落库失败：%v", err)
				}
				done()
			}

			// 每 60 秒回写一次 24h 成交额。
			//
			// ★ 为什么从 5 秒改成 60 秒：inst 上有 ix_inst_tradeable_vol /
			//   ix_inst_category_vol 两个二级索引，480 行更新要维护近千条索引项；
			//   实测它把 SyncTickers 拖到平均 3.1 秒一轮，而这项数据只用于
			//   「按热度排序」和「成交额 <100 万不买」，一分钟一次完全够。
			if volTick%volEvery == 0 {
				done := perf.Track("feed.syncVolumes")
				if err := m.SyncVolumes(); err != nil {
					m.logf("⚠ 回写 24h 成交额失败：%v", err)
				}
				done()
			}
			// 每 30 秒扫一遍「已经该出新一根」的 (合约,周期)，内部按周期节流，
			// 所以 15m/1H/4H 这些并不会真的每 30 秒发一次请求。
			if seconds%30 == 0 {
				done := perf.Track("feed.refreshLatest")
				m.refreshLatestKlines()
				done()
			}
			// K 线滚动裁剪**不在这里**做了：CleanupKlines 要 30~100 秒
			// （479 合约逐个在分区表上定位第 N 根再 DELETE），
			// 挂在回补循环上会把实时落库和行情刷新一起拖住。
			// 现在统一由 service.StartMaintenance 的年度任务负责
			// （DROP PARTITION 整段扔掉超 365 天的分区）；回补侧也在写入前
			// trimToWindow，两边不会再打架。
		}
	}
}

// refreshLatestKlines 给「库里有数据」的 (合约,周期) 续最新一根
//
// 老实现每轮都调 CoverageAll()（对 kline 全表按合约 GROUP BY）。表一旦长到
// 千万行，这个查询要几十秒，而且每 3 分钟跑一次 —— 会把数据库拖垮。
// 现在改成走内存里的 knownTs 快照：谁有数据、最新到哪根，写入时就记下来了。
func (m *BackfillManager) refreshLatestKlines() {
	known := m.knownSnapshot()
	if len(known) == 0 {
		return
	}
	now := time.Now().UnixMilli()
	for k, maxTs := range known {
		select {
		case <-m.stopCh:
			return
		default:
		}
		if !m.diskOK() {
			return
		}
		instID, bar, ok := splitJobKey(k)
		if !ok {
			continue
		}
		// 最新一根还没走完（离下根开盘还早）就跳过，省一次请求
		d := BarDuration(bar)
		if d <= 0 {
			continue
		}
		if now-maxTs < int64(d/time.Millisecond)-3000 {
			continue
		}
		ks, err := m.feed.FetchCandles(instID, bar, 3)
		if err != nil {
			continue
		}
		if len(ks) > 0 {
			_, _ = m.db.UpsertKlines(ks)
			m.markKnown(instID, bar, ks[len(ks)-1].Ts)
		}
	}
}

// splitJobKey 反解 "INST|bar"
func splitJobKey(k string) (instID, bar string, ok bool) {
	i := strings.LastIndex(k, "|")
	if i <= 0 || i == len(k)-1 {
		return "", "", false
	}
	return k[:i], k[i+1:], true
}
