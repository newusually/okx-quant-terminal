package service

// addon.go —— 加仓（补仓 / 摊薄均价）
//
// 口径（2026-10-01 二期，用户指定）：
//
//	加仓额 = 原持仓保证金 × ratio（默认 1/3，即「仓位为本金的三分之一」）
//	触发   = ★★ 与买入条件**完全一致**：该周期最后一根已收盘 K 线 8 个因子全中 ★★
//	次数   = 不限（max_times = 0）
//
// 判定顺序（每一步不满足就这一轮不加）：
//
//	① 加仓开关开着，仓位有有效开仓价
//	② 用该仓位自己的周期（rise_bar = "auto" → p.Bar）算最新一根已收盘 K 线的 8 因子
//	③ 指标暖机完整（sig.Ready）且 score ≥ cfg.ThresholdFor(instID)（当前 8）
//	④ 这根 K 线必须**晚于**开仓那根（同一根不重复加）
//	⑤ 距上次加仓至少 min_gap_bars 根（同一根 K 线只加一次）
//	⑥ 次数：max_times <= 0 表示不限
//
// ★ 为什么用 LatestSignal 而不是自己抓 K 线：
//   LatestSignal 是出场（布林上轨）与买入扫描**共用的同一个函数**
//   （loadCandles + IndexOfLastClosed + ComputeSignal），
//   所以「加仓条件 = 买入条件」这句话在代码上是真的相等，不是「看着差不多」。
//   自己另写一遍抓 K 线，迟早会在窗口长度或 Confirm 语义上走岔。
//
// ★ 旧的「15m 先跌 0.5% 后转涨」口径已下线（用户：「加仓条件也是和买入条件一样」）。
//   drop_pct / lookback_bars / only_when_price_up 只保留读兼容，不再参与判定。
//
// ★ 「加满 max_times 就自动平仓」这条出场规则已于 2026-10-01 一期删除。
//   现在加满之后只是不再补仓 —— 仓位继续等 +0.3% 止盈 / 1 小时超时，
//   两条出场通道里没有任何一条跟加仓次数有关。
//
// 满足之后按「原保证金 ÷ 3」下单；买不起最小 1 张时沿用 entry.margin_policy
// 的口径（min_one 放大到刚好 1 张，但绝不超过 max_margin_usdt）。
//
// 加仓不新开持仓行：张数 / 加权均价 / 保证金就地合并进原行，
// 所以止盈、布林上轨这些出场判定不用改也能按新均价算盈亏。

import (
	"fmt"
	"strings"

	"finally-main/internal/conf"
	"finally-main/internal/logx"
	"finally-main/internal/model"
	"finally-main/internal/repo"
)

// AddonDecision 一次加仓判定的结果
type AddonDecision struct {
	Add       bool    // 要不要加
	AddPx     float64 // 加仓价（用最新价）
	Sz        float64 // 加仓张数
	Margin    float64 // 本笔加仓保证金（USDT）
	NewSz     float64 // 合并后总张数
	NewAvgPx  float64 // 合并后加权均价
	NewMargin float64 // 合并后总保证金
	Count     int     // 合并后加仓次数
	AllMargin float64 // 合并后累计加仓保证金
	Ts        int64   // 触发用的那根 K 线时间
	Reason    string
}

// runAddons 对所有在持仓检查加仓条件，满足就补仓。
//
// 返回 (加仓成功笔数, 本轮平掉的仓位 ID 集合)。
// 注意第二个返回值现在**恒为空** —— 「加满自动平仓」这条规则已删除，
// 保留签名是为了不动调用方（trader.go）的结构。
//
// 传入的 openPos 会被就地更新（张数 / 均价 / 保证金 / 加仓计数），
// 这样同一轮后面的开仓闸门（总保证金、持仓数）看到的就是最新数据。
func runAddons(cfg *conf.Config, cli *OKXClient, store *repo.Store, kdb KlineReader,
	openPos []repo.OpenPos, markPrices map[string]float64) (int, map[int64]bool) {

	closedIDs := map[int64]bool{}

	a := cfg.Addon
	if a == nil || !a.Enabled || len(openPos) == 0 {
		return 0, closedIDs
	}

	added := 0
	for i := range openPos {
		p := &openPos[i]

		// 加仓用哪个周期判定：rise_bar = "auto"（默认）= 该仓位自己的周期。
		//
		// 为什么按仓位自己的周期：买入是按某个周期扫出来的 8 因子信号，
		// 「加仓条件与买入一致」自然应该对着同一个周期判 ——
		// 1m 开的仓按 1m 判，15m 开的仓按 15m 判。
		// 老仓（本次改造前开的）库里 bar 可能是空的，那时退回配置里的主周期。
		bar := addonBarFor(a.RiseBar, cfg.Bar, p.Bar)
		durMs := BarDurationMs(bar)
		if durMs <= 0 {
			durMs = BarDurationMs("15m")
		}

		px := markPrices[p.InstID]
		if px <= 0 {
			// 拿不到标记价就退回行情最新价
			if tk, err := cli.Tickers(); err == nil {
				if t, ok := tk[p.InstID]; ok && t.Last > 0 {
					px = t.Last
					markPrices[p.InstID] = px
				}
			}
		}
		if px <= 0 {
			continue
		}

		dec, err := checkAddon(cfg, cli, kdb, *p, px, bar, durMs)
		if err != nil {
			logx.Logf("WARN", "%s 加仓判定失败：%v", p.InstID, err)
			continue
		}
		if !dec.Add {
			continue
		}

		// —— 下单 ——
		insts, ierr := cli.Instruments(false)
		if ierr != nil {
			logx.Logf("WARN", "%s 取合约信息失败，本轮不加仓：%v", p.InstID, ierr)
			continue
		}
		ins, ok := insts[p.InstID]
		if !ok {
			logx.Logf("WARN", "%s 合约信息缺失，跳过加仓", p.InstID)
			continue
		}

		ordID := "(dry_run)"
		if !cfg.DryRun {
			if cfg.Entry.TdMode != "" {
				if err := cli.SetLeverage(p.InstID, p.Leverage, cfg.Entry.TdMode); err != nil {
					logx.Logf("WARN", "%s 加仓前设杠杆失败（继续）：%v", p.InstID, err)
				}
			}
			ord, err := cli.PlaceOrder(p.InstID, cfg.Entry.TdMode, "buy", cfg.Entry.PosSide,
				"market", fmtSz(dec.Sz, ins.LotSzDec), false)
			if err != nil {
				logx.Logf("ERROR", "%s 加仓下单失败，下一轮重试：%v", p.InstID, err)
				continue
			}
			ordID = ord.OrdID
		}

		row := model.AddonRow{
			ID: p.ID, InstID: p.InstID, Leverage: p.Leverage,
			Sz: dec.NewSz, EntryPx: dec.NewAvgPx, Margin: dec.NewMargin,
			AddSz: dec.Sz, AddPx: dec.AddPx, AddMargin: dec.Margin,
			AddonCount: dec.Count, AddonMargin: dec.AllMargin, LastAddonTs: dec.Ts,
			OrdID: ordID, Reason: dec.Reason,
		}
		if err := store.ApplyAddon(row); err != nil {
			logx.Logf("WARN", "加仓写回失败（订单已发）：%v", err)
			continue
		}

		// 就地更新，后面的闸门看到最新值
		p.Sz, p.EntryPx, p.Margin = dec.NewSz, dec.NewAvgPx, dec.NewMargin
		p.AddonCount, p.AddonMargin, p.LastAddonTs = dec.Count, dec.AllMargin, dec.Ts

		added++
		logx.Logf("SIGNAL", "加仓 %s 周期=%s 第 %d 次 张数=%s 价格=%.6f 保证金=%.4fU 新均价=%.6f 订单=%s",
			p.InstID, bar, dec.Count, fmtSz(dec.Sz, ins.LotSzDec), dec.AddPx,
			dec.Margin, dec.NewAvgPx, ordID)
	}
	return added, closedIDs
}

// addonBarFor 决定某个仓位用哪个周期做加仓判定。
//
//	riseBar = "auto"（或空）→ 用该仓位自己的周期 instBar；老仓 instBar 为空 → cfgBar
//	riseBar 写了具体周期   → 用它（想「不管什么周期开的都按 15m 加」就写死 15m）
//	最后兜底             → "15m"
//
// 抽成纯函数是为了能被穷举单测 —— 这个映射一旦走岔，
// 「加仓条件与买入一致」就会变成「拿 A 周期的信号加 B 周期的仓」，
// 而且不会报错、日志也看不出异常。
func addonBarFor(riseBar, cfgBar, instBar string) string {
	rb := strings.TrimSpace(riseBar)
	auto := rb == "" || strings.EqualFold(rb, conf.AddonAutoBar)

	bar := ""
	if auto {
		bar = strings.TrimSpace(instBar)
		if bar == "" {
			bar = strings.TrimSpace(cfgBar) // 老仓（bar 列是空的）
		}
	} else {
		bar = rb
	}
	if bar == "" {
		bar = "15m"
	}
	// ★ 2026-10-02 四期：仓位的周期可能**已经下线** ★
	//
	// `rise_bar: "auto"` 取的是**仓位自己的周期**（trade.bar）。老仓可能是
	// 已经下线的 1m —— 那份 K 线会被 repo.CleanupKlines 按 model.EnabledBars
	// 整段删掉，LatestSignal(…, "1m") 从此永远取不到数据。
	// 后果不是「加仓条件略偏」，而是**这个仓位再也不会加仓**（静默失效，
	// 而且是永久性的：K 线不会再回来）。
	//
	// 所以这里必须再判一次白名单：不在名单就退回主周期（cfgBar），
	// 主周期也不在名单就取名单最后一个（通常是最大的周期）。
	if !model.BarEnabled(bar) {
		if fb := strings.TrimSpace(cfgBar); fb != "" && model.BarEnabled(fb) {
			bar = fb
		} else if n := len(model.EnabledBars); n > 0 {
			bar = model.EnabledBars[n-1]
		}
	}
	return bar
}

// checkAddon 单仓加仓判定：算最新一根已收盘 K 线的 8 因子，然后交给纯逻辑 decideAddon。
//
// ★ 2026-10-01 二期：改用 LatestSignal —— 与买入扫描、出场（布林上轨）**同一个函数**。
//
// 用户口径「加仓条件也是和买入条件一样」，那就必须真的用买入那套代码：
// LatestSignal = loadCandles（本地优先、窗口 min_candles 根）
//              + IndexOfLastClosed（只认已收盘那根）
//              + ComputeSignal（8 因子，指标整段算一遍）。
//
// 为什么不再自己抓 K 线自己算：
//   ① 自己另取一个窗口（比如「开仓之后那几根」），递归指标（ATR/RSI/EMA/TD9）
//      的种子位置不同 → 同一根 K 线算出的值就可能不一样，「条件一致」变成假的；
//   ② 一期踩过的坑：closedWindow 直接读 Candle.Confirm，而 klinesToCandles
//      恒把 Confirm 置 false —— 喂错版本会让加仓**静默地永远不触发**。
//      共用 LatestSignal 之后，这条路只有一处实现，不存在喂错的可能。
//
// 传给 decideAddon 的 sig 已经保证：Ready = true（暖机够）、Ts = 那根 K 线的时间。
func checkAddon(cfg *conf.Config, cli *OKXClient, kdb KlineReader, p repo.OpenPos,
	markPx float64, bar string, durMs int64) (AddonDecision, error) {

	a := cfg.Addon
	if a == nil || !a.Enabled {
		return AddonDecision{}, nil
	}
	if p.EntryPx <= 0 {
		return AddonDecision{}, nil
	}

	sig, _, _, err := LatestSignal(cfg, cli, kdb, p.InstID, bar)
	if err != nil {
		return AddonDecision{}, err
	}
	if sig == nil || !sig.Ready {
		return AddonDecision{}, nil // K 线不够 / 暖机不足
	}

	insts, ierr := cli.Instruments(false)
	if ierr != nil {
		return AddonDecision{}, ierr
	}
	ins, ok := insts[p.InstID]
	if !ok {
		return AddonDecision{}, nil
	}
	return decideAddon(cfg, p, markPx, sig, durMs, ins), nil
}

// closedWindow 只留「已收盘」且与持仓时间有交集的 K 线（开仓前就收完的不要）。
//
// ★ 2026-10-01 二期起 decideAddon 不再用它（加仓条件改成了 8 因子共振），
//   保留下来的唯一原因是它是「Candle.Confirm 语义」的历史证据 ——
//   addon_test.go 里那条「喂 klinesToCandles 会得到空窗口」的回归测试还在用它。
//   新代码不要再依赖它。
func closedWindow(cands []Candle, openTs, durMs int64) []Candle {
	win := make([]Candle, 0, len(cands))
	for _, c := range cands {
		if !c.Confirm {
			continue
		}
		if durMs > 0 && c.Ts+durMs <= openTs {
			continue // 开仓之前就收完了，不算
		}
		win = append(win, c)
	}
	return win
}

// decideAddon 纯判定逻辑（不碰网络，方便单测）。
//
// ★ 2026-10-01 二期：触发条件 = 8 因子共振（与买入完全一致）★
//
// sig 必须是由 LatestSignal / ComputeSignal 算出来的那根**已收盘** K 线的信号。
func decideAddon(cfg *conf.Config, p repo.OpenPos, markPx float64,
	sig *Signal, durMs int64, ins Instrument) AddonDecision {

	a := cfg.Addon
	if a == nil || !a.Enabled || sig == nil || p.EntryPx <= 0 || markPx <= 0 {
		return AddonDecision{}
	}
	if !sig.Ready {
		return AddonDecision{}
	}

	// ③ 共振判据：**与买入扫描调同一个函数**（service.SignalQualified）。
	//
	//    2026-10-02 五期口径：Score ≥ 3（threshold = 3）
	//    + 这根 K 线必须真涨过 min_bar_rise_pct（当前 0.5%）。
	//
	//    ★ 刻意不再在这里手写 `sig.Score < th`：买入那边已经改调同一个函数，
	//      这里若继续自己判，两个条件（分数 + 涨幅）就会各自只在一半路径上生效，
	//      「加仓条件与买入一致」立刻变成假的 —— 而且不报错。
	if !SignalQualified(sig, cfg.ThresholdFor(p.InstID), cfg.MinBarRisePct()) {
		return AddonDecision{}
	}

	// ④ 这根 K 线必须**晚于**开仓那根。
	//    开仓时 trade.open_ts 写的就是「触发开仓那根 K 线的时间戳」，
	//    所以 sig.Ts > p.OpenTs 恰好等价于「不是开仓那一根自己」——
	//    否则开仓瞬间就会在同一根上再补一次，那不是加仓，是重复下单。
	if durMs > 0 {
		if sig.Ts <= p.OpenTs {
			return AddonDecision{}
		}
	} else if sig.Ts < p.OpenTs {
		return AddonDecision{}
	}

	// ⑤ 间隔：距上次加仓至少 min_gap_bars 根（同一根 K 线只加一次）
	if a.MinGapBars > 0 && p.LastAddonTs > 0 && durMs > 0 &&
		sig.Ts-p.LastAddonTs < int64(a.MinGapBars)*durMs {
		return AddonDecision{}
	}

	// ⑥ 次数：**<= 0 = 不限**（2026-10-01 二期，用户口径「加仓没有任何限制」）。
	//
	//    这里必须带 `a.MaxTimes > 0` 前置 —— 一期在 entry 的两个计数器上
	//    踩过一模一样的坑：0 会被当成「已达上限 0」，第一笔就被拦掉。
	//
	//    ★ 加满之后只是不再补仓，不再自动平仓：出场只剩 +0.3% 止盈 /
	//      1 小时超时，没有任何一条看加仓次数。
	if a.MaxTimes > 0 && p.AddonCount >= a.MaxTimes {
		return AddonDecision{}
	}

	// —— 金额：原保证金 × ratio ——
	addMargin := a.MarginUSDT
	if addMargin <= 0 {
		ratio := a.Ratio
		if ratio <= 0 {
			ratio = 1.0 / 3.0
		}
		addMargin = p.Margin * ratio
	}
	cap := cfg.Entry.MaxMarginUSDT
	if cap > 0 && addMargin > cap {
		addMargin = cap // 再怎么样也不超过单笔口径
	}
	if addMargin <= 0 {
		return AddonDecision{}
	}

	sz, used, serr := calcSizeWith(cfg, ins, markPx, addMargin, cap, cfg.Entry.MarginPolicy)
	if serr != nil {
		// 买不起最小一张且不允许放大 → 这一轮不加，不算错误
		return AddonDecision{}
	}

	// —— 合并：加权均价 ——
	newSz := p.Sz + sz
	newMargin := p.Margin + used
	newAvg := p.EntryPx
	if newSz > 0 {
		newAvg = (p.Sz*p.EntryPx + sz*markPx) / newSz
	}
	dropFromEntry := (p.EntryPx - markPx) / p.EntryPx * 100
	timesTxt := "不限"
	if a.MaxTimes > 0 {
		timesTxt = fmt.Sprintf("上限 %d 次", a.MaxTimes)
	}
	// 原因文本里把两个条件都写出来（分数门槛 + 这根必须真涨），
	// 否则事后查「为什么加了这一笔」只看到一个数字，看不出三期多出来的那个条件。
	riseTxt := ""
	if mr := cfg.MinBarRisePct(); mr > 0 {
		riseTxt = fmt.Sprintf("、触发那根涨 %.2f%% ＞ %.2f%%", sig.RisePct, mr)
	}

	return AddonDecision{
		Add: true, AddPx: markPx, Sz: sz, Margin: used,
		NewSz: newSz, NewAvgPx: newAvg, NewMargin: newMargin,
		Count: p.AddonCount + 1, AllMargin: p.AddonMargin + used,
		Ts: sig.Ts,
		Reason: fmt.Sprintf("共振 %d/%d（%s）与买入同条件%s → 补原仓位 1/3（现价距均价 %+.2f%%，次数%s）",
			sig.Score, cfg.ThresholdFor(p.InstID), sig.HitList, riseTxt, dropFromEntry, timesTxt),
	}
}
