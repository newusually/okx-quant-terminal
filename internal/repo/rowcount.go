package repo

// rowcount.go —— 大表行数 + 表元数据缓存（请求路径上永不出现全表 COUNT）
//
// ---------------------------------------------------------------------------
// 事故复盘（2026-10-01，网页慢到打不开）
// ---------------------------------------------------------------------------
// 现象：网页几乎所有接口要 8~20 秒，curl /api/state 直接超时；
//       MySQL processlist 里同时挂着 8 个 `SELECT COUNT(*) FROM kline`。
//
// 根因链：
//   1. 前端顶栏每 2 秒轮询 /api/account → 走 AccountSnapshot() → d.Stats()
//   2. Stats() 里「kline 行数」先用 information_schema.tables.table_rows 估算，
//      估算为 0 就 fallback 到 `SELECT COUNT(*) FROM kline`
//   3. 本机 InnoDB 统计信息失效 —— table_rows 恒为 0、data_length 只有 16KB
//      （真实 411 万行 / 数百 MB），于是**每次请求都退化成全表扫描**
//   4. 实测单次 7.76 秒；2 秒一轮的轮询持续堆积 → 连接池被占满 → 全站雪崩
//   5. /api/state 还额外调 TableCounts()，它对每一张表都 COUNT，又扫一遍 kline
//
// 修复原则：
//   · 行数只是「展示用」的近似值，没必要求实时 —— 从请求路径彻底摘掉
//   · 大表行数进进程内缓存，启动后异步数一次，之后每 10 分钟刷新
//   · 表元数据（information_schema）本身也要 2.4 秒，同样缓存 5 分钟
//   · 刷新期间并发请求直接吃旧值（stale-while-revalidate），绝不排队等锁
//
// 记住：换语言（Go→PHP/Python）对这件事**没有任何帮助**。
//       慢的是一条 SQL 的执行计划，不是解释器。

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"finally-main/internal/logx"
)

const (
	// RowCountTTL 大表行数的刷新间隔。
	// 400 万行 COUNT 一次约 8~12 秒，摊到 10 分钟里 CPU 占用可以忽略，
	// 而前端拿到的最多是「10 分钟前的行数」—— 展示用完全够。
	RowCountTTL = 10 * time.Minute
	// tableMetaTTL 表结构元数据的缓存时长（表结构几乎不变，30 分钟足够）
	tableMetaTTL = 30 * time.Minute
)

// bigRowTables 这些表的 COUNT(*) 走索引全扫，代价随行数线性增长。
// 新增「会长到百万级」的表时，往这里加一个名字即可。
//
// ⚠ kline 分区之后 COUNT 反而更慢了（11.9 秒 > 分区前的 7.76 秒）：
// 分区表要逐分区扫索引再合并，20 个分区就是 20 次索引扫描。
// 所以「分区 + 缓存」必须成对出现 —— 只分区不缓存会更糟。
var bigRowTables = []string{"kline", "signals", "equity", "pnl_point", "runlog"}

// ---------------------------------------------------------------------------
// 大表行数缓存
// ---------------------------------------------------------------------------

var rowCache struct {
	mu   sync.RWMutex
	vals map[string]int64
	at   time.Time
	busy atomic.Bool
}

// RowCount 读缓存的表行数。
// 第二个返回值为 false 表示「还没数过」——调用方应自行降级（比如显示 0 或实时 COUNT 小表）。
func (d *DB) RowCount(table string) (int64, bool) {
	rowCache.mu.RLock()
	v, ok := rowCache.vals[table]
	rowCache.mu.RUnlock()
	return v, ok
}

// refreshRowCounts 自己抢锁后刷新（后台定时器用）
func (d *DB) refreshRowCounts() {
	// 同一时刻只允许一个刷新在跑：避免多个 goroutine 同时压全表 COUNT
	if !rowCache.busy.CompareAndSwap(false, true) {
		return
	}
	defer rowCache.busy.Store(false)
	d.refreshRowCountsLocked()
}

// refreshRowCountsLocked 真正去数一遍。
// 调用方必须已经持有 rowCache.busy（CAS 成功），函数本身不再加锁。
//
// ★ 数「所有表」，不只是大表：小表的 COUNT 虽然便宜，但一旦出现在请求路径
// 上（/api/state、/api/tables 被前端高频轮询），在回补批量写 kline 的 IO 争抢
// 期间照样进慢查询日志（`COUNT(*) FROM meta` 累计 302 次）。
// 全部表一起预热，请求路径就再也见不到 COUNT。
func (d *DB) refreshRowCountsLocked() {
	targets := knownTableNames()
	if len(targets) == 0 {
		targets = bigRowTables
	}
	vals := make(map[string]int64, len(targets))
	for _, t := range targets {
		if !safeIdent(t) {
			continue
		}
		var n int64
		if err := d.sql.QueryRow("SELECT COUNT(*) FROM `" + t + "`").Scan(&n); err != nil {
			// 表还没建出来（首次启动）之类：保留上一次的旧值，不写 0 污染缓存
			logx.Logf("WARN", "[CACHE] 统计 %s 行数失败：%v", t, err)
			if old, ok := rowCacheValue(t); ok {
				vals[t] = old
			}
			continue
		}
		vals[t] = n
	}
	rowCache.mu.Lock()
	rowCache.vals = vals
	rowCache.at = time.Now()
	rowCache.mu.Unlock()
}

// StartRowCountRefresher 启动后台行数 + 表元数据刷新。
//
// 刻意做成「异步 + 首次延迟」：启动时的第一要务是让 HTTP 端口尽快监听，
// 不能让十几秒的全表 COUNT 挡在前面（这正是上一版启动卡死的教训）。
//
// 行数和表元数据放在同一个循环里预热：两者都是 /api/state 的数据源，
// 一个慢就等于两个都慢。
func (d *DB) StartRowCountRefresher() {
	go func() {
		// 首次预热：网络层起来 8 秒后再数。
		// 一是避开启动时的写入高峰，二是让启动日志/首次请求尽快拿到真实值。
		time.Sleep(8 * time.Second)
		d.refreshRowCounts()
		// 表元数据（information_schema，5 秒级）也在这里预热一次。
		// 不预热的话，第一个访问 /api/state 的请求会卡 5 秒 ——
		// 以前就是这个毛病：平时很快，缓存一过期就卡一下。
		if metaCache.busy.CompareAndSwap(false, true) {
			_, _ = d.loadTableMeta()
			metaCache.busy.Store(false)
		}

		tk := time.NewTicker(RowCountTTL)
		defer tk.Stop()
		for range tk.C {
			d.refreshRowCounts()
			if metaCache.busy.CompareAndSwap(false, true) {
				_, _ = d.loadTableMeta()
				metaCache.busy.Store(false)
			}
		}
	}()
}

// RowCountAge 缓存年龄（0 表示还没数过），排查用
func (d *DB) RowCountAge() time.Duration {
	rowCache.mu.RLock()
	at := rowCache.at
	rowCache.mu.RUnlock()
	if at.IsZero() {
		return 0
	}
	return time.Since(at)
}

// ---------------------------------------------------------------------------
// 表元数据缓存（表名 / 估算行数 / 占用空间）
// ---------------------------------------------------------------------------

// TableMeta 一张表的元信息
type TableMeta struct {
	Name     string
	Rows     int64 // information_schema 估算值（大表可能不准，展示时用 RowCount 覆盖）
	DataLen  int64
	IndexLen int64
}

var metaCache struct {
	mu   sync.RWMutex
	list []TableMeta
	at   time.Time
	busy atomic.Bool
}

// tableMeta 取表元数据（带 30 分钟缓存）。
//
// ⚠ 这个函数**绝不允许阻塞请求线程**。
//
// information_schema.tables 实测要 5 秒（本机 data dictionary 冷、InnoDB
// 统计信息还失效，MySQL 得逐个打开表拿 data_length/index_length）。
// 而 /api/state 每 10 秒就会走到这里，缓存一过期就正好撞上那 5 秒 ——
// 表现就是「平时很快、每隔一阵子卡一下」。
//
// 所以冷启动路径改成：拿静态表名列表先顶上去（零 DB 往返），
// 真实尺寸交给后台慢慢查，查到了下次请求自然就用上。
// 表名本来就是我们自己建表时写死的，静态解析完全等价。
func (d *DB) tableMeta() ([]TableMeta, error) {
	metaCache.mu.RLock()
	cached, at := metaCache.list, metaCache.at
	metaCache.mu.RUnlock()

	if cached != nil && time.Since(at) < tableMetaTTL {
		return cached, nil
	}

	// 无论有没有旧值，都只负责「踢一脚」后台刷新，自己立刻返回。
	if metaCache.busy.CompareAndSwap(false, true) {
		go func() {
			defer metaCache.busy.Store(false)
			_, _ = d.loadTableMeta()
		}()
	}
	if cached != nil {
		return cached, nil
	}
	return synthesizedTableMeta(), nil
}

// synthesizedTableMeta 从建表语句静态解析出表名，尺寸填 0。
//
// 行数用行数缓存覆盖（调用方会做），尺寸那一栏在刷新完成前显示 0 ——
// 对一个「数据库面板」来说，晚 5 秒出现尺寸远好过整站卡 5 秒。
func synthesizedTableMeta() []TableMeta {
	names := knownTableNames()
	out := make([]TableMeta, 0, len(names))
	for _, n := range names {
		m := TableMeta{Name: n}
		if v, ok := rowCacheValue(n); ok {
			m.Rows = v
		}
		out = append(out, m)
	}
	return out
}

func rowCacheValue(table string) (int64, bool) {
	rowCache.mu.RLock()
	v, ok := rowCache.vals[table]
	rowCache.mu.RUnlock()
	return v, ok
}

// knownTableNames 本项目所有表的表名（从 schemaStmts 里解析）。
//
// 为什么不用 information_schema 拿：那要 5 秒，而这份列表是编译期就确定的。
// 解析失败也不阻塞 —— 退回到常量兜底列表。
func knownTableNames() []string {
	var names []string
	for _, stmt := range schemaStmts {
		if n, ok := tableNameOfDDL(stmt); ok {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return []string{"kline", "kline_bench", "signals", "trade", "trade_event",
			"equity", "runlog", "meta", "ai_call", "inst", "ticker",
			"backfill_job", "pnl_point", "signal_scan_state"}
	}
	return names
}

// tableNameOfDDL 从 `CREATE TABLE IF NOT EXISTS xxx (` 里抠出表名
func tableNameOfDDL(stmt string) (string, bool) {
	const marker = "CREATE TABLE IF NOT EXISTS "
	i := strings.Index(stmt, marker)
	if i < 0 {
		return "", false
	}
	rest := strings.TrimSpace(stmt[i+len(marker):])
	end := strings.IndexAny(rest, " \t\r\n(")
	if end <= 0 {
		return "", false
	}
	name := rest[:end]
	if !safeIdent(name) {
		return "", false
	}
	return name, true
}

// loadTableMeta 查一次 information_schema 并写缓存（只由后台 goroutine 调用）
func (d *DB) loadTableMeta() ([]TableMeta, error) {
	rows, err := d.sql.Query(`
		SELECT table_name,
		       COALESCE(table_rows,0),
		       COALESCE(data_length,0),
		       COALESCE(index_length,0)
		FROM information_schema.tables
		WHERE table_schema = DATABASE() AND table_type='BASE TABLE'
		ORDER BY table_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []TableMeta{}
	for rows.Next() {
		var m TableMeta
		if err := rows.Scan(&m.Name, &m.Rows, &m.DataLen, &m.IndexLen); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	metaCache.mu.Lock()
	metaCache.list = out
	metaCache.at = time.Now()
	metaCache.mu.Unlock()
	return out, nil
}
