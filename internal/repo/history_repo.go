package repo

// history_repo.go —— OKX 历史仓位（已平仓）落库
//
// ---------------------------------------------------------------------------
// 背景
// ---------------------------------------------------------------------------
// trade 表原来只有引擎自己下的单。用户实测：OKX 账户上有 100+ 个已平仓仓位、
// 8000+ 笔成交，本地历史面板却只有 8 行 —— 「历史仓位根本不够看」。
//
// 这个文件提供把 /api/v5/account/positions-history 的结果 upsert 进 trade 的能力。
//
// ---------------------------------------------------------------------------
// 幂等怎么做的（三级匹配，顺序不能换）
// ---------------------------------------------------------------------------
//  ① (pos_id, close_ts) 命中 → 同一个 OKX 平仓事件，直接 UPDATE 财务字段
//  ② 合约 + 开仓时间          → 引擎自己开的那个仓位（引擎拿不到 posId，pos_id 为空），
//                             认领它：补上 pos_id 和真实平仓数据，但**保留引擎写的
//                             reason / bar / ai_note**（「止盈 +1.02%」这种话比
//                             「OKX 历史同步」有用得多）
//  ③ 都没命中                → 新增一行
//
// **为什么幂等键里必须带 close_ts**：OKX 的 posId 不是「一个仓位一个 ID」，
// 而是按 (合约, 方向, 保证金模式) 分配的，平掉之后再开仓拿到的还是同一个
// posId。实测 ETH-USDT-SWAP 一个 posId 下挂了 117 笔互不相干的交易
// （开仓价从 1718 到 2733 都有）。只用 posId 当键 → 后一笔把前一笔覆盖掉，
// 面板上就永远只有 1 行。带上平仓时间（uTime）才是「一次平仓一行」。
//
// 时间匹配的容差取 15 分钟：OKX 的 cTime 是服务器撮合成交时间，
// 引擎写库用的是本地扫描时间，两者差几秒到几分钟都正常。
// 容差太小会认领失败产生重复行，太大则可能把两个相邻仓位认成同一个。
//
// ---------------------------------------------------------------------------
// 为什么先在内存里建索引，而不是逐行 SELECT
// ---------------------------------------------------------------------------
// 一次同步 100~200 个仓位，逐行 SELECT 就是 200 次往返；
// trade 表统共几千行，一次全读进来的成本远低于 200 次网络往返。

import (
	"database/sql"
	"fmt"
	"strconv"
	"time"
)

// HistoryTrade 一次从 OKX 同步回来的平仓事件
type HistoryTrade struct {
	PosID    string  // OKX posId（**会重复**，幂等键要连 CloseTs 一起看）
	InstID   string  // 合约
	Side     string  // buy / sell
	Sz       float64 // 张数（closeTotalPos）
	EntryPx  float64 // 开仓均价
	ExitPx   float64 // 平仓均价
	Margin   float64 // 保证金（由名义价值 ÷ 杠杆反算，OKX 不直接给）
	Leverage int     // 杠杆
	OpenTs   int64   // 开仓时间（ms）
	CloseTs  int64   // 平仓时间（ms）
	Pnl      float64 // 已实现盈亏（含手续费）
	PnlPct   float64 // 收益率（%）
	Reason   string  // 平仓原因（人话）
	Bar      string  // 该仓位对应的周期（留空，历史同步不知道）
	AINote   string
}

// histKey 历史仓位的天然主键：posId + 平仓毫秒时间。
// 不能只用 posId —— 理由见文件头「幂等」那一段。
func histKey(posID string, closeTs int64) string {
	return posID + "|" + strconv.FormatInt(closeTs, 10)
}

// UpsertHistoryTradesResult 一次 upsert 的结果
type UpsertHistoryTradesResult struct {
	Inserted int // 新增
	Adopted  int // 认领了引擎已有的行（把 pos_id 补上）
	Updated  int // 按 pos_id 覆盖更新
}

// openTradeRef trade 表里一行「已有记录」的定位信息
type openTradeRef struct {
	ID      int64
	InstID  string
	OpenTs  int64
	Claimed bool // 已经有 pos_id（被同步认领过）→ 不能再被别人认领
}

// tradeMatchTolerance 引擎行认领的时间容差（毫秒）：15 分钟
const tradeMatchTolerance int64 = 15 * 60 * 1000

// UpsertHistoryTrades 把一批 OKX 历史仓位写进 trade 表。
//
// 单条失败不中断整批（记进 errs 由调用方打日志），因为一个脏字段
// 不该让整次同步白跑。
func (d *DB) UpsertHistoryTrades(rows []HistoryTrade) (UpsertHistoryTradesResult, []string, error) {
	var res UpsertHistoryTradesResult
	if len(rows) == 0 {
		return res, nil, nil
	}

	// ---- 1. 一次性把 trade 表的定位信息读进内存 ----
	byP := map[string]int64{}             // posId|close_ts -> id（幂等查找）
	byInst := map[string][]openTradeRef{} // inst_id -> 候选行（时间匹配用）
	{
		qr, err := d.sql.Query(`SELECT id, inst_id, COALESCE(open_ts,0),
			COALESCE(pos_id,''), COALESCE(close_ts,0) FROM trade`)
		if err != nil {
			return res, nil, fmt.Errorf("读 trade 索引失败：%w", err)
		}
		for qr.Next() {
			var id, ts, cts int64
			var inst, pid string
			if err := qr.Scan(&id, &inst, &ts, &pid, &cts); err != nil {
				qr.Close()
				return res, nil, err
			}
			if pid != "" {
				byP[histKey(pid, cts)] = id
			}
			byInst[inst] = append(byInst[inst], openTradeRef{
				ID: id, InstID: inst, OpenTs: ts, Claimed: pid != "",
			})
		}
		qr.Close()
	}

	var errs []string
	for _, r := range rows {
		if r.InstID == "" || r.PosID == "" {
			continue
		}
		key := histKey(r.PosID, r.CloseTs)
		// ---- ① (pos_id, close_ts) 命中 → 覆盖更新 ----
		if id, ok := byP[key]; ok {
			if err := d.updateHistoryTrade(id, r); err != nil {
				errs = append(errs, fmt.Sprintf("%s#%s 更新失败：%v", r.InstID, r.PosID, err))
			} else {
				res.Updated++
			}
			continue
		}
		// ---- ② 合约 + 开仓时间 命中且还没被认领 → 认领 ----
		if id, ok := pickUnclaimed(byInst[r.InstID], r.OpenTs); ok {
			if err := d.updateHistoryTrade(id, r); err != nil {
				errs = append(errs, fmt.Sprintf("%s#%s 认领失败：%v", r.InstID, r.PosID, err))
			} else {
				byP[key] = id
				res.Adopted++
			}
			continue
		}
		// ---- ③ 新增 ----
		id, err := d.insertHistoryTrade(r)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s#%s 新增失败：%v", r.InstID, r.PosID, err))
			continue
		}
		byP[key] = id
		byInst[r.InstID] = append(byInst[r.InstID], openTradeRef{
			ID: id, InstID: r.InstID, OpenTs: r.OpenTs, Claimed: true,
		})
		res.Inserted++
	}
	return res, errs, nil
}

// pickUnclaimed 在候选行里找一条「开仓时间最接近、且还没被同步认领过」的。
func pickUnclaimed(cands []openTradeRef, openTs int64) (int64, bool) {
	var bestID int64
	bestDiff := tradeMatchTolerance + 1
	for _, c := range cands {
		if c.Claimed || c.OpenTs <= 0 || openTs <= 0 {
			continue
		}
		diff := c.OpenTs - openTs
		if diff < 0 {
			diff = -diff
		}
		if diff > tradeMatchTolerance {
			continue
		}
		if diff < bestDiff {
			bestDiff, bestID = diff, c.ID
		}
	}
	if bestID == 0 {
		return 0, false
	}
	return bestID, true
}

// updateHistoryTrade 把 OKX 的真实平仓数据写到 id 上。
//
// reason / bar / ai_note 的处理：
//
//	引擎自己写的 reason（如「止盈 +1.02%」）更有信息量，同步**不覆盖**它；
//	只有原本为空、或原本就是同步写的那种模板话术时才换掉。
func (d *DB) updateHistoryTrade(id int64, r HistoryTrade) error {
	_, err := d.sql.Exec(`UPDATE trade SET
			pos_id=?, side=?, sz=?, entry_px=?, exit_px=?, margin=?, leverage=?,
			open_ts=CASE WHEN ? > 0 THEN ? ELSE open_ts END,
			close_ts=CASE WHEN ? > 0 THEN ? ELSE close_ts END,
			pnl=?, pnl_pct=?,
			reason=CASE WHEN reason='' OR reason LIKE 'OKX 历史同步%' THEN ? ELSE reason END,
			status='closed',
			bar=CASE WHEN COALESCE(bar,'')='' THEN ? ELSE bar END
		WHERE id=?`,
		r.PosID, r.Side, r.Sz, r.EntryPx, r.ExitPx, r.Margin, r.Leverage,
		r.OpenTs, r.OpenTs, r.CloseTs, r.CloseTs,
		r.Pnl, r.PnlPct, r.Reason, r.Bar, id)
	return err
}

// insertHistoryTrade 新增一行历史仓位
func (d *DB) insertHistoryTrade(r HistoryTrade) (int64, error) {
	note := r.AINote
	if note == "" {
		note = "OKX 历史仓位同步"
	}
	res, err := d.sql.Exec(`INSERT INTO trade
		(inst_id, side, sz, entry_px, exit_px, margin, leverage, open_ts, close_ts,
		 pnl, pnl_pct, reason, ord_id, pos_id, status, score, bar, ai_note,
		 addon_count, addon_margin, last_addon_ts)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,'',?,'closed',0,?,?,0,0,0)`,
		r.InstID, r.Side, r.Sz, r.EntryPx, r.ExitPx, r.Margin, r.Leverage,
		r.OpenTs, r.CloseTs, r.Pnl, r.PnlPct, r.Reason, r.PosID, r.Bar, note)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// HistoryTradeCount 当前库里有多少条「从 OKX 同步来的」历史仓位。
// 给同步日志和网页自查用（判断补录到底有没有生效）。
func (d *DB) HistoryTradeCount() (total int64, synced int64, err error) {
	row := d.sql.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN COALESCE(pos_id,'')<>'' THEN 1 ELSE 0 END),0) FROM trade`)
	err = row.Scan(&total, &synced)
	return
}

// EarliestTradeTs 库里最早 / 最晚的已平仓时间（给日志显示覆盖范围）
func (d *DB) TradeTimeSpan() (minTs, maxTs int64, err error) {
	var mn, mx sql.NullInt64
	err = d.sql.QueryRow(`SELECT MIN(close_ts), MAX(close_ts) FROM trade WHERE status='closed'`).Scan(&mn, &mx)
	if err != nil {
		return 0, 0, err
	}
	return mn.Int64, mx.Int64, nil
}

// FormatTs 毫秒时间戳 → "2006-01-02 15:04"（本地时区）
func FormatTs(ms int64) string {
	if ms <= 0 {
		return "--"
	}
	return time.UnixMilli(ms).Format("2006-01-02 15:04")
}
