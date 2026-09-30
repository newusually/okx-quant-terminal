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
	"time"

	_ "github.com/go-sql-driver/mysql"

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

	BatchSize int // 批量写入的分片大小，默认 500
}

// DefaultMySQLConfig 本机默认配置（与 conf/my.ini、scripts/init_db.sql 一致）
func DefaultMySQLConfig() MySQLConfig {
	return MySQLConfig{
		Host: "127.0.0.1", Port: 3306,
		User: "okx", Password: "OkxQuant2026", Database: "okx",
		MaxOpenConns: 64, MaxIdleConns: 32,
		ConnMaxLife: 30 * time.Minute, ConnMaxIdle: 5 * time.Minute,
		Timeout: 10 * time.Second, ReadTimeout: 60 * time.Second, WriteTimeout: 60 * time.Second,
		BatchSize: 500,
	}
}

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
func (c MySQLConfig) DSN() string {
	return fmt.Sprintf(
		"%s:%s@tcp(%s:%d)/%s"+
			"?charset=utf8mb4&collation=utf8mb4_general_ci"+
			"&allowNativePasswords=true"+
			"&interpolateParams=true"+
			"&timeout=%s&readTimeout=%s&writeTimeout=%s"+
			"&maxAllowedPacket=67108864"+
			"&clientFoundRows=false",
		c.User, c.Password, c.Host, c.Port, c.Database,
		dur(c.Timeout), dur(c.ReadTimeout), dur(c.WriteTimeout))
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

	// ---- 信号：8 因子明细 ----
	`CREATE TABLE IF NOT EXISTS signals (
		id      BIGINT NOT NULL AUTO_INCREMENT,
		inst_id VARCHAR(32) NOT NULL,
		bar     VARCHAR(4)  NOT NULL,
		ts      BIGINT      NOT NULL,
		close   DOUBLE   DEFAULT 0,
		mask    INT      DEFAULT 0,
		score   INT      DEFAULT 0,
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
		return nil, fmt.Errorf("连接 MySQL %s:%d 失败：%w", cfg.Host, cfg.Port, err)
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
	return nil
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
func (d *DB) Tables() ([]string, error) {
	rows, err := d.sql.Query(
		`SELECT table_name FROM information_schema.tables
		 WHERE table_schema = DATABASE() AND table_type='BASE TABLE'
		 ORDER BY table_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// TableCounts 每张表的行数
//
// 用 information_schema 的估算值会不准，这里对 kline 用精确 COUNT、
// 其余小表也精确 COUNT（都是百万级以下，可接受）。
func (d *DB) TableCounts() (map[string]int64, error) {
	tabs, err := d.Tables()
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(tabs))
	for _, t := range tabs {
		if !safeIdent(t) {
			continue
		}
		var n int64
		if err := d.sql.QueryRow("SELECT COUNT(*) FROM `" + t + "`").Scan(&n); err != nil {
			return nil, err
		}
		out[t] = n
	}
	return out, nil
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
