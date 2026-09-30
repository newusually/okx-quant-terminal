package service

// trader.go —— 策略引擎：扫描 → 闸门 → 下单 → 出场（对应文案 §7）
//
// 一轮 Run() 做的事：
//  1. 连接 OKX（自动挑域名 + 对时）
//  2. 出场检查（每轮都做，跟周期无关）
//  3. 若当前周期在 bars_enabled 里 → 扫描全市场 + 8 因子共振 + 开仓闸门
//  4. 落库（信号 / 成交 / 权益 / 日志）
//
// dry_run=true 时只算信号、只记库，绝不发任何真单。

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"finally-main/internal/conf"
	"finally-main/internal/logx"
	"finally-main/internal/repo"
)

type engineState struct {
	mu           sync.Mutex
	cli          *OKXClient
	cliKey       string
	storeInit    bool
	cycles       int64
	apiErrStreak int
	pausedUntil  time.Time
	pauseReason  string
}

var eng = &engineState{}

func (e *engineState) client(cfg *conf.Config) (*OKXClient, error) {
	key := cfg.OKX.BaseURL + "|" + cfg.OKX.Proxy + "|" +
		strconv.FormatBool(cfg.OKX.Simulated) + "|" + cfg.OKX.APIKey
	if e.cli != nil && e.cliKey == key {
		return e.cli, nil
	}
	c, err := newOKXClient(cfg)
	if err != nil {
		return nil, err
	}
	e.cli = c
	e.cliKey = key
	return c, nil
}

// paused 必须在持有 eng.mu 的情况下调用
func (e *engineState) paused() (bool, string) {
	if time.Now().Before(e.pausedUntil) {
		return true, e.pauseReason
	}
	return false, ""
}

func (e *engineState) pause(d time.Duration, reason string) {
	e.pausedUntil = time.Now().Add(d)
	e.pauseReason = reason
	logx.Logf("WARN", "策略暂停 %s：%s", d, reason)
}

// ---------------------------------------------------------------------------
// 主入口
// ---------------------------------------------------------------------------

// EngineRun 跑一轮。minute 取值与 OKX 一致：1m 3m 5m 15m 30m 1H 2H 4H 6H 12H 1D
func EngineRun(minute string) error {
	cfg := conf.LoadConfig()
	if cfg == nil || !cfg.Enabled {
		return nil
	}
	bar := normalizeBar(minute)

	eng.mu.Lock()
	defer eng.mu.Unlock()

	start := time.Now()
	eng.cycles++

	cli, err := eng.client(cfg)
	if err != nil {
		return err
	}
	if err := cli.EnsureReady(); err != nil {
		eng.apiErrStreak++
		eng.maybePauseOnErrors(cfg)
		return err
	}

	store := repo.NewStore(cfg)
	if !eng.storeInit {
		if err := store.Init(); err != nil {
			logx.Logf("WARN", "MySQL 初始化失败（不影响交易）：%v", err)
		} else {
			eng.storeInit = true
			logx.Logf("INFO", "MySQL 就绪：mysql://%s@%s:%d/%s",
				cfg.Store.User, cfg.Store.Host, cfg.Store.Port, cfg.Store.Database)
		}
	}

	// 在持仓 + 当日统计
	openPos, err := store.OpenPositions()
	if err != nil {
		logx.Logf("WARN", "读在持仓失败：%v", err)
		openPos = nil
	}
	ctr, err := store.Counters(todayStartMs())
	if err != nil {
		logx.Logf("WARN", "读当日统计失败：%v", err)
		ctr = &repo.Counters{LastEntryTs: map[string]int64{}}
	}

	// 账户信息（配了 Key 才有）
	var account *Account
	markPrices := map[string]float64{}
	if hasKeys(cfg) {
		if acc, aerr := cli.Balance(); aerr == nil {
			account = acc
		} else {
			eng.apiErrStreak++
			logx.Logf("WARN", "取账户余额失败：%v", aerr)
		}
		if ps, perr := cli.Positions(); perr == nil {
			account = accountOrNew(account)
			account.PosCount = len(ps)
			account.PositionList = ps
			for _, p := range ps {
				if mp := toF(p.MarkPx); mp > 0 {
					markPrices[p.InstID] = mp
				}
			}
		} else {
			logx.Logf("WARN", "取持仓失败：%v", perr)
		}
	}

	// ① 出场（每轮都查）
	closed, closedIDs := runExits(cfg, cli, store, openPos, markPrices, bar)
	openPos = dropClosed(openPos, closedIDs)

	// ①.5 加仓（浮亏补仓）：15m 先跌 0.5% 再转涨 → 补原仓位的 1/3
	//      必须在出场之后（刚平的仓不加）、入场之前（总保证金按新值算）
	added := runAddons(cfg, cli, store, openPos, markPrices)

	// ② 入场
	scanInfo := fmt.Sprintf("周期 %s 未启用扫描（bars_enabled 未包含）", bar)
	if cfg.BarEnabled(bar) {
		res, serr := Scan(cfg, cli, bar)
		if serr != nil {
			eng.apiErrStreak++
			eng.maybePauseOnErrors(cfg)
			return serr
		}
		eng.apiErrStreak = 0

		// 先只记信号（acted=0），下单情况再回来补
		if len(res.Signals) > 0 {
			rows := make([]repo.EngineSignalRow, 0, len(res.Signals))
			nowMs := time.Now().UnixMilli()
			for _, s := range res.Signals {
				rows = append(rows, repo.EngineSignalRow{
					InstID: s.InstID, Bar: s.Bar, Ts: s.Ts, Close: s.Close,
					Mask: s.Mask, Score: s.Score, HitList: s.HitList,
					Pot: s.Pot, Fri: s.Fri, Kin: s.Kin, Rsi: s.Rsi, Td: s.Td,
					CreatedAt: nowMs,
				})
			}
			if err := store.Ingest(repo.StorePayload{Signal: rows}); err != nil {
				logx.Logf("WARN", "写信号失败：%v", err)
			}
		}

		opened, updates := runEntries(cfg, cli, store, res, ctr, openPos, account, bar)
		if len(updates) > 0 {
			if err := store.Ingest(repo.StorePayload{SignalUpdate: updates}); err != nil {
				logx.Logf("WARN", "更新信号状态失败：%v", err)
			}
		}
		scanInfo = fmt.Sprintf("全市场 %d / 候选 %d / 实算 %d / 信号 %d / 开仓 %d",
			res.Universe, res.Candidates, res.Scanned, len(res.Signals), opened)
	}

	if closed > 0 || added > 0 || cfg.BarEnabled(bar) {
		logx.Logf("INFO", "[%s] %s；在持 %d 仓，本轮平仓 %d、加仓 %d，用时 %s",
			bar, scanInfo, len(openPos), closed, added, time.Since(start).Round(time.Millisecond))
	}

	// ③ 权益快照
	if account != nil {
		if err := store.Ingest(repo.StorePayload{Equity: []repo.EquityRow{{
			Ts: time.Now().UnixMilli(), TotalEq: account.TotalEq,
			Avail: account.AvailEq, Upl: account.Upl, PosCount: account.PosCount,
		}}}); err != nil {
			logx.Logf("WARN", "写权益快照失败：%v", err)
		}
	}

	// ④ 日志落库
	if logs := logx.TakeRunLogs(); len(logs) > 0 {
		store.Ingest(repo.StorePayload{Runlog: logs}) // 失败不再写日志，避免递归
	}

	// ⑤ 每 20 轮滚动清理一次
	if eng.cycles%20 == 0 {
		if err := store.Cleanup(); err != nil {
			logx.Logf("WARN", "滚动清理失败：%v", err)
		}
	}
	return nil
}

func (e *engineState) maybePauseOnErrors(cfg *conf.Config) {
	if cfg.Risk == nil || cfg.Risk.PauseOnAPIError <= 0 {
		return
	}
	if e.apiErrStreak >= cfg.Risk.PauseOnAPIError {
		e.pause(30*time.Minute, fmt.Sprintf("连续 %d 次 API 错误", e.apiErrStreak))
		e.apiErrStreak = 0
	}
}

// ---------------------------------------------------------------------------
// 出场（对应文案 §7.3）
// ---------------------------------------------------------------------------

func runExits(cfg *conf.Config, cli *OKXClient, store *repo.Store, openPos []repo.OpenPos,
	markPrices map[string]float64, bar string) (int, map[int64]bool) {

	closedIDs := map[int64]bool{}
	if len(openPos) == 0 {
		return 0, closedIDs
	}
	// 拿不到标记价就退回行情
	needQuote := false
	for _, p := range openPos {
		if markPrices[p.InstID] <= 0 {
			needQuote = true
			break
		}
	}
	if needQuote {
		if tk, err := cli.Tickers(); err == nil {
			for _, p := range openPos {
				if markPrices[p.InstID] <= 0 {
					if t, ok := tk[p.InstID]; ok && t.Last > 0 {
						markPrices[p.InstID] = t.Last
					}
				}
			}
		}
	}

	closed := 0
	nowMs := time.Now().UnixMilli()
	for _, p := range openPos {
		px := markPrices[p.InstID]
		if px <= 0 || p.EntryPx <= 0 {
			continue
		}
		pnlPct := (px/p.EntryPx - 1) * 100
		reason := ""

		if cfg.Exit.TakeProfitPct > 0 && pnlPct >= cfg.Exit.TakeProfitPct {
			reason = fmt.Sprintf("止盈 %+.2f%%", pnlPct)
		} else if cfg.Exit.StopLossPct > 0 && pnlPct <= -cfg.Exit.StopLossPct {
			reason = fmt.Sprintf("止损 %.2f%%", pnlPct)
		} else if cfg.Exit.MaxHoldBars > 0 {
			dur := BarDurationMs(p.Bar)
			if dur <= 0 {
				dur = BarDurationMs(cfg.Bar)
			}
			if dur > 0 && nowMs-p.OpenTs >= int64(cfg.Exit.MaxHoldBars)*dur {
				reason = fmt.Sprintf("超时 %d 根", cfg.Exit.MaxHoldBars)
			}
		}
		if reason == "" && cfg.Exit.BollUpperExit {
			if sig, _, err := LatestSignal(cfg, cli, p.InstID, p.Bar); err == nil && sig != nil &&
				!isNaN(sig.BollUp) && sig.Close > sig.BollUp {
				reason = "布林上轨（均值回归出场）"
			}
		}
		if reason == "" {
			continue
		}

		pnl := p.Margin * float64(p.Leverage) * pnlPct / 100
		ordID := "(dry_run)"
		if !cfg.DryRun {
			ord, err := cli.PlaceOrder(p.InstID, cfg.Entry.TdMode, "sell", cfg.Entry.PosSide,
				"market", fmtSz(p.Sz, 10), true)
			if err != nil {
				logx.Logf("ERROR", "%s 平仓失败，下一轮重试：%v", p.InstID, err)
				continue
			}
			ordID = ord.OrdID
		}
		row := repo.CloseRow{
			ID: p.ID, ExitPx: px, Pnl: pnl, PnlPct: pnlPct,
			Reason: reason, CloseTs: nowMs, OrdID: ordID,
		}
		if err := store.Ingest(repo.StorePayload{CloseTrade: &row}); err != nil {
			logx.Logf("WARN", "写平仓记录失败：%v", err)
		}
		closed++
		closedIDs[p.ID] = true
		logx.Logf("SIGNAL", "平仓 %s 张数=%s 开仓价=%.6f 平仓价=%.6f 盈亏=%+.4fU(%+.2f%%) 原因=%s",
			p.InstID, fmtSz(p.Sz, 10), p.EntryPx, px, pnl, pnlPct, reason)
	}
	return closed, closedIDs
}

// dropClosed 把本轮已平掉的仓位从在持仓列表里摘掉。
//
// 不做这一步的话，同一轮里刚平掉的仓还会被当成「在持仓」：
// 加仓会对已平仓的単子补仓、开仓闸门也会被无谓占用。
func dropClosed(pos []repo.OpenPos, closedIDs map[int64]bool) []repo.OpenPos {
	if len(closedIDs) == 0 {
		return pos
	}
	out := make([]repo.OpenPos, 0, len(pos))
	for _, p := range pos {
		if !closedIDs[p.ID] {
			out = append(out, p)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 入场（对应文案 §7.6 的判定顺序）
// ---------------------------------------------------------------------------

func runEntries(cfg *conf.Config, cli *OKXClient, store *repo.Store, res *ScanResult, ctr *repo.Counters,
	openPos []repo.OpenPos, account *Account, bar string) (int, []repo.SignalUpdate) {

	updates := []repo.SignalUpdate{}
	if len(res.Signals) == 0 {
		return 0, updates
	}

	insts, ierr := cli.Instruments(false)
	if ierr != nil {
		logx.Logf("WARN", "取合约信息失败，本轮不开仓：%v", ierr)
		for _, s := range res.Signals {
			updates = append(updates, repo.SignalUpdate{InstID: s.InstID, Bar: s.Bar, Ts: s.Ts,
				Acted: 2, Reason: "取合约信息失败"})
		}
		return 0, updates
	}

	openSet := map[string]bool{}
	marginSum := 0.0
	for _, p := range openPos {
		openSet[p.InstID] = true
		marginSum += p.Margin
	}
	durMs := BarDurationMs(bar)
	if durMs <= 0 {
		durMs = BarDurationMs(cfg.Bar)
	}

	blockAll, blockWhy := globalRiskBlock(cfg, ctr, account, marginSum)
	if blockAll {
		logx.Logf("WARN", "全局风控拦截，本轮只记信号不下单：%s", blockWhy)
	}

	opened := 0
	for _, s := range res.Signals {
		upd := repo.SignalUpdate{InstID: s.InstID, Bar: s.Bar, Ts: s.Ts}
		skip := func(why string) {
			upd.Acted = 2
			upd.Reason = why
			updates = append(updates, upd)
		}

		// —— §7.6 闸门，顺序照抄 ——
		if blockAll {
			skip(blockWhy)
			continue
		}
		if paused, why := eng.paused(); paused {
			skip("策略暂停：" + why)
			continue
		}
		if len(openPos)+opened >= cfg.Entry.MaxConcurrentPositions {
			skip(fmt.Sprintf("持仓数已达上限 %d", cfg.Entry.MaxConcurrentPositions))
			continue
		}
		if openSet[s.InstID] {
			skip("该合约已持仓")
			continue
		}
		if last, ok := ctr.LastEntryTs[s.InstID]; ok && last > 0 && durMs > 0 &&
			s.Ts-last < int64(cfg.Entry.CooldownBars)*durMs {
			skip(fmt.Sprintf("冷却中（距上次开仓不足 %d 根）", cfg.Entry.CooldownBars))
			continue
		}
		if ctr.OrdersToday+opened >= cfg.Entry.DailyMaxEntries {
			skip(fmt.Sprintf("当日开仓已达上限 %d", cfg.Entry.DailyMaxEntries))
			continue
		}

		ins, ok := insts[s.InstID]
		if !ok {
			skip("合约信息缺失")
			continue
		}
		sz, marginUsed, serr := calcSize(cfg, ins, s.Close)
		if serr != nil {
			skip(serr.Error())
			continue
		}

		// —— 下单 ——
		ordID := "(dry_run)"
		if cfg.OrderVia == "legacy" || cfg.OrderVia == "python" {
			if cfg.DryRun {
				logx.Logf("INFO", "[dry_run] 应走 legacy orderbuy：%s %s", s.InstID, bar)
			} else if err := legacyOrder(cfg, s.InstID, bar); err != nil {
				skip("legacy 下单失败：" + truncate(err.Error(), 100))
				logx.Logf("ERROR", "%s legacy 下单失败：%v", s.InstID, err)
				continue
			} else {
				ordID = "(legacy)"
			}
		} else if cfg.DryRun {
			logx.Logf("INFO", "[dry_run] 应开仓 %s 张数=%s 保证金=%.2fU 杠杆=%dx 价格=%.6f 共振=%d/8",
				s.InstID, fmtSz(sz, ins.LotSzDec), marginUsed,
				cfg.Entry.Leverage, s.Close, s.Score)
		} else {
			if err := cli.SetLeverage(s.InstID, cfg.Entry.Leverage, cfg.Entry.TdMode); err != nil {
				logx.Logf("WARN", "%s 设杠杆失败（继续下单）：%v", s.InstID, err)
			}
			ord, err := cli.PlaceOrder(s.InstID, cfg.Entry.TdMode, "buy", cfg.Entry.PosSide,
				cfg.Entry.OrdType, fmtSz(sz, ins.LotSzDec), false)
			if err != nil {
				skip("下单失败：" + truncate(err.Error(), 100))
				logx.Logf("ERROR", "%s 下单失败：%v", s.InstID, err)
				continue
			}
			ordID = ord.OrdID
		}

		// —— AI 解读：只对「过闸门、真开仓（含 dry_run 模拟开仓）」的信号调用 ——
		aiNote := ""
		if cfg.AI != nil && cfg.AI.Enabled {
			note, aerr := AIReview(cfg, cli, s, s.Close)
			if aerr != nil {
				aiNote = "AI 解读不可用"
				logx.Logf("WARN", "AI 解读失败（不影响交易）：%v", aerr)
			} else {
				aiNote = note
				logx.Logf("SIGNAL", "AI 点评 %s：%s", s.InstID, note)
			}
		}

		reason := fmt.Sprintf("8因子共振 %d/8（%s）", s.Score, s.HitList)
		if err := store.Ingest(repo.StorePayload{Trade: []repo.TradeRow{{
			InstID: s.InstID, Side: "buy", Sz: sz, EntryPx: s.Close,
			Margin: marginUsed, Leverage: cfg.Entry.Leverage,
			OpenTs: s.Ts, Score: s.Score, Bar: s.Bar, Reason: reason,
			OrdID: ordID, Status: "open", AINote: aiNote,
		}}}); err != nil {
			logx.Logf("WARN", "写成交失败：%v", err)
		}

		upd.Acted = 1
		upd.Reason = reason
		upd.AINote = aiNote
		updates = append(updates, upd)

		openSet[s.InstID] = true
		ctr.LastEntryTs[s.InstID] = s.Ts
		opened++
		logx.Logf("SIGNAL", "开仓 %s 张数=%s 开仓价=%.6f 共振 %d/8 [%s] 订单=%s",
			s.InstID, fmtSz(sz, ins.LotSzDec), s.Close, s.Score, s.HitList, ordID)
	}

	return opened, updates
}

// ---------------------------------------------------------------------------
// 风控（对应文案 §7.5）
// ---------------------------------------------------------------------------

func globalRiskBlock(cfg *conf.Config, ctr *repo.Counters, account *Account, marginSum float64) (bool, string) {
	r := cfg.Risk
	if r == nil {
		return false, ""
	}
	if r.ConsecutiveLossPause > 0 && ctr.ConsecutiveLosses >= r.ConsecutiveLossPause {
		return true, fmt.Sprintf("连亏 %d 笔，超过 %d 笔暂停阈值", ctr.ConsecutiveLosses, r.ConsecutiveLossPause)
	}
	if account == nil {
		// 没配 Key（例如 dry_run 观察期）→ 只做统计类风控
		return false, ""
	}
	if r.AccountEquityStop > 0 && account.TotalEq > 0 && account.TotalEq < r.AccountEquityStop {
		return true, fmt.Sprintf("权益 %.2f 低于下限 %.2f", account.TotalEq, r.AccountEquityStop)
	}
	if r.MinAvailableUSDT > 0 && account.AvailEq < r.MinAvailableUSDT {
		return true, fmt.Sprintf("可用余额 %.2f 低于下限 %.2f", account.AvailEq, r.MinAvailableUSDT)
	}
	if r.DailyLossStopPct > 0 && account.TotalEq > 0 && ctr.TodayPnl < 0 {
		lossPct := -ctr.TodayPnl / account.TotalEq * 100
		if lossPct >= r.DailyLossStopPct {
			return true, fmt.Sprintf("当日亏损 %.2f%% 超过 %.2f%%", lossPct, r.DailyLossStopPct)
		}
	}
	if r.MaxTotalMarginPct > 0 && account.TotalEq > 0 {
		if pct := marginSum / account.TotalEq * 100; pct >= r.MaxTotalMarginPct {
			return true, fmt.Sprintf("总保证金占比 %.2f%% 超过 %.2f%%", pct, r.MaxTotalMarginPct)
		}
	}
	return false, ""
}

// ---------------------------------------------------------------------------
// 手数（对应文案 §7.1）
// ---------------------------------------------------------------------------

// calcSize 张数 = 保证金 × 杠杆 / (ctVal × ctMult × price)，向下对齐 lotSz 且不小于 minSz
//
// 返回 (张数, 实际占用保证金, 错误)。
// OKX 永续的 sz 单位是「张」：BTC 一张≈600U、ETH≈300U，所以 1U×20x=20U 名义买不起它们。
// 这时按 entry.margin_policy 决定：fixed 跳过；min_one 放大到刚好买 1 张（不超过 max_margin_usdt）。
func calcSize(cfg *conf.Config, ins Instrument, price float64) (float64, float64, error) {
	return calcSizeWith(cfg, ins, price, cfg.Entry.MarginUSDT, cfg.Entry.MaxMarginUSDT, cfg.Entry.MarginPolicy)
}

// calcSizeWith 按指定保证金 / 上限 / 口径算张数（加仓复用同一套逻辑）。
//
//	price        当前价（用来算每张名义价值）
//	margin       这笔想投多少保证金
//	cap          这笔的保证金硬上限（0 = 不限）
//	policy       "fixed" 买不起就报错 / "min_one" 放大到刚好 1 张（≤ cap）
func calcSizeWith(cfg *conf.Config, ins Instrument, price, margin, cap float64,
	policy string) (float64, float64, error) {

	if price <= 0 {
		return 0, 0, fmt.Errorf("价格非法")
	}
	per := ins.CtVal * ins.CtMult * price
	if per <= 0 {
		return 0, 0, fmt.Errorf("每张名义价值非法（ctVal=%.8g ctMult=%.8g）", ins.CtVal, ins.CtMult)
	}
	lot := ins.LotSz
	if lot <= 0 {
		lot = 1
	}
	// 最小可下单位：lotSz 与 minSz 取大（并按 lotSz 对齐）
	unit := lot
	if ins.MinSz > unit {
		unit = math.Ceil(ins.MinSz/lot) * lot
	}

	lev := float64(cfg.Entry.Leverage)
	if lev <= 0 {
		lev = 1
	}
	if margin <= 0 {
		return 0, 0, fmt.Errorf("保证金预算为 0，跳过")
	}
	sz := math.Floor(margin*lev/per/lot+1e-9) * lot

	if sz < unit {
		if strings.EqualFold(policy, "min_one") {
			needMargin := unit * per / lev
			if cap > 0 && needMargin > cap {
				return 0, 0, fmt.Errorf("买 %s 张至少要 %.2fU 保证金，超过上限 %.2fU（每张名义 %.2fU）",
					fmtSz(unit, ins.LotSzDec), needMargin, cap, per)
			}
			return unit, needMargin, nil
		}
		needMargin := unit * per / lev
		return 0, 0, fmt.Errorf("名义 %.0fU 买不起 %s 张（每张名义 %.2fU，至少要 %.2fU 保证金）"+
			"；调大 margin_usdt，或把 margin_policy 改成 \"min_one\"",
			margin*lev, fmtSz(unit, ins.LotSzDec), per, needMargin)
	}
	return sz, margin, nil
}

// fmtSz 按小数位格式化张数（OKX 的 sz 是字符串，多一位精度都会报错）
func fmtSz(sz float64, dec int) string {
	if dec < 0 {
		dec = 0
	}
	if dec > 10 {
		dec = 10
	}
	s := strconv.FormatFloat(sz, 'f', dec, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	if s == "" || s == "-" {
		s = "0"
	}
	return s
}

// ---------------------------------------------------------------------------
// 其它
// ---------------------------------------------------------------------------

func normalizeBar(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "15m"
	}
	return s
}

func hasKeys(cfg *conf.Config) bool {
	return cfg.OKX != nil && cfg.OKX.APIKey != "" && cfg.OKX.SecretKey != "" && cfg.OKX.Passphrase != ""
}

func accountOrNew(a *Account) *Account {
	if a == nil {
		return &Account{}
	}
	return a
}

func todayStartMs() int64 {
	now := time.Now()
	y, m, d := now.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, now.Location()).UnixMilli()
}

// legacyOrder 曾经是 exec.Command("python", "gorun.py", ...) 那条老路。
// 现在 gorun.py 已经删掉、逻辑全在 mvc 包里，这里直接调纯 Go 的 orderbuy，
// 一个外部进程都不再拉起来。函数名保持不动，免得动 cfg.order_via 的取值语义。
func legacyOrder(cfg *conf.Config, instID, bar string) error {
	ops, err := DefaultOps()
	if err != nil {
		return fmt.Errorf("初始化账户失败：%w", err)
	}
	msg, err := ops.OrderBuy(instID, bar, false)
	if err != nil {
		return fmt.Errorf("下单失败：%w", err)
	}
	logx.Logf("INFO", "下单完成：%s", truncate(msg, 300))
	return nil
}
