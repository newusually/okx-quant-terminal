package repo

// mysql.go —— MySQL 数据层基座（替代原 SQLite 实现）
//
// 为什么换 MySQL：
//   本项目要同时对 400+ 个 USDT-SWAP 合约做实时写入 + 回补 + 查询。
//   SQLite 是单写者模型（全局一把写锁），400 并发下必然 SQLITE_BUSY 排队，
//   吞吐上不去。MySQL InnoDB 行级锁 + 多写线程才能真正吃满并发。
//
// 性能设计要点：
//   1. 连接池 64 条（2 核机器够用），idle 32，长连接复用
//   2. interpolateParams=true —— 免去每条语句的 prepare 往返，高并发下省一半 RTT
//   3. 全部写入走「多行 INSERT ... ON DUPLICATE KEY UPDATE」批量提交，
//      一次网络往返写 500 行，比逐行 Exec 快 20~50 倍
//   4. 表结构精简：kline 只保留聚簇主键，不建冗余二级索引（省 ~1GB 磁盘）
//   5. innodb_flush_log_at_trx_commit=2（见 conf/my.ini），提交不实时 fsync

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"finally-main/internal/conf"
	"finally-main/internal/logx"
	"finally-main/internal/model"
)

// ---------------------------------------------------------------------------
// 连接配置
// ---------------------------------------------------------------------------

// MySQLConfig MySQL 连接参数
type MySQLConfig struct {
	Host     string // 默认 127.0.0.1
	Port     int    // 默认 3306
	User     string
	Password string
	Database string

	MaxOpenConns int           // 连接池上限，默认 64
	MaxIdleConns int           // 空闲连接，默认 32
	ConnMaxLife  time.Duration // 连接最长寿命，默认 30min
	ConnMaxIdle  time.Duration // 空闲最长寿命，默认 5min

	Timeout      time.Duration // 建连超时，默认 10s
	ReadTimeout  time.Duration // 读超时，默认 60s
	WriteTimeout time.Duration // 写超时，默认 60s

	// LongDDL 只给维护路径用（-partition / -repartition）。
	// 为 true 时 DSN 里的 readTimeout/writeTimeout 设成 0，
	// 否则整表重建跑到 60 秒会被驱动掐断连接（见 DSN 的注释）。
	LongDDL bool

	BatchSize int // 批量写入的分片大小，默认 500
}

// DefaultMySQLConfig 本机默认配置（与 conf/my.ini、scripts/init_db.sql 一致）
//
// ★ 口令不在这里 ★
//
//	2026-10-01 之前，这个函数里写死了 `Password: "OkxQuant****"`（旧值已轮换失效），
//	连同 cmd/okxweb、cmd/okxbench 的 flag 默认值，一共 4 处明文躺在
//	public 仓库里。现在改成本文件不持有口令，而是走 conf 的解析链：
//
//	    环境变量 OKX_MYSQL_PASS  →  <项目根>\.mysql-pass  →  空
//
//	详见 internal/conf/secret.go。设置 / 轮换用 scripts\set_db_pass.bat。
//
//	为什么口令一定要比 user/db 更晚决定：user 和 db 是「怎么连」，
//	口令是「凭什么连」，只有它需要保密，所以只有它单独走 secret 通道。
//
// ★ 连接池为什么收这么小（2 逻辑核 / 1.97GB 的机器）：
//
//	老配置 MaxOpen=64 / MaxIdle=32，实测 processlist 长期挂着 55 条连接
//	（52 条 Sleep）。MySQL 每条连接 = 一个 OS 线程，线程数量在 2 核机器上
//	直接换算成上下文切换开销；而这台机器上真正并发的查询本来就只有几个
//	（引擎巡检、ticker 落库、网页轮询），64 个并发连接纯属浪费。
//	MaxIdle=4 + 60 秒自动回收，让「闲置线程」自己消失。
//
//	注意：连接池不是「越大越抗压」。上一轮全站雪崩（查询 8~20 秒）恰恰是
//	连接池被慢查询占满导致的 —— 池子越大，堆积的慢查询越多，雪崩越猛。
func DefaultMySQLConfig() MySQLConfig {
	pass, _ := conf.MySQLSecret()
	return MySQLConfig{
		Host: "127.0.0.1", Port: 3306,
		User: conf.MySQLUser(), Password: pass, Database: conf.DefaultMySQLDatabase,
		MaxOpenConns: 16, MaxIdleConns: 4,
		ConnMaxLife: 30 * time.Minute, ConnMaxIdle: 60 * time.Second,
		Timeout: 10 * time.Second, ReadTimeout: 60 * time.Second, WriteTimeout: 60 * time.Second,
		BatchSize: 500,
	}
}

// MySQLSecretSource 口令来源描述（「环境变量 OKX_MYSQL_PASS」/「密钥文件 …」/
// 「★ 未配置」）。启动横幅和 -acct 诊断都打这一行 ——
// 排障时第一个要确认的就是「口令到底从哪来的」，而**只报来源不报口令**，
// 因为日志经常被人截图贴出去。
func MySQLSecretSource() string {
	if _, src := conf.MySQLSecret(); src != "" {
		return src
	}
	return "★ 未配置"
}

// MySQLHint 未配置口令时的操作提示（转给 conf，给 CLI 打印用）
func MySQLHint() string { return conf.MySQLHint() }

// normalize 补齐零值，避免调用方漏填导致怪异行为
func (c *MySQLConfig) normalize() {
	d := DefaultMySQLConfig()
	if c.Host == "" {
		c.Host = d.Host
	}
	if c.Port == 0 {
		c.Port = d.Port
	}
	if c.User == "" {
		c.User = d.User
	}
	if c.Database == "" {
		c.Database = d.Database
	}
	if c.MaxOpenConns <= 0 {
		c.MaxOpenConns = d.MaxOpenConns
	}
	if c.MaxIdleConns <= 0 {
		c.MaxIdleConns = d.MaxIdleConns
	}
	if c.MaxIdleConns > c.MaxOpenConns {
		c.MaxIdleConns = c.MaxOpenConns
	}
	if c.ConnMaxLife <= 0 {
		c.ConnMaxLife = d.ConnMaxLife
	}
	if c.ConnMaxIdle <= 0 {
		c.ConnMaxIdle = d.ConnMaxIdle
	}
	if c.Timeout <= 0 {
		c.Timeout = d.Timeout
	}
	if c.ReadTimeout <= 0 {
		c.ReadTimeout = d.ReadTimeout
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = d.WriteTimeout
	}
	if c.BatchSize <= 0 {
		c.BatchSize = d.BatchSize
	}
	if c.BatchSize > 2000 {
		c.BatchSize = 2000
	}
}

// DSN 拼 go-sql-driver/mysql 的数据源串
//
// interpolateParams=true 是这个项目性能的关键：
// 驱动会在本地把参数拼进 SQL 再发，省掉 Prepare→Execute→Close 三个往返。
// 参数由驱动自己转义，注入安全。
//
// LongDDL=true 时把 readTimeout / writeTimeout 设成 0（=不超时）。
// 为什么需要这个开关：`ALTER TABLE kline PARTITION BY ...` 是整表重建，
// 421MB 在本机要跑 3~5 分钟，而日常 DSN 里 readTimeout=60s —— 时间一到
// 驱动直接断开连接，服务端看到的是一次「客户端不见了」，重建白做。
// 日常查询仍然保留超时（连接卡死能被及时发现），只有维护路径关掉它。
func (c MySQLConfig) DSN() string {
	readTO, writeTO := dur(c.ReadTimeout), dur(c.WriteTimeout)
	if c.LongDDL {
		readTO, writeTO = "0", "0"
	}
	return fmt.Sprintf(
		"%s:%s@tcp(%s:%d)/%s"+
			"?charset=utf8mb4&collation=utf8mb4_general_ci"+
			"&allowNativePasswords=true"+
			"&interpolateParams=true"+
			"&timeout=%s&readTimeout=%s&writeTimeout=%s"+
			"&maxAllowedPacket=67108864"+
			"&clientFoundRows=false",
		c.User, c.Password, c.Host, c.Port, c.Database,
		dur(c.Timeout), readTO, writeTO)
}

func dur(d time.Duration) string {
	if d%time.Second == 0 {
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
	return d.String()
}

// ---------------------------------------------------------------------------
// DDL —— MySQL 语法（utf8mb4 / InnoDB / 紧凑行格式）
// ---------------------------------------------------------------------------

// schemaStmts 建表语句（逐条执行，MySQL 驱动不支持一次多条）
//
// 与旧 SQLite 库的语义一一对应，字段名保持一致，业务代码无感。
var schemaStmts = []string{
	// ---- K 线：全项目最大的表（400 合约 × 6 周期 × 30 天 ≈ 3000 万行）----
	// 只保留聚簇主键，不建二级索引：省 ~1GB 磁盘，且网页查询都带 inst_id 前缀
	`CREATE TABLE IF NOT EXISTS kline (
		inst_id VARCHAR(32) NOT NULL,
		bar     VARCHAR(4)  NOT NULL,
		ts      BIGINT      NOT NULL,
		o DOUBLE NOT NULL DEFAULT 0,
		h DOUBLE NOT NULL DEFAULT 0,
		l DOUBLE NOT NULL DEFAULT 0,
		c DOUBLE NOT NULL DEFAULT 0,
		v DOUBLE NOT NULL DEFAULT 0,
		PRIMARY KEY (inst_id, bar, ts)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci ROW_FORMAT=DYNAMIC`,

	// ---- 压测专用表：结构和 kline 完全一致，但压测永远只写这张表 ----
	// 血泪教训：压测工具曾经直接写 kline，28.68 万行合成数据把线上所有
	// 均线/布林带算歪。隔离到独立表之后，物理上就不可能再污染生产数据。
	`CREATE TABLE IF NOT EXISTS kline_bench (
		inst_id VARCHAR(32) NOT NULL,
		bar     VARCHAR(4)  NOT NULL,
		ts      BIGINT      NOT NULL,
		o DOUBLE NOT NULL DEFAULT 0,
		h DOUBLE NOT NULL DEFAULT 0,
		l DOUBLE NOT NULL DEFAULT 0,
		c DOUBLE NOT NULL DEFAULT 0,
		v DOUBLE NOT NULL DEFAULT 0,
		PRIMARY KEY (inst_id, bar, ts)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci ROW_FORMAT=DYNAMIC`,

	// ---- 信号：8 因子明细 ----
	`CREATE TABLE IF NOT EXISTS signals (
		id      BIGINT NOT NULL AUTO_INCREMENT,
		inst_id VARCHAR(32) NOT NULL,
		bar     VARCHAR(4)  NOT NULL,
		ts      BIGINT      NOT NULL,
		close   DOUBLE   DEFAULT 0,
		mask    INT      DEFAULT 0,
		score   INT      DEFAULT 0,
		rise_pct DOUBLE DEFAULT 0,
		hit_list VARCHAR(255) DEFAULT '',
		pot DOUBLE DEFAULT 0, fri DOUBLE DEFAULT 0, kin DOUBLE DEFAULT 0,
		rsi DOUBLE DEFAULT 0, td INT DEFAULT 0,
		acted  INT DEFAULT 0,
		reason VARCHAR(255) DEFAULT '',
		ai_note TEXT,
		created_at BIGINT DEFAULT 0,
		PRIMARY KEY (id),
		UNIQUE KEY uk_signal (inst_id, bar, ts),
		KEY ix_signal_ts (ts),
		KEY ix_signal_score (score, ts)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`,

	// ---- 成交 / 持仓 ----
	`CREATE TABLE IF NOT EXISTS trade (
		id       BIGINT NOT NULL AUTO_INCREMENT,
		inst_id  VARCHAR(32) NOT NULL,
		side     VARCHAR(8)  DEFAULT 'buy',
		sz       DOUBLE   DEFAULT 0,
		entry_px DOUBLE   DEFAULT 0,
		exit_px  DOUBLE   DEFAULT 0,
		margin   DOUBLE   DEFAULT 0,
		leverage INT      DEFAULT 0,
		open_ts  BIGINT   DEFAULT 0,
		close_ts BIGINT   DEFAULT 0,
		pnl      DOUBLE   DEFAULT 0,
		pnl_pct  DOUBLE   DEFAULT 0,
		reason   VARCHAR(255) DEFAULT '',
		ord_id   VARCHAR(64)  DEFAULT '',
		pos_id   VARCHAR(64)  DEFAULT '',
		status   VARCHAR(16)  DEFAULT 'open',
		score    INT      DEFAULT 0,
		bar      VARCHAR(4)   DEFAULT '',
		ai_note  TEXT,
		addon_count  INT    DEFAULT 0,
		addon_margin DOUBLE DEFAULT 0,
		last_addon_ts BIGINT DEFAULT 0,
		PRIMARY KEY (id),
		KEY ix_trade_status (status, inst_id),
		KEY ix_trade_open_ts (open_ts),
		KEY ix_trade_close_ts (close_ts)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`,

	// ---- 权益快照 ----
	`CREATE TABLE IF NOT EXISTS equity (
		ts        BIGINT NOT NULL,
		total_eq  DOUBLE DEFAULT 0,
		avail     DOUBLE DEFAULT 0,
		upl       DOUBLE DEFAULT 0,
		pos_count INT    DEFAULT 0,
		PRIMARY KEY (ts)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`,

	// ---- 运行日志（加自增主键，避免 InnoDB 用隐藏聚簇索引）----
	`CREATE TABLE IF NOT EXISTS runlog (
		id    BIGINT NOT NULL AUTO_INCREMENT,
		ts    BIGINT NOT NULL,
		level VARCHAR(8) DEFAULT 'INFO',
		msg   TEXT,
		PRIMARY KEY (id),
		KEY ix_runlog_ts (ts)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`,

	// ---- 交易事件流水（开仓 / 加仓 / 平仓，一次一笔）----
	//
	// trade 表一个仓位只有一行，加仓是就地合并进原行的，所以「每次加仓
	// 在什么时间、什么价格、加了多少钱」在原表里留不下来。K 线图上要按
	// 时间点标出买入/加仓/平仓、历史里要能翻交易记录详情，都靠这张流水表。
	//
	// 唯一键 (inst_id, kind, ts, trade_id) 是幂等保护：引擎重放同一批事件不会写重。
	//
	// 为什么必须带 trade_id：OKX 一笔大单会拆成多笔成交，这些成交的 fillTime
	// 经常落在同一毫秒。只用 (inst_id, kind, ts) 时，同毫秒的后续成交会被
	// ON DUPLICATE KEY UPDATE 吃掉 —— 实测 100 笔成交只入库 49 条。
	// 程序自己产生的事件 trade_id=0，等价于原先的粒度，不受影响。
	`CREATE TABLE IF NOT EXISTS trade_event (
		id      BIGINT NOT NULL AUTO_INCREMENT,
		inst_id VARCHAR(32) NOT NULL,
		kind    VARCHAR(12) NOT NULL,
		ts      BIGINT NOT NULL,
		px      DOUBLE DEFAULT 0,
		sz      DOUBLE DEFAULT 0,
		margin  DOUBLE DEFAULT 0,
		leverage INT   DEFAULT 0,
		pnl     DOUBLE DEFAULT 0,
		pnl_pct DOUBLE DEFAULT 0,
		score   INT    DEFAULT 0,
		reason  VARCHAR(255) DEFAULT '',
		ord_id  VARCHAR(64)  DEFAULT '',
		trade_id BIGINT DEFAULT 0,
		created_at BIGINT DEFAULT 0,
		PRIMARY KEY (id),
		UNIQUE KEY uk_event (inst_id, kind, ts, trade_id),
		KEY ix_event_inst_ts (inst_id, ts),
		KEY ix_event_ts (ts)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`,

	// ---- 键值元数据 ----
	`CREATE TABLE IF NOT EXISTS meta (
		k VARCHAR(64) NOT NULL,
		v TEXT,
		PRIMARY KEY (k)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`,

	// ---- AI 调用流水 ----
	`CREATE TABLE IF NOT EXISTS ai_call (
		id      BIGINT NOT NULL AUTO_INCREMENT,
		ts      BIGINT NOT NULL,
		inst_id VARCHAR(32) DEFAULT '',
		note    TEXT,
		PRIMARY KEY (id),
		KEY ix_ai_call_ts (ts)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`,

	// ---- 合约静态信息（前端下拉框数据源）----
	`CREATE TABLE IF NOT EXISTS inst (
		inst_id      VARCHAR(32) NOT NULL,
		base_ccy     VARCHAR(16) DEFAULT '',
		quote_ccy    VARCHAR(16) DEFAULT '',
		settle_ccy   VARCHAR(16) DEFAULT '',
		ct_val       DOUBLE DEFAULT 0,
		ct_mult      DOUBLE DEFAULT 0,
		lot_sz       DOUBLE DEFAULT 0,
		min_sz       DOUBLE DEFAULT 0,
		tick_sz      DOUBLE DEFAULT 0,
		lever        INT    DEFAULT 0,
		state        VARCHAR(16) DEFAULT '',
		list_time    BIGINT DEFAULT 0,
		quote_vol24h DOUBLE DEFAULT 0,
		updated_at   BIGINT DEFAULT 0,
		inst_category VARCHAR(4) DEFAULT '',
		tradeable    TINYINT DEFAULT 0,
		exclude_reason VARCHAR(24) DEFAULT '',
		PRIMARY KEY (inst_id),
		KEY ix_inst_vol (quote_vol24h)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`,

	// ---- 实时行情快照 ----
	`CREATE TABLE IF NOT EXISTS ticker (
		inst_id      VARCHAR(32) NOT NULL,
		ts           BIGINT   DEFAULT 0,
		last         DOUBLE   DEFAULT 0,
		open24h      DOUBLE   DEFAULT 0,
		high24h      DOUBLE   DEFAULT 0,
		low24h       DOUBLE   DEFAULT 0,
		vol24h       DOUBLE   DEFAULT 0,
		vol_ccy24h   DOUBLE   DEFAULT 0,
		quote_vol24h DOUBLE   DEFAULT 0,
		chg_pct      DOUBLE   DEFAULT 0,
		PRIMARY KEY (inst_id),
		KEY ix_ticker_vol (quote_vol24h)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`,

	// ---- 回补任务进度 ----
	`CREATE TABLE IF NOT EXISTS backfill_job (
		inst_id    VARCHAR(32) NOT NULL,
		bar        VARCHAR(4)  NOT NULL,
		from_ts    BIGINT DEFAULT 0,
		to_ts      BIGINT DEFAULT 0,
		rows_cnt   BIGINT DEFAULT 0,
		status     VARCHAR(16) DEFAULT '',
		msg        VARCHAR(255) DEFAULT '',
		updated_at BIGINT DEFAULT 0,
		PRIMARY KEY (inst_id, bar),
		KEY ix_job_updated (updated_at)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`,

	// ---- 实时盈亏流水 ----
	`CREATE TABLE IF NOT EXISTS pnl_point (
		ts        BIGINT NOT NULL,
		upl       DOUBLE DEFAULT 0,
		total_eq  DOUBLE DEFAULT 0,
		pos_count INT    DEFAULT 0,
		PRIMARY KEY (ts)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`,

	// ---- 历史信号回算的扫描水位线（双向）----
	//
	// K 线回补是「从最近往老补」的：先有 [T-1天, 现在]，再往前扩到 [T-30天, 现在]。
	// 所以只记 MAX(ts) 会漏 —— 后补进来的更老 K 线全都比水位线小，被当成
	// 「算过了」跳过，信号永远追不上 K 线（1m/3m/5m 卡在只有几个合约有信号）。
	//
	// 这里同时记 min_ts / max_ts：已扫区间是闭区间 [min_ts, max_ts]，
	// 每轮只补两头新增的部分（左边新回补的老 K 线 + 右边新生成的新 K 线）。
	`CREATE TABLE IF NOT EXISTS signal_scan_state (
		inst_id    VARCHAR(32) NOT NULL,
		bar        VARCHAR(4)  NOT NULL,
		min_ts     BIGINT NOT NULL DEFAULT 0,
		max_ts     BIGINT NOT NULL DEFAULT 0,
		scanned    BIGINT NOT NULL DEFAULT 0,
		updated_at BIGINT NOT NULL DEFAULT 0,
		PRIMARY KEY (inst_id, bar)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`,
}

// ---------------------------------------------------------------------------
// DB
// ---------------------------------------------------------------------------

// DB MySQL 句柄。
//
// 注意：并发安全 —— database/sql 自带连接池，400 个 goroutine 同时写也只是
// 从池里各拿一条连接，不会像 SQLite 那样互相锁。
type DB struct {
	sql *sql.DB
	cfg MySQLConfig

	// tblCache / tblMu：表是否存在的查询缓存。
	// retention.go 的清理清单会在每次运行时逐表判断存在性，
	// 每次都去查 information_schema 是没必要的（一进程内表不会凭空出现/消失）。
	tblMu    sync.Mutex
	tblCache map[string]bool
}

// OpenMySQL 连库 + 建表
func OpenMySQL(cfg MySQLConfig) (*DB, error) {
	cfg.normalize()
	db, err := sql.Open("mysql", cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("打开 MySQL 失败：%w", err)
	}
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLife)
	db.SetConnMaxIdleTime(cfg.ConnMaxIdle)

	// 建连探活（直到这里失败才说明 MySQL 真没起来）
	if err := db.Ping(); err != nil {
		db.Close()
		// 口令是空的就别只说「Access denied」了 —— 那会让人以为是口令写错，
		// 实际上根本没配。直接告诉他去哪配。
		if cfg.Password == "" {
			return nil, fmt.Errorf("连接 MySQL %s:%d 失败（用户 %s，口令为空）：%w\n%s",
				cfg.Host, cfg.Port, cfg.User, err, conf.MySQLHint())
		}
		return nil, fmt.Errorf("连接 MySQL %s:%d 失败（用户 %s，口令来源：%s）：%w",
			cfg.Host, cfg.Port, cfg.User, MySQLSecretSource(), err)
	}

	d := &DB{sql: db, cfg: cfg}
	if err := d.Init(); err != nil {
		db.Close()
		return nil, err
	}
	return d, nil
}

// Init 建表 + 增量迁移（幂等）
func (d *DB) Init() error {
	for _, stmt := range schemaStmts {
		if _, err := d.sql.Exec(stmt); err != nil {
			return fmt.Errorf("建表失败：%w\nSQL: %s", err, firstLine(stmt))
		}
	}
	if err := d.migrate(); err != nil {
		return err
	}
	// 索引同步放在最后：表/列都齐了再建，避免「列还不存在」的假失败。
	if n, msgs, err := d.applyIndexPlan(); err != nil {
		return err
	} else if len(msgs) > 0 {
		logx.Logf("INFO", "[INDEX] 索引同步：新建/重建 %d 条", n)
		for _, m := range msgs {
			logx.Logf("INFO", "[INDEX]   %s", m)
		}
	}
	return nil
}

// migrate 增量迁移。
// MySQL 的 ALTER TABLE ADD COLUMN 没有 IF NOT EXISTS，所以先查 information_schema。
func (d *DB) migrate() error {
	migs := []struct{ table, col, def string }{
		{"inst", "inst_category", "VARCHAR(4) DEFAULT '' AFTER quote_vol24h"},
		{"inst", "tradeable", "TINYINT DEFAULT 0"},
		{"inst", "exclude_reason", "VARCHAR(24) DEFAULT ''"},
		// 加仓（浮亏补仓）三件套：加了几次 / 累计加仓保证金 / 上次加仓时间
		{"trade", "addon_count", "INT DEFAULT 0"},
		{"trade", "addon_margin", "DOUBLE DEFAULT 0"},
		{"trade", "last_addon_ts", "BIGINT DEFAULT 0"},
		// pos_id：OKX 的仓位 ID。历史仓位同步（okxpositions.go）靠它做幂等 ——
		// 同一个仓位反复同步只会 UPDATE，不会插出重复行。
		// 引擎自己下的单拿不到 posId，先留空，等同步时按「合约 + 开仓时间」认领。
		{"trade", "pos_id", "VARCHAR(64) DEFAULT '' AFTER ord_id"},
		// rise_pct：信号那根 K 线的涨跌幅 (c-o)/o*100（二十一期，用户口径
		// 「标记下跌多少百分比 跌幅告诉我 记录在数据库」）。存量行由 SQL 回填。
		{"signals", "rise_pct", "DOUBLE DEFAULT 0 AFTER score"},
	}
	for _, m := range migs {
		if err := d.ensureColumn(m.table, m.col, m.def); err != nil {
			return fmt.Errorf("迁移 %s.%s 失败：%w", m.table, m.col, err)
		}
	}
	// 表名迁移：老的 signal 表（signal 是 MySQL 保留字）改名 signals
	if err := d.renameTableIfExists("signal", "signals"); err != nil {
		return err
	}
	// 唯一键迁移：trade_event 的 uk_event 从 (inst_id, kind, ts) 扩到带上 trade_id。
	// 原因见 schemaStmts 里的注释：同毫秒多笔成交会互相覆盖。
	// MySQL 8 的 DROP/ADD INDEX 是 INPLACE + LOCK=NONE，不会阻塞引擎写入。
	if err := d.ensureUniqueIndex("trade_event", "uk_event",
		[]string{"inst_id", "kind", "ts", "trade_id"}); err != nil {
		return err
	}
	return nil
}

// ensureUniqueIndex 保证 table 上存在名为 index 的唯一索引，且列清单完全等于 cols。
//
// 实现统一走 indexes.go 的 syncIndex（同一套「比对 information_schema → 缺则建、
// 列不对则重建」的逻辑），避免这里和 indexPlan 两处各写一份、日后分叉。
func (d *DB) ensureUniqueIndex(table, index string, cols []string) error {
	_, err := d.syncIndex(idxDef{Table: table, Name: index, Unique: true, Cols: cols})
	return err
}

func (d *DB) ensureColumn(table, col, def string) error {
	if !safeIdent(table) || !safeIdent(col) {
		return fmt.Errorf("非法标识符：%s.%s", table, col)
	}
	var n int
	err := d.sql.QueryRow(
		`SELECT COUNT(*) FROM information_schema.columns
		 WHERE table_schema=DATABASE() AND table_name=? AND column_name=?`,
		table, col).Scan(&n)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err = d.sql.Exec("ALTER TABLE `" + table + "` ADD COLUMN `" + col + "` " + def)
	return err
}

func (d *DB) renameTableIfExists(from, to string) error {
	if !safeIdent(from) || !safeIdent(to) {
		return fmt.Errorf("非法表名：%s -> %s", from, to)
	}
	var n int
	if err := d.sql.QueryRow(
		`SELECT COUNT(*) FROM information_schema.tables
		 WHERE table_schema=DATABASE() AND table_name=?`, from).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	var m int
	if err := d.sql.QueryRow(
		`SELECT COUNT(*) FROM information_schema.tables
		 WHERE table_schema=DATABASE() AND table_name=?`, to).Scan(&m); err != nil {
		return err
	}
	if m > 0 {
		return nil // 目标已存在，不动
	}
	_, err := d.sql.Exec("RENAME TABLE `" + from + "` TO `" + to + "`")
	return err
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Close 关连接池
func (d *DB) Close() error { return d.sql.Close() }

// SQL 暴露底层句柄
func (d *DB) SQL() *sql.DB { return d.sql }

// Config 返回连接配置
func (d *DB) Config() MySQLConfig { return d.cfg }

// Path 兼容旧接口：返回 MySQL 的「地址/库」，日志里打印用
func (d *DB) Path() string {
	return fmt.Sprintf("mysql://%s@%s:%d/%s", d.cfg.User, d.cfg.Host, d.cfg.Port, d.cfg.Database)
}

// DBName 当前库名
func (d *DB) DBName() string { return d.cfg.Database }

// Tables 列出所有表
// Tables 所有基础表的表名（按名字排序）
//
// 走元数据缓存：information_schema 实测 2.4 秒（要打开 data dictionary），
// 表结构几乎不变，缓存 5 分钟。
func (d *DB) Tables() ([]string, error) {
	metas, err := d.tableMeta()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(metas))
	for _, m := range metas {
		out = append(out, m.Name)
	}
	return out, nil
}

// TableCounts 每张表的行数
//
// ★ 2026-10-01 二次修复：**所有表**都只读进程内缓存，请求路径上一条 COUNT 都不发。
//
//	原来只给「大表」走缓存，小表（meta / signal_scan_state / equity …）每次请求
//	都实时 COUNT。看起来毫秒级很便宜，实际上：
//	  · 慢查询日志里 `SELECT COUNT(*) FROM \`meta\`` 出现了 302 次、
//	    `... signal_scan_state` 276 次、`... equity` 149 次
//	  · 这些表在 IO 争抢（回补批量写 kline）时，光是「打开表 + 拿一次快照」
//	    就能超过 long_query_time
//	  · 而 /api/state /api/tables 是被前端高频轮询的
//	结论：只要在请求路径上，COUNT 就不该出现 —— 行数是展示值，缓存 10 分钟无感。
//
// 缓存未预热时返回缓存里已有的（可能缺几张表），并顺手触发一次后台刷新。
func (d *DB) TableCounts() (map[string]int64, error) {
	tabs, err := d.Tables()
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(tabs))
	miss := 0
	for _, t := range tabs {
		if !safeIdent(t) {
			continue
		}
		if n, ok := d.RowCount(t); ok {
			out[t] = n
			continue
		}
		miss++
		// 缓存里彻底没有（进程刚起、还没预热完）才实时数一次。
		// 热态下永远走不到这里。
		var n int64
		if err := d.sql.QueryRow("SELECT COUNT(*) FROM `" + t + "`").Scan(&n); err != nil {
			return nil, err
		}
		out[t] = n
	}
	if miss > 0 && rowCache.busy.CompareAndSwap(false, true) {
		go func() {
			defer rowCache.busy.Store(false)
			d.refreshRowCountsLocked()
		}()
	}
	return out, nil
}

// isBigRowTable 判断是否是需要走缓存的大表
func isBigRowTable(t string) bool {
	for _, b := range bigRowTables {
		if b == t {
			return true
		}
	}
	return false
}

// safeIdent 表名白名单校验（表名来自 information_schema，仍然防一手）
func safeIdent(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r == '_' || r == '$' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// count 单值计数
func (d *DB) count(sqlStr string, args ...any) (int64, error) {
	var n int64
	if err := d.sql.QueryRow(sqlStr, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// Ping 探活
func (d *DB) Ping() error { return d.sql.Ping() }

// ServerVersion 数据库版本（自检用）
func (d *DB) ServerVersion() string {
	var v string
	if err := d.sql.QueryRow("SELECT VERSION()").Scan(&v); err != nil {
		return ""
	}
	return v
}

// PoolStats 连接池状态（自检 / 监控用）
func (d *DB) PoolStats() map[string]any {
	st := d.sql.Stats()
	return map[string]any{
		"open":        st.OpenConnections,
		"inUse":       st.InUse,
		"idle":        st.Idle,
		"maxOpen":     st.MaxOpenConnections,
		"waitCount":   st.WaitCount,
		"waitSeconds": st.WaitDuration.Seconds(),
	}
}

// ---------------------------------------------------------------------------
// 批量写入助手 —— 高并发的核心
// ---------------------------------------------------------------------------

// bulkUpsert 把 rows 分片拼成多行 INSERT ... ON DUPLICATE KEY UPDATE
//
//	table  表名
//	cols   列名
//	rows   每行一组参数（长度必须等于 len(cols)）
//	updateCols 冲突时要更新的列（空 = 走 INSERT IGNORE 语义，不改已有行）
//
// 返回实际发送的行数。
func (d *DB) bulkUpsert(table string, cols []string, rows [][]any, updateCols []string) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	if !safeIdent(table) {
		return 0, fmt.Errorf("非法表名：%s", table)
	}
	ncol := len(cols)
	bs := d.cfg.BatchSize
	if bs <= 0 {
		bs = 500
	}

	// 预拼 SQL 骨架
	colList := "`" + strings.Join(cols, "`,`") + "`"
	rowPlaceholder := "(" + strings.TrimSuffix(strings.Repeat("?,", ncol), ",") + ")"
	tail := " ON DUPLICATE KEY UPDATE " + joinset(updateCols)
	if len(updateCols) == 0 {
		tail = ""
	}
	head := "INSERT INTO `" + table + "` (" + colList + ") VALUES "
	if len(updateCols) == 0 {
		head = "INSERT IGNORE INTO `" + table + "` (" + colList + ") VALUES "
	}

	total := 0
	for start := 0; start < len(rows); start += bs {
		end := start + bs
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]

		var sb strings.Builder
		sb.Grow(len(head) + (len(rowPlaceholder)+1)*len(chunk) + len(tail) + 8)
		sb.WriteString(head)
		args := make([]any, 0, ncol*len(chunk))
		for i, r := range chunk {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(rowPlaceholder)
			if len(r) != ncol {
				return total, fmt.Errorf("表 %s 第 %d 行列数不匹配：期望 %d 实际 %d", table, start+i, ncol, len(r))
			}
			args = append(args, r...)
		}
		sb.WriteString(tail)

		if _, err := d.sql.Exec(sb.String(), args...); err != nil {
			return total, fmt.Errorf("批量写 %s 失败（第 %d~%d 行）：%w", table, start, end-1, err)
		}
		total += len(chunk)
	}
	return total, nil
}

// joinset 生成 ON DUPLICATE KEY UPDATE 的赋值串
func joinset(cols []string) string {
	if len(cols) == 0 {
		return "1=1"
	}
	parts := make([]string, 0, len(cols))
	for _, c := range cols {
		if !safeIdent(c) {
			continue
		}
		parts = append(parts, "`"+c+"`=VALUES(`"+c+"`)")
	}
	if len(parts) == 0 {
		return "1=1"
	}
	return strings.Join(parts, ",")
}

// quoteIdent 反引号包列名（拼 SQL 用）
func quoteIdent(s string) string { return "`" + s + "`" }

// 编译期断言：model 包被引用（避免 import 抖动）
var _ = model.Kline{}
