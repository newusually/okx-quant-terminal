package repo

// engine_store.go —— 策略引擎侧持久化（MySQL 版）
//
// 【改动说明】
// 老版本这里是 runtime/db.py 的纯 Go 复刻（SQLite + WAL）。
// 400+ 合约并发下 SQLite 的单写者模型成为瓶颈，现整体切到 MySQL：
//   - 复用 mysql.go 的连接池与批量 upsert
//   - 不再需要 WAL / busy_timeout 之类的调优开关
//   - InnoDB 行级锁：不同合约的写入真正并行
//
// 对外方法签名一个没改：
//
//	NewStore(cfg) / Init() / Ingest(payload) / OpenPositions() / Counters(dayStart)
//	Cleanup() / State()
//
// trader.go 不需要任何改动。

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"finally-main/internal/conf"
	"finally-main/internal/model"
)

// ---------------------------------------------------------------------------
// 数据行 —— 统一用 internal/model 里的定义，这里只留别名
// ---------------------------------------------------------------------------

type (
	// KlineRow K 线
	KlineRow = model.KlineRow
	// EngineSignalRow 信号（引擎侧，含 8 因子明细）
	EngineSignalRow = model.EngineSignalRow
	// TradeRow 开仓成交
	TradeRow = model.TradeRow
	// SignalUpdate 对已存在信号行的补充更新（acted / reason / ai_note）
	SignalUpdate = model.SignalUpdate
	// EquityRow 权益快照
	EquityRow = model.EquityRow
	// CloseRow 平仓
	CloseRow = model.CloseRow
	// TradeEventRow 交易事件流水（开仓 / 加仓 / 平仓，一次一笔）
	TradeEventRow = model.TradeEventRow
	// StorePayload 一批要写库的数据
	StorePayload = model.StorePayload
	// RunLogRow 一条运行日志
	RunLogRow = model.RunLogRow
	// OpenPos 数据库里的在持仓记录
	OpenPos = model.OpenPos
	// AddonRow 一次加仓的合并结果
	AddonRow = model.AddonRow
	// Counters 风控要用的当日统计
	Counters = model.Counters
)

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

// Store 引擎侧数据库句柄（底层就是 mysql.go 的 DB）
type Store struct {
	cfg *conf.Config
	db  *DB
	err error
}

// NewStore 建 Store（此时还没连库，第一次用的时候懒连接）
func NewStore(cfg *conf.Config) *Store { return &Store{cfg: cfg} }

func (s *Store) enabled() bool {
	return s.cfg != nil && s.cfg.Store != nil && s.cfg.Store.Enabled
}

// mysqlConfig 从策略配置里取出 MySQL 连接参数
func (s *Store) mysqlConfig() MySQLConfig {
	c := DefaultMySQLConfig()
	st := s.cfg.Store
	if st == nil {
		return c
	}
	if st.Host != "" {
		c.Host = st.Host
	}
	if st.Port > 0 {
		c.Port = st.Port
	}
	if st.User != "" {
		c.User = st.User
	}
	if st.Password != "" {
		c.Password = st.Password
	}
	if st.Database != "" {
		c.Database = st.Database
	}
	if st.MaxOpenConns > 0 {
		c.MaxOpenConns = st.MaxOpenConns
	}
	if st.MaxIdleConns > 0 {
		c.MaxIdleConns = st.MaxIdleConns
	}
	if st.BatchSize > 0 {
		c.BatchSize = st.BatchSize
	}
	return c
}

// open 懒连接 + 建表。失败会记在 s.err 上，后续调用都直接返回这个错误。
func (s *Store) open() (*DB, error) {
	if !s.enabled() {
		return nil, fmt.Errorf("存储未启用")
	}
	if s.db != nil {
		return s.db, nil
	}
	if s.err != nil {
		return nil, s.err
	}
	db, err := OpenMySQL(s.mysqlConfig())
	if err != nil {
		s.err = err
		return nil, s.err
	}
	s.db = db
	return s.db, nil
}

// Init 建表（对应老的 store.Init()）
func (s *Store) Init() error {
	if !s.enabled() {
		return nil
	}
	_, err := s.open()
	return err
}

// Close 关连接（进程退出时调一下更干净）
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// ---------------------------------------------------------------------------
// 写
// ---------------------------------------------------------------------------

// Ingest 批量写入。对应老的 db.py ingest。
//
// 任何一个环节失败都返回错误，由调用方（trader.go）记日志吞掉 ——
// 和以前一样，写库失败绝不影响交易主流程。
func (s *Store) Ingest(p StorePayload) error {
	if !s.enabled() {
		return nil
	}
	if len(p.Kline)+len(p.Signal)+len(p.SignalUpdate)+len(p.Trade)+len(p.Equity)+len(p.Runlog)+len(p.Event) == 0 && p.CloseTrade == nil {
		return nil
	}
	db, err := s.open()
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()

	// ---- K 线（最大批量）----
	if len(p.Kline) > 0 {
		args := make([][]any, 0, len(p.Kline))
		for _, r := range p.Kline {
			args = append(args, []any{r.InstID, r.Bar, r.Ts, r.O, r.H, r.L, r.C, r.V})
		}
		if _, err := db.bulkUpsert("kline", klineCols, args, klineUpdateCols); err != nil {
			return err
		}
	}

	// ---- 信号（唯一键冲突则忽略，不覆盖已有点评）----
	if len(p.Signal) > 0 {
		args := make([][]any, 0, len(p.Signal))
		for _, r := range p.Signal {
			created := r.CreatedAt
			if created == 0 {
				created = now
			}
			args = append(args, []any{r.InstID, r.Bar, r.Ts, r.Close, r.Mask, r.Score, r.RisePct, r.HitList,
				r.Pot, r.Fri, r.Kin, r.Rsi, r.Td, r.Acted, r.Reason, r.AINote, created})
		}
		if _, err := db.bulkUpsert("signals",
			[]string{"inst_id", "bar", "ts", "close", "mask", "score", "rise_pct", "hit_list",
				"pot", "fri", "kin", "rsi", "td", "acted", "reason", "ai_note", "created_at"},
			args, nil); err != nil {
			return err
		}
	}

	// ---- 信号补充更新（acted / reason / ai_note），量很小，逐条 UPDATE ----
	if len(p.SignalUpdate) > 0 {
		stmt, e := db.sql.Prepare(`UPDATE signals SET acted=?, reason=?,
			ai_note=CASE WHEN ?<>'' THEN ? ELSE ai_note END
			WHERE inst_id=? AND bar=? AND ts=?`)
		if e != nil {
			return e
		}
		for _, r := range p.SignalUpdate {
			if _, e = stmt.Exec(r.Acted, r.Reason, r.AINote, r.AINote, r.InstID, r.Bar, r.Ts); e != nil {
				stmt.Close()
				return e
			}
		}
		stmt.Close()
	}

	// ---- 开仓成交 ----
	if len(p.Trade) > 0 {
		args := make([][]any, 0, len(p.Trade))
		for _, r := range p.Trade {
			side := r.Side
			if side == "" {
				side = "buy"
			}
			status := r.Status
			if status == "" {
				status = "open"
			}
			args = append(args, []any{r.InstID, side, r.Sz, r.EntryPx, r.Margin, r.Leverage,
				r.OpenTs, r.Score, r.Bar, r.Reason, r.OrdID, status, r.AINote})
		}
		if _, err := db.bulkUpsert("trade",
			[]string{"inst_id", "side", "sz", "entry_px", "margin", "leverage", "open_ts",
				"score", "bar", "reason", "ord_id", "status", "ai_note"},
			args, nil); err != nil {
			return err
		}
	}

	// ---- 权益快照 ----
	if len(p.Equity) > 0 {
		args := make([][]any, 0, len(p.Equity))
		for _, r := range p.Equity {
			args = append(args, []any{r.Ts, r.TotalEq, r.Avail, r.Upl, r.PosCount})
		}
		if _, err := db.bulkUpsert("equity",
			[]string{"ts", "total_eq", "avail", "upl", "pos_count"},
			args, []string{"total_eq", "avail", "upl", "pos_count"}); err != nil {
			return err
		}
	}

	// ---- 运行日志 ----
	if len(p.Runlog) > 0 {
		args := make([][]any, 0, len(p.Runlog))
		for _, r := range p.Runlog {
			ts := r.Ts
			if ts == 0 {
				ts = now
			}
			args = append(args, []any{ts, r.Level, r.Msg})
		}
		if _, err := db.bulkUpsert("runlog",
			[]string{"ts", "level", "msg"}, args, nil); err != nil {
			return err
		}
	}

	// ---- 平仓 ----
	if p.CloseTrade != nil {
		ct := p.CloseTrade
		closeTs := ct.CloseTs
		if closeTs == 0 {
			closeTs = now
		}
		if _, err := db.sql.Exec(`UPDATE trade SET exit_px=?, pnl=?, pnl_pct=?, reason=?,
			close_ts=?, ord_id=?, status='closed' WHERE id=?`,
			ct.ExitPx, ct.Pnl, ct.PnlPct, ct.Reason, closeTs, ct.OrdID, ct.ID); err != nil {
			return err
		}
	}
	// ---- 交易事件流水：开仓 / 加仓 / 平仓，一次一笔 ----
	//
	// K 线图上的「买入 / 加仓 / 平仓」标记、历史里的「交易记录详情」都读这张表。
	// 唯一键 (inst_id, kind, ts) + upsert 保证引擎重放同一批事件不会写重。
	if len(p.Event) > 0 {
		args := make([][]any, 0, len(p.Event))
		for _, r := range p.Event {
			ts := r.Ts
			if ts == 0 {
				ts = now
			}
			kind := r.Kind
			if kind == "" {
				kind = "open"
			}
			args = append(args, []any{r.InstID, kind, ts, r.Px, r.Sz, r.Margin,
				r.Leverage, r.Pnl, r.PnlPct, r.Score, r.Reason, r.OrdID, r.TradeID, now})
		}
		if _, err := db.bulkUpsert("trade_event",
			[]string{"inst_id", "kind", "ts", "px", "sz", "margin", "leverage",
				"pnl", "pnl_pct", "score", "reason", "ord_id", "trade_id", "created_at"},
			args,
			[]string{"px", "sz", "margin", "leverage", "pnl", "pnl_pct", "score",
				"reason", "ord_id", "trade_id"}); err != nil {
			return err
		}
	}
	return nil
}

// OpenPositions 读在持仓。对应老的 db.py openpositions。
func (s *Store) OpenPositions() ([]OpenPos, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(`SELECT id,inst_id,sz,entry_px,margin,leverage,open_ts,bar,score,
		COALESCE(ai_note,''),COALESCE(addon_count,0),COALESCE(addon_margin,0),COALESCE(last_addon_ts,0)
		FROM trade WHERE status='open' ORDER BY open_ts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OpenPos{}
	for rows.Next() {
		var p OpenPos
		var bar, note string
		var score sql.NullInt64
		if err := rows.Scan(&p.ID, &p.InstID, &p.Sz, &p.EntryPx, &p.Margin, &p.Leverage,
			&p.OpenTs, &bar, &score, &note, &p.AddonCount, &p.AddonMargin, &p.LastAddonTs); err != nil {
			return nil, err
		}
		p.Bar, p.Score, p.AINote = bar, int(score.Int64), note
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetEntryPx 用 OKX 的**真实持仓均价**覆盖本地开仓价。
//
// 为什么必须校正：下单时本地只能先记一个「乐观初值」——
// trader.go 里写的是信号 K 线的收盘价（`EntryPx: s.Close`）。
// 从信号收盘到市价单真成交往往隔几十秒，流动性差的小币能差 0.5%~0.8%。
// 而止盈判据是 (标记价 ÷ entry_px − 1)，基准价偏低就会「一开仓就假浮盈到止盈线」
// → 立刻市价平掉 → 实际是倒亏手续费（实测 GRASS：开仓 26 秒后平，
// 标记「止盈 +0.79%」，OKX 真实账单 −3.68%）。
//
// ★ 故意**不做 round6** ★：本项目价格跨度极大（BTC 84821 与 0.0000067 的小币同库），
// 保留 6 位小数会把后者的相对误差放大到百分之几，直接污染止盈判据。
// 列本身是 DOUBLE，存原值即可。
//
// 只动 entry_px，**不动 margin** —— margin 是下单前按预算算出来、已参与
// 当日保证金累计与风控口径的，改它会把「占用保证金」这个数弄脏。
func (s *Store) SetEntryPx(instID string, px float64) error {
	if instID == "" || px <= 0 {
		return nil
	}
	db, err := s.open()
	if err != nil {
		return err
	}
	_, err = db.sql.Exec(`UPDATE trade SET entry_px=? WHERE inst_id=? AND status='open'`, px, instID)
	return err
}

// ApplyAddon 把一次加仓「合并」进原持仓行。
//
// 加仓不新开一条持仓记录，就地更新张数 / 加权均价 / 保证金，
// 这样止盈和布林上轨出场不用改也能按新均价算盈亏。
// 同时在 runlog 里留一条痕迹（谁在什么时候补了多少）。
func (s *Store) ApplyAddon(a AddonRow) error {
	db, err := s.open()
	if err != nil {
		return err
	}
	res, err := db.sql.Exec(`UPDATE trade
		SET sz=?, entry_px=?, margin=?, addon_count=?, addon_margin=?, last_addon_ts=?
		WHERE id=? AND status='open'`,
		round6(a.Sz), a.EntryPx, round6(a.Margin), a.AddonCount, round6(a.AddonMargin),
		a.LastAddonTs, a.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("加仓写回失败：持仓 #%d 已不存在或已平仓", a.ID)
	}
	ts := a.LastAddonTs
	if ts <= 0 {
		ts = time.Now().UnixMilli()
	}

	// ★ 写一条加仓流水。
	//
	// trade 表是「一个仓位一行」的合并视图，加仓会把张数/均价/保证金覆盖掉，
	// 所以每次加仓的成交价和金额只有这里留得下来 —— K 线图上的「加仓」标记、
	// 历史里的交易记录详情，全靠这条流水。
	if a.InstID != "" {
		if _, e := db.sql.Exec(`INSERT INTO trade_event
			(inst_id,kind,ts,px,sz,margin,leverage,pnl,pnl_pct,score,reason,ord_id,trade_id,created_at)
			VALUES (?,?,?,?,?,?,?,0,0,0,?,?,?,?)
			ON DUPLICATE KEY UPDATE px=VALUES(px), sz=VALUES(sz), margin=VALUES(margin),
			                        leverage=VALUES(leverage), reason=VALUES(reason), ord_id=VALUES(ord_id)`,
			a.InstID, "addon", ts, a.AddPx, a.AddSz, a.AddMargin, a.Leverage,
			a.Reason, a.OrdID, a.ID, time.Now().UnixMilli()); e != nil {
			return fmt.Errorf("写加仓流水失败：%w", e)
		}
	}

	return s.Ingest(StorePayload{Runlog: []RunLogRow{{
		Ts: ts, Level: "SIGNAL",
		Msg: fmt.Sprintf("加仓 #%d 张数=%s 价格=%.6f 保证金=%.4fU 累计加仓=%d 次/%.4fU 订单=%s 原因=%s",
			a.ID, fmtSz6(a.AddSz), a.AddPx, a.AddMargin, a.AddonCount, a.AddonMargin, a.OrdID, a.Reason),
	}}})
}

// fmtSz6 张数格式化（去掉多余的 0）
func fmtSz6(v float64) string {
	s := strconv.FormatFloat(v, 'f', 8, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	if s == "" || s == "-" {
		return "0"
	}
	return s
}

// Counters 读当日统计。对应老的 db.py counters。
func (s *Store) Counters(dayStartMs int64) (*Counters, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	c := &Counters{LastEntryTs: map[string]int64{}}

	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM trade WHERE open_ts >= ?`, dayStartMs).Scan(&c.OrdersToday); err != nil {
		return nil, err
	}
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM signals WHERE ts >= ?`, dayStartMs).Scan(&c.SignalsToday); err != nil {
		return nil, err
	}

	// 当日平仓：直接 SQL 聚合，比拉回来在 Go 里数快得多
	var wins1, wins2 sql.NullInt64
	if err := db.sql.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(pnl),0), COALESCE(SUM(CASE WHEN pnl>0 THEN 1 ELSE 0 END),0)
		 FROM trade WHERE status='closed' AND close_ts >= ?`, dayStartMs).
		Scan(&c.ClosedToday, &c.TodayPnl, &wins1); err != nil {
		return nil, err
	}
	c.WinsToday = int(wins1.Int64)
	c.TodayPnl = round6(c.TodayPnl)
	_ = wins2

	// 连亏：从最近一笔平仓往前数（只要前 50 笔，量小）
	lrows, err := db.sql.Query(`SELECT COALESCE(pnl,0) FROM trade WHERE status='closed' ORDER BY close_ts DESC LIMIT 50`)
	if err != nil {
		return nil, err
	}
	for lrows.Next() {
		var pnl float64
		if err := lrows.Scan(&pnl); err != nil {
			lrows.Close()
			return nil, err
		}
		if pnl < 0 {
			c.ConsecutiveLosses++
		} else {
			break
		}
	}
	lrows.Close()

	// 每个合约最后一次开仓时间（冷却用）
	eRows, err := db.sql.Query(`SELECT inst_id, MAX(open_ts) FROM trade GROUP BY inst_id`)
	if err != nil {
		return nil, err
	}
	for eRows.Next() {
		var inst string
		var ts sql.NullInt64
		if err := eRows.Scan(&inst, &ts); err != nil {
			eRows.Close()
			return nil, err
		}
		c.LastEntryTs[inst] = ts.Int64
	}
	eRows.Close()

	return c, nil
}

// Cleanup 滚动清理 —— **只做便宜的那一半**。
//
// 调用方是 trader 的「每 20 轮一次」（≈ 20 分钟），所以这里绝不能跑贵的活。
//
// ★ 为什么把 CleanupKlines 从这里摘掉 ★
// CleanupKlines 是「每个 (合约,周期) 定位第 N 根 + 一条分区表 DELETE」，
// 479 合约 × 4 周期 = 1900 多轮，在这个 2 核 2G 的机器上实测要 **99 秒**。
// 挂在 20 分钟一次的路径上 ≈ 每 20 分钟把数据库和 CPU 按住两分钟不放 ——
// 表现就是 K 线迟迟不更新、出场巡检 (live.exitPass) 从 3 秒涨到 26 秒。
//
// K 线裁剪现在只由 service.StartMaintenance 一家负责 —— 但那已经不是
// 「每次顺手裁一遍」，而是 ①写入前 trimToWindow 保证不超窗口、
// ②每年一次的年度任务按 DROP PARTITION 整段扔掉超 365 天的分区。
// 两条路径都不在交易主链上。
//
// PurgeExpired 留着是因为它便宜：每张表一条 `DELETE ... LIMIT 2000`，
// 没超期数据时走索引立刻返回，不会伤到交易循环。
func (s *Store) Cleanup() error {
	db, err := s.open()
	if err != nil {
		return err
	}
	_, err = db.PurgeExpired(RetainDays())
	return err
}

// DB 暴露底层连接（service 层的清理程序 / 诊断工具有时需要直接下 SQL）
func (s *Store) DB() (*DB, error) { return s.open() }

// State 总览（给网页用）
func (s *Store) State() (map[string]interface{}, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	lt := time.Now()
	today0 := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, lt.Location()).UnixMilli()

	q := func(sqlStr string, args ...any) float64 {
		var v sql.NullFloat64
		if err := db.sql.QueryRow(sqlStr, args...).Scan(&v); err != nil {
			return 0
		}
		return v.Float64
	}

	return map[string]interface{}{
		"ok":            true,
		"server_time":   time.Now().Format("2006-01-02 15:04:05"),
		"ts":            time.Now().UnixMilli(),
		"pos_count":     int(q(`SELECT COUNT(*) FROM trade WHERE status='open'`)),
		"signals_today": int(q(`SELECT COUNT(*) FROM signals WHERE ts >= ?`, today0)),
		"orders_today":  int(q(`SELECT COUNT(*) FROM trade WHERE open_ts >= ?`, today0)),
		"trades_total":  int(q(`SELECT COUNT(*) FROM trade WHERE status='closed'`)),
		"today_pnl":     round6(q(`SELECT COALESCE(SUM(pnl),0) FROM trade WHERE status='closed' AND close_ts >= ?`, today0)),
		"pnl_total":     round6(q(`SELECT COALESCE(SUM(pnl),0) FROM trade WHERE status='closed'`)),
	}, nil
}

func round6(v float64) float64 {
	return float64(int64(v*1e6+sign(v)*0.5)) / 1e6
}

func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}
