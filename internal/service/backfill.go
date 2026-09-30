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
	"time"

	"finally-main/internal/model"
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
	FocusAll    bool
	Workers     int // 并发回补协程数，默认 6
	MaxPages    int // 单个 (合约,周期) 最多翻多少页，默认 2000（防跑飞）
	RealtimeSec int // 实时行情落库间隔（秒），默认 5
}

// DefaultBackfillConfig 默认配置
func DefaultBackfillConfig() BackfillConfig {
	return BackfillConfig{
		Days:          30,
		Bars:          append([]string{}, SupportedBars...),
		FocusN:        8,
		OnlyTradeable: true,
		Workers:       6,
		MaxPages:      2000,
		RealtimeSec:   5,
	}
}

// jobKey 任务唯一键
func jobKey(instID, bar string) string { return instID + "|" + bar }

// BackfillManager 回补 + 实时数据管理器
type BackfillManager struct {
	db   *repo.DB
	feed *DataFeed
	cfg  BackfillConfig

	mu      sync.Mutex
	queue   chan [2]string // [instID, bar]
	queued  map[string]bool
	running bool

	progressMu sync.Mutex
	progress   map[string]model.BackfillJob

	stopCh chan struct{}
	wg     sync.WaitGroup

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
	return &BackfillManager{
		db: db, feed: feed, cfg: cfg,
		queue:    make(chan [2]string, 4096),
		queued:   map[string]bool{},
		progress: map[string]model.BackfillJob{},
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

	// 回补协程
	m.mu.Lock()
	m.running = true
	m.mu.Unlock()
	for i := 0; i < m.cfg.Workers; i++ {
		m.wg.Add(1)
		go m.worker()
	}

	// 焦点合约入队
	focus := m.focusList()
	m.logf("回补焦点合约（%d 个）：%s", len(focus), strings.Join(focus, " "))
	for _, inst := range focus {
		for _, bar := range m.cfg.Bars {
			m.Enqueue(inst, bar)
		}
	}

	// 实时落库
	m.wg.Add(1)
	go m.realtimeLoop()

	return nil
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
func (m *BackfillManager) SyncTickers() error {
	list, err := m.feed.FetchTickers()
	if err != nil {
		return err
	}
	// 1) 行情快照落库（480 行，一条多行 upsert）
	if _, err := m.db.UpsertTickers(list); err != nil {
		return err
	}
	// 2) 顺手把 24h 成交额回写到 inst 表，保证下拉框按热度排序是最新的。
	//    480 行单事务更新，十几毫秒，直接同步做掉 —— 异步的话
	//    （比如 -init-only 立即退出）会丢掉这次更新。
	vols := make([]model.Instrument, 0, len(list))
	for _, t := range list {
		vols = append(vols, model.Instrument{InstID: t.InstID, QuoteVol24h: t.QuoteVol24h})
	}
	if err := m.db.UpdateVolumes(vols); err != nil {
		m.logf("WARN", "回写 24h 成交额失败：%v", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 回补
// ---------------------------------------------------------------------------

// Enqueue 排队一个 (合约,周期) 的回补任务。已经在队列里就跳过。
func (m *BackfillManager) Enqueue(instID, bar string) bool {
	if instID == "" || bar == "" || !IsSupportedBar(bar) {
		return false
	}
	k := jobKey(instID, bar)
	m.mu.Lock()
	if m.queued[k] {
		m.mu.Unlock()
		return false
	}
	m.queued[k] = true
	m.mu.Unlock()

	select {
	case m.queue <- [2]string{instID, bar}:
		m.setProgress(instID, bar, "queued", "已排队")
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
			instID, bar := job[0], job[1]
			if err := m.backfillOne(instID, bar); err != nil {
				m.logf("✗ 回补失败 %s %s：%v", instID, bar, err)
			}
			m.mu.Lock()
			delete(m.queued, jobKey(instID, bar))
			m.mu.Unlock()
		}
	}
}

// BackfillOne 同步回补一个 (合约,周期)，供命令行直接调用
func (m *BackfillManager) BackfillOne(instID, bar string) error {
	return m.backfillOne(instID, bar)
}

// backfillOne 单个任务：先补最新 300 根，再一路往前翻到覆盖满 Days 天
func (m *BackfillManager) backfillOne(instID, bar string) error {
	target := time.Now().AddDate(0, 0, -m.cfg.Days)
	targetMs := target.UnixMilli()

	m.setProgress(instID, bar, "running", "开始回补")

	// 1) 最新一批
	latest, err := m.feed.FetchCandles(instID, bar, 300)
	if err != nil {
		m.setProgress(instID, bar, "error", "拉最新 K 线失败："+err.Error())
		return err
	}
	if len(latest) > 0 {
		if _, err := m.db.UpsertKlines(latest); err != nil {
			m.setProgress(instID, bar, "error", "写库失败："+err.Error())
			return err
		}
	}

	total := int64(len(latest))
	oldest := int64(0)
	if len(latest) > 0 {
		oldest = latest[0].Ts
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
		if _, err := m.db.UpsertKlines(cut); err != nil {
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
	m.setProgress(instID, bar, "done",
		fmt.Sprintf("覆盖 %.1f 天 / %d 根（目标 %d 天）", cov.Days, cov.Count, m.cfg.Days))
	m.logf("✓ 回补完成 %s %s：%d 根，覆盖 %.1f 天", instID, bar, cov.Count, cov.Days)
	return nil
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
	m.progress[jobKey(instID, bar)] = j
	m.progressMu.Unlock()
	_ = m.db.SaveJob(j)
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

	// 最新 K 线不用每轮都拉，按周期各自节流
	lastKline := map[string]time.Time{}
	seconds := 0

	for {
		select {
		case <-m.stopCh:
			return
		case <-tick.C:
			seconds += m.cfg.RealtimeSec
			if err := m.SyncTickers(); err != nil {
				m.logf("⚠ 实时行情落库失败：%v", err)
			}

			// 每 3 分钟续一次 K 线（只续「已回补过」的合约，避免炸 OKX）
			if seconds%180 == 0 {
				m.refreshLatestKlines(lastKline)
			}
			// 每小时把库里最老的数据裁一次
			if seconds%(3600*6) == 0 && seconds > 0 {
				if n, err := m.db.CleanupKlines(30000); err == nil && n > 0 {
					m.logf("滚动清理：删除 %d 根过老 K 线", n)
				}
			}
		}
	}
}

// refreshLatestKlines 只给「库里有数据」的 (合约,周期) 续最新一根
func (m *BackfillManager) refreshLatestKlines(last map[string]time.Time) {
	covs, err := m.db.CoverageAll()
	if err != nil {
		return
	}
	for _, c := range covs {
		if c.Count == 0 {
			continue
		}
		k := jobKey(c.InstID, c.Bar)
		minGap := BarDuration(c.Bar) / 3
		if minGap < 20*time.Second {
			minGap = 20 * time.Second
		}
		if t, ok := last[k]; ok && time.Since(t) < minGap {
			continue
		}
		ks, err := m.feed.FetchCandles(c.InstID, c.Bar, 5)
		if err != nil {
			continue
		}
		if len(ks) > 0 {
			_, _ = m.db.UpsertKlines(ks)
		}
		last[k] = time.Now()
	}
}
