package repo

// coverage.go —— K 线「覆盖情况」的缓存层（请求路径永不发起全表聚合）
//
// ---------------------------------------------------------------------------
// 事故复盘（2026-10-01 02:25，cpu 打满）
// ---------------------------------------------------------------------------
// 现象：typeperf 采样显示 mysqld#1 ≈ 98~119%（单核）、_Total ≈ 198%
//       （2 核机器接近满负载），而 okxweb 只占 22~35%。
//       MySQL processlist 里同时挂着 3 条一模一样的：
//         SELECT bar, COUNT(*), MIN(ts), MAX(ts) FROM kline
//          WHERE inst_id='SOL-USDT-SWAP' GROUP BY bar ORDER BY bar
//
// 根因链：
//   1. 前端「回补进度」页每 2 秒轮询 /api/backfill
//   2. handleBackfill → CoverageAll()，它对**每个合约各发一条** GROUP BY
//   3. kline 按 ts 做了 RANGE 分区后，`WHERE inst_id=?` 无法做分区裁剪，
//      于是每条 SQL 要扫 20 个分区；476 个合约 × 20 = 近万次分区索引扫描
//   4. 单次请求 10~20 秒 → 2 秒轮询必然堆积 → 3 条并发 → 一颗核跑满
//
// 修复原则（和 rowcount.go 同一套）：
//   · 覆盖情况是「展示用」的，晚 10 分钟完全可接受
//   · 改成**一条**聚合 SQL：SELECT inst_id,bar,COUNT(*),MIN(ts),MAX(ts)
//     FROM kline GROUP BY inst_id,bar —— 一次全索引扫描（实测 6.5 秒热态），
//     对比原来的「476 次分区扫描」是一个数量级的下降
//   · 结果进进程内缓存，TTL 10 分钟，启动后异步预热
//   · 刷新期间并发请求直接吃旧值（stale-while-revalidate），绝不排队
//   · 单飞（busy 标记）：同一时刻只有一条聚合在跑，杜绝并发堆积
//
// 算一下账：6.5 秒 / 600 秒 ≈ 占单核 1.1%。
// 原来长期 100% → 现在 1%，这就是「CPU 降一个数量级」的主要来源。
//
// 记住：这一条同样是 SQL 的问题，换成 C++/PHP 写完全不会变快。

import (
	"database/sql"
	"sync"
	"sync/atomic"
	"time"

	"finally-main/internal/logx"
)

const (
	// CoverageTTL 全量覆盖情况的刷新间隔
	CoverageTTL = 10 * time.Minute
	// coverageOneTTL 单个 (合约,周期) 覆盖情况的缓存时长。
	// /api/kline 每次都读它，20 秒够新，又能把同一张图连续翻页的重复查询吃掉。
	coverageOneTTL = 20 * time.Second
	// coverageOneMax 单个缓存的条目上限，超过就整体清空（防长时间运行内存缓慢膨胀）。
	// 合约数 × 周期数约 476×6 = 2856，8192 是留了三倍余量。
	coverageOneMax = 8192
)

// ---------------------------------------------------------------------------
// 全量覆盖缓存
// ---------------------------------------------------------------------------

var covCache struct {
	mu   sync.RWMutex
	list []KlineCoverage
	at   time.Time
	busy atomic.Bool
}

// CoverageAllCached 取全量覆盖情况。
//
// 只读内存，**永不阻塞**：冷启动时返回空列表，后台补上；
// 缓存过期时先返回旧值，同时踢一脚后台刷新。
func (d *DB) CoverageAllCached() []KlineCoverage {
	covCache.mu.RLock()
	list, at := covCache.list, covCache.at
	covCache.mu.RUnlock()

	if list != nil && time.Since(at) < CoverageTTL {
		return list
	}
	// 过期或没数据：踢一脚后台刷新（单飞），自己立刻返回手里这份
	if covCache.busy.CompareAndSwap(false, true) {
		go func() {
			defer covCache.busy.Store(false)
			if err := d.scanCoverage(); err != nil {
				logx.Logf("WARN", "[CACHE] 刷新覆盖情况失败：%v", err)
			}
		}()
	}
	if list != nil {
		return list
	}
	return nil
}

// CoverageAge 缓存年龄（0 表示还没扫过），排查用
func CoverageAge() time.Duration {
	covCache.mu.RLock()
	at := covCache.at
	covCache.mu.RUnlock()
	if at.IsZero() {
		return 0
	}
	return time.Since(at)
}

// scanCoverage 真正跑一次全表聚合（只允许后台 goroutine 调用）。
//
// 一条 SQL 取代原来的 476 条：PK 是 (inst_id,bar,ts)，这个 GROUP BY
// 正好是「松散顺序」的全索引扫描，不需要回表。
func (d *DB) scanCoverage() error {
	rows, err := d.sql.Query(
		`SELECT inst_id, bar, COUNT(*), MIN(ts), MAX(ts) FROM kline
		 GROUP BY inst_id, bar`)
	if err != nil {
		return err
	}
	defer rows.Close()

	out := make([]KlineCoverage, 0, 3072)
	for rows.Next() {
		var c KlineCoverage
		var minTs, maxTs sql.NullInt64
		if err := rows.Scan(&c.InstID, &c.Bar, &c.Count, &minTs, &maxTs); err != nil {
			return err
		}
		c.MinTs, c.MaxTs = minTs.Int64, maxTs.Int64
		if c.Count > 1 && c.MaxTs > c.MinTs {
			c.Days = float64(c.MaxTs-c.MinTs) / 86400000.0
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	covCache.mu.Lock()
	covCache.list = out
	covCache.at = time.Now()
	covCache.mu.Unlock()
	return nil
}

// StartCoverageRefresher 启动全量覆盖情况的后台刷新。
//
// 首次延迟 12 秒：避开启停时的写入高峰（回补队列刚拉起、ticker 首轮落库），
// 也让 HTTP 端口和首次请求先跑完 —— 和 rowcount 预热同一套节奏。
func (d *DB) StartCoverageRefresher() {
	go func() {
		time.Sleep(12 * time.Second)
		d.refreshCoverage()
		tk := time.NewTicker(CoverageTTL)
		defer tk.Stop()
		for range tk.C {
			d.refreshCoverage()
		}
	}()
}

// refreshCoverage 自己抢单飞标记后执行一次扫描（后台定时器用）
func (d *DB) refreshCoverage() {
	if !covCache.busy.CompareAndSwap(false, true) {
		return
	}
	defer covCache.busy.Store(false)
	if err := d.scanCoverage(); err != nil {
		logx.Logf("WARN", "[CACHE] 刷新覆盖情况失败：%v", err)
		return
	}
	covCache.mu.RLock()
	n := len(covCache.list)
	covCache.mu.RUnlock()
	logx.Logf("INFO", "[CACHE] 覆盖情况已刷新：%d 组（每 %s 一次）", n, CoverageTTL)
}

// ---------------------------------------------------------------------------
// 单个 (合约,周期) 覆盖缓存
// ---------------------------------------------------------------------------

// covEntry 一条缓存记录
type covEntry struct {
	v    KlineCoverage
	at   time.Time
	busy bool // 正在后台刷新，防并发重复查
}

var covOne struct {
	mu sync.Mutex
	m  map[string]*covEntry
}

// CoverageCached 单个 (合约,周期) 的覆盖情况（带 20 秒 SWR 缓存）。
//
// 为什么这个也要缓存：/api/kline 每次请求都读它（图上的「覆盖 N 天 / N 根」
// 脚标 + 是否需要触发自动回补），前端轮询时同一对 (inst,bar) 会反复命中。
// 命中缓存时零 DB 往返；过期时返回旧值并后台刷新，请求线程不等。
//
// 首次（缓存里还没有）才同步查一次 —— 那是毫秒级的 PK 前缀扫描，
// 而且必须拿到真值，否则「库里没数据」会被误判成「需要回补」。
func (d *DB) CoverageCached(instID, bar string) KlineCoverage {
	if instID == "" || bar == "" {
		return KlineCoverage{InstID: instID, Bar: bar}
	}
	k := instID + "|" + bar

	covOne.mu.Lock()
	if covOne.m == nil {
		covOne.m = make(map[string]*covEntry, 256)
	}
	if e := covOne.m[k]; e != nil {
		if time.Since(e.at) < coverageOneTTL {
			v := e.v
			covOne.mu.Unlock()
			return v
		}
		if !e.busy {
			e.busy = true
			go func() {
				c, err := d.Coverage(instID, bar)
				covOne.mu.Lock()
				if err == nil {
					e.v, e.at = c, time.Now()
				}
				e.busy = false
				covOne.mu.Unlock()
			}()
		}
		v := e.v
		covOne.mu.Unlock()
		return v
	}
	covOne.mu.Unlock()

	// 首次：同步查一次
	c, err := d.Coverage(instID, bar)
	if err != nil {
		return c
	}
	covOne.mu.Lock()
	if len(covOne.m) >= coverageOneMax {
		covOne.m = make(map[string]*covEntry, 256)
	}
	covOne.m[k] = &covEntry{v: c, at: time.Now()}
	covOne.mu.Unlock()
	return c
}
