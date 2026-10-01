package service

// okxpositions.go —— 把 OKX 的「已平仓仓位历史」同步进本地 trade 表
//
// ---------------------------------------------------------------------------
// 为什么需要它（用户原话：「历史仓位根本不够看，只有 8 条记录」）
// ---------------------------------------------------------------------------
// trade 表原来只记**程序自己下的单**。用户实测核对下来：
//
//	OKX 账户实际：100+ 个已平仓仓位、8000+ 笔成交（可回溯到 3 个月前）
//	本地 trade  ：8 行
//
// okxhistory.go 那个 fills 同步只写 trade_event（逐笔流水），
// 而「历史仓位」面板读的是 trade（一个仓位一行）—— 所以对不上。
//
// 这里补的就是这块：拉 /api/v5/account/positions-history，
// 一个仓位一行 upsert 进 trade。
//
// ---------------------------------------------------------------------------
// 接口口径（2026-10-01 实测，踩过的坑都写在这里）
// ---------------------------------------------------------------------------
//	GET /api/v5/account/positions-history?instType=SWAP&limit=100[&after=<毫秒时间戳>]
//
//	★ 分页游标是**毫秒时间戳**，不是 posId！★
//	  实测 after=<posId> → {"code":"51000","msg":"Parameter after error"}
//	  网上很多示例写 after=posId，在本接口上是错的。
//	  after=<ms> 的语义 = 「返回平仓时间早于该毫秒的下一批」。
//
//	★ limit 只认最大值 100 ★：传 limit=20 依然返回 100 条。
//	★ begin / end / startTime / endTime 全部被静默忽略 ★：
//	  传 2026-04 的窗口，返回的还是最近那 100 条 —— 别指望用它切片。
//	★ 不传 type 时默认给「完全平仓」为主（实测首页 100 条 type 全是 2，
//	  翻到历史里会混进 type=3 爆仓）；type=1 返回 0 条，所以不要传 type。
//
//	· 关键字段：
//	    posId          仓位 ID —— **会重复！** OKX 按 (合约, 方向, 保证金模式)
//	                   分配 posId，平掉之后再开仓还是同一个 posId。
//	                   实测 ETH-USDT-SWAP 一个 posId 下挂了 117 笔独立交易，
//	                   所以幂等键必须是 posId + 平仓时间（uTime），不是 posId 本身。
//	    cTime / uTime  开仓 / 平仓时间（毫秒）
//	    openAvgPx      开仓均价     closeAvgPx    平仓均价
//	    closeTotalPos  平仓张数
//	    realizedPnl    已实现盈亏（含手续费，USDT）—— 这是「真金白银」那个数
//	    pnlRatio       收益率（OKX 口径，小数）
//	    lever          杠杆
//	    type           1=部分平仓 2=完全平仓 3=强制平仓(爆仓) 4/5/6=3 个月前的归档
//
//	· 真实存量（2026-10-01 全量遍历实测）：
//	    不传 type   6000+ 行 / 560 个 posId / 2026-03-26 起（60 页才打满）
//	    type=3      112 行 / 98 个 posId / 2024-12-09 起
//	    最近 30 天   711 行 / 209 个 posId  ← 本地保留窗口内应有这个量级
//
//	· OKX **不返回保证金**，得自己按 名义价值 ÷ 杠杆 反算：
//	    名义价值 = openAvgPx × closeTotalPos × ctVal × ctMult
//	  ctVal/ctMult 从本地 inst 表拿（和 trade_event 完全同一套口径，
//	  不然 SAND 这种一张=10 币的会小一个数量级、BTC 反而大 100 倍）。

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"finally-main/internal/conf"
	"finally-main/internal/logx"
	"finally-main/internal/repo"
)

const okxPositionsHistoryPath = "/api/v5/account/positions-history"

// 同步间隔：10 分钟。仓位历史只在平仓时新增，10 分钟足够及时；
// 这个接口限频 5 次/2 秒，单次同步 8 页左右，完全在预算内。
const okxPositionsSyncInterval = 10 * time.Minute

// 单次同步最多翻几页（每页 100 条）。
// 本地只留 30 天 ≈ 711 行 ≈ 8 页，给到 20 页（2000 行 ≈ 84 天）是冗余量；
// 真正让它停下来的是下面那个时间下界，不是页数。
const okxPositionsMaxPages = 20

// 往回翻多少天 = 保留窗口本身（RetainDays()，默认 30 天）。
//
// ★ 这里曾经写死 35 天（「比窗口多一点，保证面板是满的」），结果是
//   30~35 天那一档的行每轮同步都被写进来、每周又被 PurgeExpired 删掉，
//   清理日志永远显示「trade 删了 N 行」，看着像没清干净。
//   下界直接对齐红线：多翻无收益，只会制造 churn。
func okxPositionsLookbackDays() int {
	return repo.RetainDays()
}

// OKXPositionHistory 一笔已平仓仓位（OKX 原始字段）
type OKXPositionHistory struct {
	InstID        string `json:"instId"`
	PosID         string `json:"posId"`
	PosSide       string `json:"posSide"`
	Direction     string `json:"direction"`
	CTime         string `json:"cTime"`
	UTime         string `json:"uTime"`
	OpenAvgPx     string `json:"openAvgPx"`
	CloseAvgPx    string `json:"closeAvgPx"`
	CloseTotalPos string `json:"closeTotalPos"`
	RealizedPnl   string `json:"realizedPnl"`
	Pnl           string `json:"pnl"`
	PnlRatio      string `json:"pnlRatio"`
	Lever         string `json:"lever"`
	Type          string `json:"type"`
	Fee           string `json:"fee"`
	MgnMode       string `json:"mgnMode"`
}

// FetchOKXPositionsHistory 向前翻页拉已平仓仓位（最多 maxPages 页）。
//
// 【游标是毫秒时间戳，不是 posId】—— 详见文件头「接口口径」。
// 每页拿本批最早的 uTime，减 1 毫秒当下一次的 after，一路往更早走。
//
// sinceMs > 0 时，某页最早时间早于它就收手：本地只留 30 天，
// 没必要把 18 个月、6000+ 行全拖回来（那要 60 次请求，每次同步都白跑）。
//
// **注意命令替换的 \r 陷阱**：Windows 上从命令行/脚本拼进来的字符串
// 尾部可能带 \r，拼进 URL 后 OKX 会静默忽略非法参数并返回第一页 ——
// 表现就是「翻页永远翻不动」。所以这里统一 TrimSpace。
func FetchOKXPositionsHistory(cli *OKXClient, maxPages int, sinceMs int64) ([]OKXPositionHistory, error) {
	if maxPages <= 0 {
		maxPages = okxPositionsMaxPages
	}
	out := make([]OKXPositionHistory, 0, maxPages*100)
	seen := map[string]bool{}
	cursor := time.Now().UnixMilli()

	for i := 0; i < maxPages; i++ {
		path := fmt.Sprintf("%s?instType=SWAP&limit=100&after=%d", okxPositionsHistoryPath, cursor)
		raw, err := cli.Get(path, true)
		if err != nil {
			if i == 0 {
				return nil, err
			}
			break // 翻页中途失败就用已经拿到的
		}

		var rows []OKXPositionHistory
		if err := json.Unmarshal(raw, &rows); err != nil {
			// 有些接口会把 data 包一层，兜一次
			var wrap struct {
				Data []OKXPositionHistory `json:"data"`
			}
			if e2 := json.Unmarshal(raw, &wrap); e2 != nil || len(wrap.Data) == 0 {
				return out, fmt.Errorf("解析仓位历史失败：%w", err)
			}
			rows = wrap.Data
		}
		if len(rows) == 0 {
			break
		}

		oldest := int64(0)
		for _, r := range rows {
			pid, ut := stringTrim(r.PosID), stringTrim(r.UTime)
			if pid == "" || ut == "" {
				continue
			}
			t := atoi64(ut)
			// 游标靠本批最早时间往更早走，所以 oldest 要先算（哪怕这行要丢）。
			if t > 0 && (oldest == 0 || t < oldest) {
				oldest = t
			}
			// ★ 窗口外的行直接丢，不要 append。
			// OKX 的 after= 语义是「平仓时间早于该毫秒的下一批」，所以
			// 「翻过下界就 break」的那个判断天然发生在 append 之后 ——
			// 每一轮同步都会多带回来一整页（最多 100 行、可横跨十几天）。
			// 不在这里挡住，「只保留 30 天」就会被撑成 49 天。
			if sinceMs > 0 && t > 0 && t < sinceMs {
				continue
			}
			// posId 会重复（OKX 按 (合约,方向) 复用），幂等键必须是
			// posId + 平仓时间；同一 key 在跨页时也可能重出，本地先去一次重。
			key := pid + "|" + ut
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, r)
		}
		if oldest == 0 {
			break
		}
		if sinceMs > 0 && oldest < sinceMs {
			break // 已经翻过时间下界
		}
		if len(rows) < 100 {
			break // 不够一页 = 到底了
		}
		next := oldest - 1
		if next >= cursor {
			break // 游标没前进，防死循环
		}
		cursor = next
	}
	return out, nil
}

// PositionHistoryToTrade 把 OKX 的仓位历史翻译成本地 trade 行。
//
// ins 是本地合约信息表（拿 ctVal / ctMult 算名义价值和保证金）。
func PositionHistoryToTrade(h OKXPositionHistory, ins map[string]Instrument) repo.HistoryTrade {
	ctVal, ctMult := 1.0, 1.0
	if it, ok := ins[h.InstID]; ok {
		if it.CtVal > 0 {
			ctVal = it.CtVal
		}
		if it.CtMult > 0 {
			ctMult = it.CtMult
		}
	}
	lever := int(atof(h.Lever))
	if lever <= 0 {
		lever = 20
	}
	sz := atof(h.CloseTotalPos)
	openPx := atof(h.OpenAvgPx)
	closePx := atof(h.CloseAvgPx)
	pnl := atof(h.RealizedPnl)

	// 保证金 = 名义价值 ÷ 杠杆（OKX 不直接给）
	notional := openPx * sz * ctVal * ctMult
	margin := 0.0
	if lever > 0 {
		margin = notional / float64(lever)
	}
	// 收益率优先用「已实现盈亏 ÷ 保证金」，和本地其它行完全同口径；
	// 保证金算不出来时才退到 OKX 的 pnlRatio。
	pnlPct := 0.0
	if margin > 1e-12 {
		pnlPct = pnl / margin * 100
	} else {
		pnlPct = atof(h.PnlRatio) * 100
	}

	side := "buy"
	if h.PosSide == "short" || h.Direction == "short" || h.Direction == "sell" {
		side = "sell"
	}

	return repo.HistoryTrade{
		PosID:    stringTrim(h.PosID),
		InstID:   h.InstID,
		Side:     side,
		Sz:       sz,
		EntryPx:  openPx,
		ExitPx:   closePx,
		Margin:   margin,
		Leverage: lever,
		OpenTs:   atoi64(stringTrim(h.CTime)),
		CloseTs:  atoi64(stringTrim(h.UTime)),
		Pnl:      pnl,
		PnlPct:   pnlPct,
		Reason:   positionHistoryReason(h),
	}
}

// positionHistoryReason 把 OKX 的 type 字段翻译成人话（写进 trade.reason）
func positionHistoryReason(h OKXPositionHistory) string {
	switch stringTrim(h.Type) {
	case "3", "6":
		return "OKX 历史同步 · 强制平仓（爆仓）"
	case "1", "4":
		return "OKX 历史同步 · 部分平仓"
	default:
		return "OKX 历史同步 · 完全平仓"
	}
}

// SyncOKXPositionsHistory 同步一次，返回写入条数。
func SyncOKXPositionsHistory(cli *OKXClient, store *repo.Store) (repo.UpsertHistoryTradesResult, error) {
	var res repo.UpsertHistoryTradesResult

	rows, err := FetchOKXPositionsHistory(cli, okxPositionsMaxPages,
		time.Now().AddDate(0, 0, -okxPositionsLookbackDays()).UnixMilli())
	if err != nil {
		return res, err
	}
	if len(rows) == 0 {
		return res, nil
	}

	// 合约信息（ctVal/ctMult）。拿不到就退化成 1，数字会偏但不至于崩。
	ins := map[string]Instrument{}
	if m, e := cli.Instruments(false); e == nil && m != nil {
		ins = m
	}

	// 老的排前面，让 INSERT 顺序和真实时间顺序一致（id 递增 = 时间递增，
	// 翻页/排序看起来更自然）。
	sort.SliceStable(rows, func(i, j int) bool {
		return atoi64(stringTrim(rows[i].UTime)) < atoi64(stringTrim(rows[j].UTime))
	})

	list := make([]repo.HistoryTrade, 0, len(rows))
	for _, r := range rows {
		if r.InstID == "" || stringTrim(r.PosID) == "" {
			continue
		}
		list = append(list, PositionHistoryToTrade(r, ins))
	}

	db, err := store.DB()
	if err != nil {
		return res, err
	}
	res, errs, err := db.UpsertHistoryTrades(list)
	for _, e := range errs {
		logx.Logf("WARN", "[POSHIST] %s", e)
	}
	return res, err
}

// StartOKXPositionsSync 后台定时同步（和 fills 同步一样，跟自动交易开关无关）。
//
// 启动后 35 秒先同步一次（错开回补 / 信号回算的高峰），之后每 10 分钟一次。
// 幂等：重复拉同一批仓位只会 UPDATE，不会写出重复行。
func StartOKXPositionsSync(ctx context.Context) {
	go func() {
		time.Sleep(35 * time.Second)
		for {
			syncOKXPositionsOnce()
			select {
			case <-ctx.Done():
				return
			case <-time.After(okxPositionsSyncInterval):
			}
		}
	}()
}

// syncOKXPositionsOnce 同步一次（失败只记日志，不中断循环）
func syncOKXPositionsOnce() {
	cfg := conf.LoadConfig()
	if cfg == nil || !cfg.Enabled {
		return
	}
	cli, err := eng.client(cfg)
	if err != nil || cli == nil {
		return
	}
	if err := cli.EnsureReady(); err != nil {
		return
	}
	st := repo.NewStore(cfg)
	res, err := SyncOKXPositionsHistory(cli, st)
	if err != nil {
		logx.Logf("WARN", "[POSHIST] 仓位历史同步失败：%v", err)
		return
	}
	if res.Inserted+res.Adopted+res.Updated == 0 {
		return
	}

	// 同步完打一行「现在库里有多少、覆盖到哪天」——这样「历史仓位只有 8 条」
	// 这类问题下次可以直接从日志里看出来，不用再去数页面。
	if db, e := st.DB(); e == nil {
		total, synced, _ := db.HistoryTradeCount()
		mn, mx, _ := db.TradeTimeSpan()
		logx.Logf("INFO", "[POSHIST] 仓位历史已同步：新增 %d / 认领 %d / 更新 %d；"+
			"本地共 %d 个已平仓仓位（其中 %d 个带 posId），覆盖 %s ~ %s",
			res.Inserted, res.Adopted, res.Updated, total, synced,
			repo.FormatTs(mn), repo.FormatTs(mx))
	}
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// stringTrim 去掉首尾空白（含 Windows 命令行可能混进来的 \r）
func stringTrim(s string) string {
	i, j := 0, len(s)
	for i < j && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	for j > i && (s[j-1] == ' ' || s[j-1] == '\t' || s[j-1] == '\n' || s[j-1] == '\r') {
		j--
	}
	return s[i:j]
}
