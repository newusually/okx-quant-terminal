package service

// backfill.go —— 历史数据回补（默认 10 天）+ 实时数据保存
//
// 三件事：
//   1. 回补：把每个 (合约, 周期) 的 K 线从 OKX 拉到本地 MySQL，默认覆盖 10 天
//      （四个周期 1m/3m/5m/15m 都补，保留窗口由 kline_retain_days 决定）
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

	"finally-main/internal/logx"
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
	//   "plan"      默认。先给「全部 live 合约」各拉一次最新 300 根（Light，
	//               实际会补到 min_candles 那么多，见 MinCandlesFn），
	//               再逐个往前翻满 Days 天；四个周期（1m/3m/5m/15m）一起补，
	//               1m 排最前。保留窗口 10 天 ⇒ 磁盘约 200 MB，图很快可用。
	//   "tradeable" 可交易合约 × 全部周期
	//   "live"      全部 live 合约 × 全部周期（约 480 合约，磁盘自动守卫）
	//   "focus"     只回补 FocusN 个焦点合约（旧行为）
	Scope string

	// MinFreeMB 剩余磁盘低于这个数就暂停回补（默认 800MB）。
	// 数据量算得出来：一行约 107 字节；10 天窗口下 480 合约 × 4 周期
	// ≈ 110 万行 ≈ 120 MB，远低于一期的「15m 留一年」1041 万行。
	MinFreeMB int
	// RootDir 用来查所在卷的剩余空间
	RootDir string

	Workers     int // 并发回补协程数，默认 6
	MaxPages    int // 单个 (合约,周期) 最多翻多少页，默认 2000（防跑飞）
	RealtimeSec int // 实时行情落库间隔（秒），默认 5

	// RefreshTopNFn 返回「续最新一根」只做前 N 个合约（0 / nil = 不限）。
	//
	// ★ 为什么必须截断 ★
	//
	// OKX /market/*-candles 限 20 次 / 2 秒，且**全进程共用一把闸门**
	// （internal/ratelimit）。四周期（1m/3m/5m/15m）下，需要续的
	// (合约,周期) 条数 ≈ 可交易合约数 × 4。169 个可交易合约 = 676 条，
	// 单轮最少 67.6 秒 —— **已经超过 1m K 线的一分钟**。
	//
	// 而扫描读本地的判据是「本地最新一根 ≥ 刚收盘那根」（见 scanner.localCandlesFresh），
	// 于是 1m 永远不达标 → 每轮都回退网络（80 次请求）→ 和续 K 线抢同一把闸门
	// → 续 K 线更慢 → 本地更不新鲜 …… 死循环。
	// 实测这一循环把 eng.scan 顶到 101.7s、live.exitWait（出场巡检等锁）顶到 35.4s。
	//
	// 截到「扫描真正会用的那批」（= 策略 top_n_by_volume，默认 80）后，
	// 一轮只有 320 条 ≈ 32 秒 < 60 秒，1m 本地能真正转热，扫描回落本地，
	// 省下的闸门还给回补 —— 这是打破死循环的唯一办法（加大并发没用，闸门是地板）。
	//
	// 做成回调而不是定值：top_n_by_volume 在策略 JSON 里是热插拔的，
	// 启动时读一次会跟 JSON 脱节。
	RefreshTopNFn func() int

	// MinCandlesFn 返回「扫描算信号至少需要多少根 K 线」（= 策略 min_candles，默认 400）。
	// 回补的 Light 铺底会至少铺这么多根，0 / nil 时退回 300。
	//
	// ★ 为什么 Light 必须铺够 400 而不是 300 ★
	//
	// 扫描读本地有**两个**前提：够新（本地已有刚收盘那根）与**够长**（len ≥ min_candles）。
	// 而单次 /market/candles 的 limit **上限就是 300** ——
	// 只铺 300 根的话，本地永远差 100 根、永远达不到门槛，
	// 于是 1m/3m/5m × 80 个候选 = **240 次网络回退**，回头和回补抢同一把 OKX 闸门。
	// 实测就是这个：整轮 3m48s、eng.scan 101.7s、出场巡检等锁 35.4s。
	//
	// 代价只是 Light 阶段每个 (合约,周期) 多一次 100 根的历史请求
	// （约 3 周期 × 170 合约 = 510 次 ≈ 51 秒闸门，一次性），
	// 换来的是 1m/3m/5m 全部转本地、扫描不再回退网络。
	MinCandlesFn func() int
}

// DefaultBackfillConfig 默认配置
func DefaultBackfillConfig() BackfillConfig {
	return BackfillConfig{
		// 默认回补 10 天，和 K 线保留窗口（kline_retain_days=10）对齐。
		// cmd/okxweb 会用 resolveBackfillDays 再覆写一次（跟随配置真源），
		// 这里的 10 只是「没人传参时」的兜底，避免又出现「拉一年只留十天」。
		Days:          10,
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

	// takerIDs 返回当前要同步 taker 买卖量的合约池（非美股非ETF、24h 成交额前 N）。
	//
	// 做成回调而不是直接把池子存进来：池子依赖 ticker 表（每分钟变），
	// 存快照就会越跑越旧；回调每次现算，口径永远和交易侧一致。
	// 由 cmd 层注入（可复用准入过滤的结果），nil 表示不跑 taker 同步。
	takerIDs func() []string

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

// SetTakerIDs 注入 taker 同步的合约池回调（见字段注释）
func (m *BackfillManager) SetTakerIDs(f func() []string) {
	m.takerIDs = f
}

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

	// taker 买卖量 30 天首铺（★ 2026-10-03 二十二期）
	//
	// ★ 必须放后台 goroutine：80 个合约 × 翻页 + 节流 ≈ 分钟级，
	//   同步跑会挡在 HTTP 监听前面 —— 这正是本项目「启动看起来像失败」
	//   那个老坑（见 PurgeBadKlines 的注释）。
	//
	// ★ 已铺够就跳过：每次重启都重拉 30 天 = 白打 OKX 800 个请求，
	//   还容易吃 429。判据 = taker_scan_state 里已覆盖的合约数
	//   （回补成功才会写水位线，所以它就是「铺过没有」的可靠标记）。
	//   缺了几个补几个：只把没铺过的合约塞进首铺队列。
	if m.takerIDs != nil {
		ids := m.takerIDs()
		if len(ids) > 0 {
			m.wg.Add(1)
			go func() {
				defer m.wg.Done()
				need := make([]string, 0, len(ids))
				for _, id := range ids {
					if _, _, cnt, _, ok := m.db.TakerState(id, "5m"); !ok || cnt == 0 {
						need = append(need, id)
					}
				}
				if len(need) == 0 {
					logx.Logf("INFO", "[TAKER] 30 天数据已就绪（%d 个合约），跳过首铺", len(ids))
					return
				}
				logx.Logf("INFO", "[TAKER] 首铺开始：%d/%d 个合约缺数据", len(need), len(ids))
				t0 := time.Now()
				ok, rows := TakerBackfillInsts(m.db, need, 30, m.cfg.Workers)
				// ★ 服务模式没有控制台，m.logf（fmt.Printf）会被整个丢弃，
				//   留痕必须走 logx（本项目坑 11 的同款）。
				logx.Logf("INFO", "[TAKER] 首铺完成：%d/%d 个合约，%d 行，耗时 %s",
					ok, len(need), rows, time.Since(t0).Round(time.Second))
			}()
		}
	}

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
	//	  几千个请求，几分钟就能让每个合约的每个周期都有近期 K 线。
	//	  历史信号要 200 根暖机，之前 1m/3m/5m 一条信号都没有，就是连
	//	  这段近期的 K 线都还没铺。
	//	第二遍 Full：再逐个往前翻满 Days 天（已铺够的走轻量路径秒过）。
	//
	// ★ 四周期一起回补（2026-10-01 口径变更）★
	//   用户要求「1 分钟 / 3 分钟 / 5 分钟 / 15 分钟选项卡重新生成并且补充数据」，
	//   并且四个周期都参与开仓，所以 1m/3m/5m 从「已下线」恢复为必补。
	//   周期顺序直接沿用 cfg.Bars（= model.EnabledBars = 1m,3m,5m,15m）：
	//   数据量最大的 1m 排最前 —— 它的图最空、最缺，先补先能用。
	//
	//   请求量（保留窗口 10 天）：1m 每合约翻约 10 页、3m 约 5 页、
	//   5m 约 3 页、15m 约 1 页 ⇒ 480 个 live 合约全量约 1.1 万次
	//   history-candles 调用，令牌桶 20 次/2 秒 ⇒ 约 20 分钟跑完；
	//   期间实时行情与下单不受影响。
	// Light 遍：沿用 cfg.Bars 原序。
	//
	// 每个 (合约,周期) 只要 1 次请求，全部任务合起来两三分钟就跑完 ——
	// 顺序无所谓，重点是让每个周期的图先有近期数据（历史信号要 200 根
	// 暖机，没这一段图就是空的、信号一条都出不来）。
	light := appendAll([]BackfillTask{}, all, m.cfg.Bars...)

	// Full 遍：★ 2026-10-02 十九期改为「5m 优先」★ —— 见 preferredBackfillBars。
	full := appendAll([]BackfillTask{}, all, preferredBackfillBars(m.cfg.Bars)...)

	out := make([]BackfillTask, 0, len(light)+len(full))
	for _, t := range light {
		out = append(out, BackfillTask{InstID: t.InstID, Bar: t.Bar, Light: true})
	}
	return append(out, full...)
}

// preferredBackfillBars 把回补周期按**优先级**重排：5m 提到最前，其余保持原序。
//
// ★ 2026-10-02 十九期新增 ★
//
// 起因：用户要「补充所有符合要求的合约的 5m 30 天数据」。
// Full 遍原本按 cfg.Bars 的原序翻页（3m → 5m → 15m），30 天口径下：
//
//	3m  每合约约 144 页   5m 约 87 页   15m 约 29 页
//	约 480 个 live 合约合计 ≈ 12.5 万次 history-candles
//	令牌桶 20 次/2 秒 ⇒ 全部跑完约 3.5 小时
//
// 按原序 5m 要等 3m 全部翻完（约 116 分钟）才轮到 —— 用户最常看的那个
// 周期反而最晚可用。重排后 5m 第一顺位，约 35 分钟就能铺满 30 天。
//
// ⚠ **只用于回补顺序**，绝不能拿去改 model.EnabledBars：
//   那个是「前端选项卡顺序 + 扫描周期」的权威定义
//   （internal/service/feed.go 的 SupportedBars 直接转发它），
//   动它会把网页上的 3m/5m/15m 按钮顺序一起改掉。
func preferredBackfillBars(bars []string) []string {
	const first = "5m"
	out := make([]string, 0, len(bars))
	taken := make([]bool, len(bars))
	for i, b := range bars {
		if strings.EqualFold(strings.TrimSpace(b), first) {
			out = append(out, b)
			taken[i] = true
		}
	}
	for i, b := range bars {
		if !taken[i] {
			out = append(out, b)
		}
	}
	// 万一 cfg.Bars 里没有 5m（例如 -bars 3m）→ 原样返回，不要凭空塞一个
	// 白名单外的周期进去，否则 /api/kline 与信号回算会一起拒绝它。
	if len(out) != len(bars) {
		return bars
	}
	return out
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

// pickRefreshInsts 从「已按成交额降序排好的可交易合约」里取前 n 个，做成集合。
//
// 纯函数、单独抽出来是有意为之：它决定「要不要给某个合约续最新 K 线」，
// 一旦挑错就是「扫到的没续、续了的没扫」的静默退化，必须有单测钉住。
//
// 判据（返回 nil = 不限）：
//   - n <= 0            → nil（不限，退回老行为）
//   - 列表为空          → nil（**宁可不限也不误杀全部**：查库失败时全截断会让所有图都停更）
//   - n >= len(list)    → nil（截了等于没截，省一次 map 分配）
func pickRefreshInsts(ordered []string, n int) map[string]bool {
	if n <= 0 || len(ordered) == 0 || n >= len(ordered) {
		return nil
	}
	out := make(map[string]bool, n)
	for _, id := range ordered[:n] {
		out[id] = true
	}
	return out
}

// refreshScope 返回「值得续最新一根」的合约集合（nil = 不限）。
//
// ★ 口径必须和 scanner 的候选完全一致 ★
//
// 两处都走 inst 表：tradeable=1，按 quote_vol24h DESC 取前 N。
// scanner 是「FilterUniverse 过滤 → 按成交额排序 → 截 TopNByVolume」，
// 而 tradeable 这个标记正是 FilterUniverse 自己写进 inst 的（UpdateTradeable），
// 所以两边挑出来的是同一批。**各写一份口径 = 必然对不上**（本项目已踩过多次）。
func (m *BackfillManager) refreshScope() map[string]bool {
	if m.cfg.RefreshTopNFn == nil {
		return nil
	}
	n := m.cfg.RefreshTopNFn()
	if n <= 0 {
		return nil
	}
	ts, err := m.db.ListTradeableInstruments()
	if err != nil || len(ts) == 0 {
		// 查库失败就不限流：宁可多打几次 OKX，也不能让所有合约的图一起停更
		return nil
	}
	ordered := make([]string, 0, len(ts))
	for _, it := range ts {
		ordered = append(ordered, it.InstID)
	}
	return pickRefreshInsts(ordered, n)
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

	// ★ 第二页：把长度顶过扫描的读本地门槛（min_candles，默认 400）★
	//
	// 单次 /market/candles 的 limit 上限是 300，只铺 300 根本地就永远不够长，
	// 扫描每轮都会回退网络并和回补抢闸门（详见 BackfillConfig.MinCandlesFn 的注释）。
	// 这里补一页历史（100 根），Light 一遍下来 1m/3m/5m 就能直接读本地。
	//
	// 取数口径与扫描的网络路径一致（300 + 翻 1 页 100），
	// 所以本地窗口与网络窗口**等长同内容**，不会算出不同的信号。
	total := len(latest)
	if min := m.lightMin(); min > 300 {
		older, herr := m.feed.FetchHistoryCandles(instID, bar, latest[0].Ts, 100)
		if herr != nil {
			// 翻页失败不当致命：已经铺的那 300 根照用，下次还会再跑
			m.logf("⚠ %s %s 补第二页失败：%v（本次只铺 %d 根）", instID, bar, herr, total)
		} else if trimmed := m.trimToWindow(older); len(trimmed) > 0 {
			if _, err := m.db.UpsertKlines(trimmed); err != nil {
				m.setProgress(instID, bar, "error", "写库失败："+err.Error())
				return err
			}
			total += len(trimmed)
		}
	}
	m.setProgress(instID, bar, "queued", fmt.Sprintf("已铺最新 %d 根", total))
	return nil
}

// lightMin Light 阶段至少要铺多少根（下限 300，上限 1000 防跑飞）。
func (m *BackfillManager) lightMin() int {
	if m.cfg.MinCandlesFn == nil {
		return 300
	}
	v := m.cfg.MinCandlesFn()
	if v <= 300 {
		return 300
	}
	if v > 1000 {
		return 1000
	}
	return v
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
			// taker 买卖量增量（★ 2026-10-03 二十二期）：5m 切片，每 5 分钟
			// 才出一根新数据，所以 60 秒一轮足够，且只拉最近 2 小时。
			if volTick%volEvery == 0 && m.takerIDs != nil {
				ids := m.takerIDs()
				if len(ids) > 0 {
					done := perf.Track("feed.takerSync")
					if ok, rows := TakerSyncLatest(m.db, ids, m.cfg.Workers); rows > 0 {
						logx.Logf("INFO", "[TAKER] 实时增量：%d 个合约，%d 行", ok, rows)
					}
					done()
				}
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
//
// ★ 2026-10-01 串行改并发 ★
//
// 原实现是「串行 for 循环 + 每个合约一次 HTTP 请求」：
//
//	for k, maxTs := range known {
//	    ks, err := m.feed.FetchCandles(instID, bar, 3)   // ← 每次 ~130ms 网络等待
//	}
//
// 实测 `feed.refreshLatest` 平均 44.6 秒、**最大 753.3 秒（12.5 分钟）**。
// 根因：15m K 线收盘的那一瞬间，479 个合约**同时到期**，而循环是一次一条，
// 单线程等价于「479 × 单次网络延迟」；再叠加与回补共用限频闸门
// （ratelimit.Candle() = 20 次/2 秒）时的排队，就滚到了 12 分钟。
//
// 现在拆成两段：
//
//	① 筛「到期任务」—— 纯内存判断，串行，微秒级
//	② 抓最新一根 —— worker pool 并发，写法与 scanner.go 完全一致
//
// ⚠️ 收益的**天花板是限频闸门，不是并发数**：
//
//	闸门 20 次 / 2 秒 = 10 次/秒 → 479 条最少也要 47.9 秒。
//	并发能把「等网络」和「等闸门」重叠起来，但不可能突破这个地板。
//	所以预期是 753s → 50~90s，而不是「几秒」。想再快只能减少请求数
//	（例如只给可交易合约续 K 线），那是产品口径变更，不在这里动。
//
// 并发数取 m.cfg.Workers（默认 6；本机 2 核，别超 8）。
func (m *BackfillManager) refreshLatestKlines() {
	known := m.knownSnapshot()
	if len(known) == 0 {
		return
	}
	now := time.Now().UnixMilli()

	// 磁盘满了就整轮不做（与旧实现一致：旧版在循环里遇到就 return）
	if !m.diskOK() {
		return
	}

	// ---- ① 筛到期任务（纯内存，串行）----
	//
	// allow 是「只续这些合约」（nil = 不限）。截断的理由见 RefreshTopNFn 的注释：
	// 不截，四周期 × 可交易合约数就会顶穿 OKX 的 20 次/2 秒闸门，
	// 1m 永远不新鲜，扫描和续 K 线互相抢闸门形成死循环。
	allow := m.refreshScope()
	type refreshJob struct {
		instID string
		bar    string
	}
	jobs := make([]refreshJob, 0, len(known))
	for k, maxTs := range known {
		select {
		case <-m.stopCh:
			return
		default:
		}
		instID, bar, ok := splitJobKey(k)
		if !ok {
			continue
		}
		if allow != nil && !allow[instID] {
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
		jobs = append(jobs, refreshJob{instID: instID, bar: bar})
	}
	if len(jobs) == 0 {
		return
	}

	// ---- ② 并发抓最新一根（I/O 段）----
	//
	// 限频由 ratelimit.Candle() 在客户端内部统一把关（与回补、扫描共用同一把），
	// 所以这里放心开并发 —— 加 worker 不会多打 OKX，只是把等待重叠掉。
	workers := m.cfg.Workers
	if workers <= 0 {
		workers = 6
	}
	if workers > len(jobs) {
		workers = len(jobs)
	}

	ch := make(chan refreshJob)
	var wg sync.WaitGroup
	var okN, failN, skipN int64

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				ks, err := m.feed.FetchCandles(j.instID, j.bar, 3)
				if err != nil {
					atomic.AddInt64(&failN, 1)
					continue
				}
				if len(ks) == 0 {
					atomic.AddInt64(&skipN, 1)
					continue
				}
				// 与原实现一致：入库失败不影响水位线推进
				_, _ = m.db.UpsertKlines(ks)
				m.markKnown(j.instID, j.bar, ks[len(ks)-1].Ts)
				atomic.AddInt64(&okN, 1)
			}
		}()
	}

	start := time.Now()
	stopped := false
	for _, j := range jobs {
		select {
		case <-m.stopCh:
			stopped = true
		case ch <- j:
		}
		if stopped {
			break
		}
	}
	close(ch)
	wg.Wait()

	// 只在「真干了活」或「出过错」时打日志，避免每 30 秒刷屏
	ok, fail := atomic.LoadInt64(&okN), atomic.LoadInt64(&failN)
	elapsed := time.Since(start)
	if fail > 0 || elapsed > 5*time.Second {
		tail := ""
		if stopped {
			tail = "（收到停止信号，提前收尾）"
		}
		scopeText := "全部"
		if allow != nil {
			scopeText = fmt.Sprintf("Top%d（共 %d 个合约有数据）", len(allow), len(known))
		}
		m.logf("续最新 K 线：范围 %s，到期 %d 条，成功 %d，失败 %d，跳过 %d，并发 %d，耗时 %.1fs%s",
			scopeText, len(jobs), ok, fail, atomic.LoadInt64(&skipN), workers, elapsed.Seconds(), tail)
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
