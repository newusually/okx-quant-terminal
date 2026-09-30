package repo

// catalog_repo.go —— 合约静态信息 + 实时行情快照（MySQL 版）
//
// 这两张表都是「每轮全量刷新」的形态：
//   inst   —— 480 个 USDT-SWAP 合约，10 分钟刷一次
//   ticker —— 每 5 秒刷一轮 480 行
// 都用多行 upsert，一轮一次网络往返。

import (
	"fmt"
	"strings"
	"time"
)

// instCols inst 表列序
var instCols = []string{
	"inst_id", "base_ccy", "quote_ccy", "settle_ccy", "ct_val", "ct_mult",
	"lot_sz", "min_sz", "tick_sz", "lever", "state", "list_time", "quote_vol24h", "inst_category", "updated_at",
	"tradeable", "exclude_reason",
}

// instUpdateCols 是「合约同步时允许覆盖」的列。
//
// ★ tradeable / exclude_reason 故意不在这里：
//
//	它们由准入过滤（service.FilterUniverse → UpdateTradeable）单独写，
//	如果让 10 分钟一轮的合约同步也去更新，就会把过滤结果冲回默认值 0，
//	表现是「日志说 171 个可交易，但列表里一个都没有」。
var instUpdateCols = []string{
	"base_ccy", "quote_ccy", "settle_ccy", "ct_val", "ct_mult",
	"lot_sz", "min_sz", "tick_sz", "lever", "state", "list_time", "quote_vol24h", "inst_category", "updated_at",
}

// tickerCols ticker 表列序
var tickerCols = []string{
	"inst_id", "ts", "last", "open24h", "high24h", "low24h",
	"vol24h", "vol_ccy24h", "quote_vol24h", "chg_pct",
}

var tickerUpdateCols = []string{
	"ts", "last", "open24h", "high24h", "low24h",
	"vol24h", "vol_ccy24h", "quote_vol24h", "chg_pct",
}

// ---------------------------------------------------------------------------
// inst
// ---------------------------------------------------------------------------

// UpsertInstruments 写入 / 刷新合约静态信息
func (d *DB) UpsertInstruments(list []Instrument) (int, error) {
	if len(list) == 0 {
		return 0, nil
	}
	now := time.Now().UnixMilli()
	args := make([][]any, 0, len(list))
	for _, it := range list {
		args = append(args, []any{
			it.InstID, it.BaseCcy, it.QuoteCcy, it.SettleCcy,
			it.CtVal, it.CtMult, it.LotSz, it.MinSz, it.TickSz,
			it.Lever, it.State, it.ListTime, it.QuoteVol24h, it.InstCategory, now,
			it.Tradeable, it.ExcludeReason,
		})
	}
	return d.bulkUpsert("inst", instCols, args, instUpdateCols)
}

// ListInstruments 列出所有合约（按 24h 成交额降序），供前端下拉框
func (d *DB) ListInstruments() ([]Instrument, error) {
	rows, err := d.sql.Query(`SELECT inst_id,base_ccy,quote_ccy,settle_ccy,ct_val,ct_mult,
		lot_sz,min_sz,tick_sz,lever,state,list_time,quote_vol24h,COALESCE(inst_category,''),updated_at,
		COALESCE(tradeable,0),COALESCE(exclude_reason,'')
		FROM inst ORDER BY tradeable DESC, quote_vol24h DESC, inst_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Instrument, 0, 512)
	for rows.Next() {
		var it Instrument
		if err := rows.Scan(&it.InstID, &it.BaseCcy, &it.QuoteCcy, &it.SettleCcy,
			&it.CtVal, &it.CtMult, &it.LotSz, &it.MinSz, &it.TickSz, &it.Lever,
			&it.State, &it.ListTime, &it.QuoteVol24h, &it.InstCategory, &it.UpdatedAt,
			&it.Tradeable, &it.ExcludeReason); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// UpdateVolumes 只更新 24h 成交额（下拉框热度排序用），不覆盖其它字段
//
// 一次只刷几百行、10 分钟一轮，走单条 UPDATE 完全够；
// 用 CASE WHEN 拼批量反而容易踩 max_allowed_packet。
// UpdateVolumes 批量回写 24h 成交额（inst.quote_vol24h）
//
// ★ 性能修复（2026-10-01）：
//
//	老实现 = 开事务 + Prepare + 逐行 Exec，480 行就是 **480 次客户端↔服务端往返**；
//	而 inst 上还有两个二级索引（ix_inst_tradeable_vol / ix_inst_category_vol），
//	每行都要维护 2 条索引 → 一轮近千次索引写。
//	实测这一项让 SyncTickers 平均耗时 3.1 秒（perf 计数器），而它每 5 秒就要跑一次。
//
//	现在改成**一条 SQL 更新一批**：把 480 行做成派生表 JOIN 上去，
//	往返次数从 480 降到 3（每批 200 行）。索引维护量不变，但网络/解析开销没了。
//
// 另外调用方已把节奏从 5 秒放宽到 60 秒（见 backfill.go realtimeLoop）：
// 成交额是给「按热度排序」和「成交额 <100 万不买」用的，不需要 5 秒级新鲜度。
func (d *DB) UpdateVolumes(list []Instrument) error {
	if len(list) == 0 {
		return nil
	}
	now := time.Now().UnixMilli()
	const bs = 200 // 每批行数：200×2+1 = 401 个占位符，SQL 文本约 6KB
	for start := 0; start < len(list); start += bs {
		end := start + bs
		if end > len(list) {
			end = len(list)
		}
		chunk := list[start:end]

		var sb strings.Builder
		sb.Grow(64 + len(chunk)*40)
		sb.WriteString("UPDATE inst i JOIN (")
		args := make([]any, 0, len(chunk)*2+1)
		for i, it := range chunk {
			if i > 0 {
				sb.WriteString(" UNION ALL ")
			}
			sb.WriteString("SELECT ? AS inst_id, ? AS v")
			args = append(args, it.InstID, it.QuoteVol24h)
		}
		sb.WriteString(") t ON i.inst_id=t.inst_id SET i.quote_vol24h=t.v, i.updated_at=?")
		args = append(args, now)

		if _, err := d.sql.Exec(sb.String(), args...); err != nil {
			return fmt.Errorf("批量回写成交额失败（第 %d~%d 行）：%w", start, end-1, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// ticker
// ---------------------------------------------------------------------------

// UpsertTickers 更新实时行情快照（每 5 秒一轮，480 行一次写完）
func (d *DB) UpsertTickers(list []Ticker) (int, error) {
	if len(list) == 0 {
		return 0, nil
	}
	args := make([][]any, 0, len(list))
	for _, t := range list {
		args = append(args, []any{
			t.InstID, t.Ts, t.Last, t.Open24h, t.High24h, t.Low24h,
			t.Vol24h, t.VolCcy24h, t.QuoteVol24h, t.ChgPct,
		})
	}
	return d.bulkUpsert("ticker", tickerCols, args, tickerUpdateCols)
}

// ListTickers 列出全部行情快照
func (d *DB) ListTickers() ([]Ticker, error) {
	rows, err := d.sql.Query(`SELECT inst_id,ts,last,open24h,high24h,low24h,vol24h,vol_ccy24h,
		quote_vol24h,chg_pct FROM ticker`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Ticker, 0, 512)
	for rows.Next() {
		var t Ticker
		if err := rows.Scan(&t.InstID, &t.Ts, &t.Last, &t.Open24h, &t.High24h, &t.Low24h,
			&t.Vol24h, &t.VolCcy24h, &t.QuoteVol24h, &t.ChgPct); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpdateTradeable 批量刷新「是否可交易」与排除原因
//
// 准入过滤在 service 层算好，这里只负责落库。478 行单事务，几十毫秒。
func (d *DB) UpdateTradeable(rows []Instrument) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(
		`UPDATE inst SET tradeable=?, exclude_reason=? WHERE inst_id=?`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, it := range rows {
		if _, err := stmt.Exec(it.Tradeable, it.ExcludeReason, it.InstID); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// ListTradeableInstruments 只列可交易的合约（按成交额降序）
func (d *DB) ListTradeableInstruments() ([]Instrument, error) {
	rows, err := d.sql.Query(`SELECT inst_id,base_ccy,quote_ccy,settle_ccy,ct_val,ct_mult,
		lot_sz,min_sz,tick_sz,lever,state,list_time,quote_vol24h,COALESCE(inst_category,''),updated_at,
		COALESCE(tradeable,0),COALESCE(exclude_reason,'')
		FROM inst WHERE tradeable=1 ORDER BY quote_vol24h DESC, inst_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Instrument, 0, 256)
	for rows.Next() {
		var it Instrument
		if err := rows.Scan(&it.InstID, &it.BaseCcy, &it.QuoteCcy, &it.SettleCcy,
			&it.CtVal, &it.CtMult, &it.LotSz, &it.MinSz, &it.TickSz, &it.Lever,
			&it.State, &it.ListTime, &it.QuoteVol24h, &it.InstCategory, &it.UpdatedAt,
			&it.Tradeable, &it.ExcludeReason); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}
