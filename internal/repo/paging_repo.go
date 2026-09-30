package repo

// paging_repo.go —— 服务端分页
//
// ---------------------------------------------------------------------------
// 为什么分页要下沉到数据库
// ---------------------------------------------------------------------------
// 前端原来是把整批数据（最多 5000 条）拉下来再在浏览器里切片显示。
// 对几百条没问题，但：
//   · 传输量白白多一个数量级（用户一次只看 50 行）；
//   · 页码跳到第 20 页时，前 19 页的数据也全都传了一遍；
//   · JSON 解析本身就要时间，2 核机器上很直观。
//
// 下沉到 `LIMIT ? OFFSET ?` 之后，一次请求只取一页，网络和前端的活都少了。
// 配合 INDEX（见 indexes.go），OFFSET 也能走覆盖索引回表，不用 filesort。
//
// 注意：OFFSET 天生是 O(offset) 的（MySQL 要数过前面那些行）。本项目这些表
// 都在千行量级（trade / signals / trade_event 三天窗口），完全无所谓。
// 真到百万行再换成「游标分页」（WHERE ts < lastTs ORDER BY ts DESC LIMIT n）。

import (
	"fmt"
	"strings"
)

// pageArgs 归一化 limit/offset，防止负数或离谱值把数据库拖死。
func pageArgs(limit, offset int, defLimit, maxLimit int) (int, int) {
	if limit <= 0 {
		limit = defLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// countOf 通用 COUNT：table 与 where 都必须是代码里写死的常量（不接受外部输入），
// 这里仍做一次标识符白名单校验，防止以后有人图方便把用户输入拼进来。
func (d *DB) countOf(table, where string, args ...any) (int64, error) {
	if !safeIdent(table) {
		return 0, fmt.Errorf("非法表名：%s", table)
	}
	q := "SELECT COUNT(*) FROM `" + table + "`"
	if strings.TrimSpace(where) != "" {
		q += " WHERE " + where
	}
	var n int64
	if err := d.sql.QueryRow(q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// trade：历史仓位分页
// ---------------------------------------------------------------------------

// ClosedTradesPage 已平仓列表，按 close_ts 倒序分页。
//
// since>0 时只看 close_ts >= since 的（「最近 3 天」口径）。
func (d *DB) ClosedTradesPage(since int64, limit, offset int) ([]ClosedTrade, error) {
	limit, offset = pageArgs(limit, offset, 50, 2000)
	where := `status='closed'`
	args := []any{}
	if since > 0 {
		where += ` AND close_ts >= ?`
		args = append(args, since)
	}
	args = append(args, limit, offset)
	return d.tradesQuery("SELECT id,inst_id,COALESCE(side,'buy'),sz,entry_px,COALESCE(exit_px,0),"+
		"margin,COALESCE(leverage,0),open_ts,COALESCE(close_ts,0),COALESCE(pnl,0),COALESCE(pnl_pct,0),"+
		"COALESCE(reason,''),COALESCE(bar,''),COALESCE(ai_note,''),COALESCE(status,'closed')"+
		" FROM trade WHERE "+where+" ORDER BY close_ts DESC LIMIT ? OFFSET ?", limit, args...)
}

// ClosedTradesCount 历史仓位总数（同样支持 since 过滤）
func (d *DB) ClosedTradesCount(since int64) (int64, error) {
	if since > 0 {
		return d.countOf("trade", `status='closed' AND close_ts >= ?`, since)
	}
	return d.countOf("trade", `status='closed'`)
}

// ClosedTradesAgg 已平仓仓位的汇总口径（笔数 / 盈亏合计 / 胜笔数）。
//
// 必须覆盖**全部**数据而不是当前页 —— 胜率和累计盈亏是列表的统计口径，
// 只对当前页算出来的数字是错的（下一页会变成另一个胜率）。
func (d *DB) ClosedTradesAgg(since int64) (count int64, sum float64, wins int64, err error) {
	q := `SELECT COUNT(*), COALESCE(SUM(pnl),0), COALESCE(SUM(CASE WHEN pnl>0 THEN 1 ELSE 0 END),0)
	      FROM trade WHERE status='closed'`
	args := []any{}
	if since > 0 {
		q += ` AND close_ts >= ?`
		args = append(args, since)
	}
	err = d.sql.QueryRow(q, args...).Scan(&count, &sum, &wins)
	return
}

// OpenTradesPage 持仓中的仓位分页（历史列表要把它们排在最前面）
func (d *DB) OpenTradesPage(limit, offset int) ([]ClosedTrade, error) {
	limit, offset = pageArgs(limit, offset, 50, 500)
	return d.tradesQuery("SELECT id,inst_id,COALESCE(side,'buy'),sz,entry_px,COALESCE(exit_px,0),"+
		"margin,COALESCE(leverage,0),open_ts,COALESCE(close_ts,0),COALESCE(pnl,0),COALESCE(pnl_pct,0),"+
		"COALESCE(reason,''),COALESCE(bar,''),COALESCE(ai_note,''),COALESCE(status,'closed')"+
		" FROM trade WHERE status='open' ORDER BY open_ts DESC LIMIT ? OFFSET ?", limit, limit, offset)
}

// tradesQuery 执行 trades 系列查询（SQL 与参数都由上面的函数给死）
func (d *DB) tradesQuery(query string, cap int, args ...any) ([]ClosedTrade, error) {
	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ClosedTrade, 0, cap)
	for rows.Next() {
		var t ClosedTrade
		if err := rows.Scan(&t.ID, &t.InstID, &t.Side, &t.Sz, &t.EntryPx, &t.ExitPx,
			&t.Margin, &t.Leverage, &t.OpenTs, &t.CloseTs, &t.Pnl, &t.PnlPct,
			&t.Reason, &t.Bar, &t.AINote, &t.Status); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// signals：信号分页
// ---------------------------------------------------------------------------

// SignalsPage 信号列表分页（按 ts 倒序）
func (d *DB) SignalsPage(limit, offset int) ([]SignalRow, error) {
	limit, offset = pageArgs(limit, offset, 50, 2000)
	return d.signalsQuery("SELECT id,inst_id,bar,ts,COALESCE(close,0),COALESCE(mask,0),"+
		"COALESCE(score,0),COALESCE(hit_list,''),COALESCE(rsi,0),COALESCE(td,0),"+
		"COALESCE(acted,0),COALESCE(reason,''),COALESCE(ai_note,''),COALESCE(created_at,0)"+
		" FROM signals ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?", limit, limit, offset)
}

// SignalsCount 信号总数
func (d *DB) SignalsCount() (int64, error) {
	return d.countOf("signals", "")
}

// SignalsByInstPage 某合约的信号分页
func (d *DB) SignalsByInstPage(instID string, limit, offset int) ([]SignalRow, error) {
	limit, offset = pageArgs(limit, offset, 50, 2000)
	return d.signalsQuery("SELECT id,inst_id,bar,ts,COALESCE(close,0),COALESCE(mask,0),"+
		"COALESCE(score,0),COALESCE(hit_list,''),COALESCE(rsi,0),COALESCE(td,0),"+
		"COALESCE(acted,0),COALESCE(reason,''),COALESCE(ai_note,''),COALESCE(created_at,0)"+
		" FROM signals WHERE inst_id=? ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?",
		limit, instID, limit, offset)
}

func (d *DB) signalsQuery(query string, cap int, args ...any) ([]SignalRow, error) {
	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SignalRow, 0, cap)
	for rows.Next() {
		var s SignalRow
		if err := rows.Scan(&s.ID, &s.InstID, &s.Bar, &s.Ts, &s.Close, &s.Mask,
			&s.Score, &s.HitList, &s.Rsi, &s.Td, &s.Acted, &s.Reason, &s.AINote, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
