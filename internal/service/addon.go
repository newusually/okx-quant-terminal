package service

// addon.go —— 加仓（浮亏补仓 / 摊薄均价）
//
// 口径（用户指定）：
//
//	加仓额 = 原持仓保证金 × 1/3
//	触发   = 15m 周期上「先跌 0.5%」然后「15m K 线重新转涨」
//
// 判定顺序（每一步不满足就这一轮不加）：
//
//	① 加仓开关开着，且本仓加仓次数 < max_times
//	② 只取「开仓之后」的已收盘 15m K 线（开仓前的历史走势不算数）
//	③ 先跌：这批 K 线里出现过 ≤ 开仓价 ×(1 - drop_pct%) 的最低价
//	④ 后涨：最后一根已收盘 15m 是阳线（收 > 开）且收盘价高于前一根收盘价
//	⑤ 低点必须落在最后一根之前（先跌 → 后涨，顺序不能反）
//	⑥ 距上次加仓至少 min_gap_bars 根（同一根 K 线只加一次）
//
// ★ 「加满 max_times 就自动平仓」这条出场规则已于 2026-10-01 彻底删除
//   （用户：「平仓取消掉一个条件，就是加仓次数，这个不需要」）。
//   现在加满之后只是不再补仓 —— 仓位继续等 +1% 止盈 / 6 小时超时 / 布林上轨，
//   三条出场通道里没有任何一条跟加仓次数有关。
//
// 满足之后按「原保证金 ÷ 3」下单；买不起最小 1 张时沿用 entry.margin_policy
// 的口径（min_one 放大到刚好 1 张，但绝不超过 max_margin_usdt）。
//
// 加仓不新开持仓行：张数 / 加权均价 / 保证金就地合并进原行，
// 所以止盈、布林上轨这些出场判定不用改也能按新均价算盈亏。

import (
	"fmt"
	"math"
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
func runAddons(cfg *conf.Config, cli *OKXClient, store *repo.Store,
	openPos []repo.OpenPos, markPrices map[string]float64) (int, map[int64]bool) {

	closedIDs := map[int64]bool{}

	a := cfg.Addon
	if a == nil || !a.Enabled || len(openPos) == 0 {
		return 0, closedIDs
	}
	bar := strings.TrimSpace(a.RiseBar)
	if bar == "" {
		bar = "15m"
	}
	durMs := BarDurationMs(bar)
	if durMs <= 0 {
		durMs = BarDurationMs("15m")
	}

	added := 0
	for i := range openPos {
		p := &openPos[i]
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

		dec, err := checkAddon(cfg, cli, *p, px, bar, durMs)
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
		logx.Logf("SIGNAL", "加仓 %s 第 %d 次 张数=%s 价格=%.6f 保证金=%.4fU 新均价=%.6f 订单=%s",
			p.InstID, dec.Count, fmtSz(dec.Sz, ins.LotSzDec), dec.AddPx,
			dec.Margin, dec.NewAvgPx, ordID)
	}
	return added, closedIDs
}

// checkAddon 单仓加仓判定：抓 K 线 + 合约信息，然后交给纯逻辑 decideAddon
func checkAddon(cfg *conf.Config, cli *OKXClient, p repo.OpenPos,
	markPx float64, bar string, durMs int64) (AddonDecision, error) {

	a := cfg.Addon
	if a == nil || !a.Enabled {
		return AddonDecision{}, nil
	}
	// 次数加满的判定放在下面第 ⑥ 步（要先确认「加仓信号确实又成立了」，
	// 否则一个根本没触发的仓位也会被当成「加满」而白跑一遍分支）。
	if p.EntryPx <= 0 {
		return AddonDecision{}, nil
	}

	look := a.LookbackBars
	if look < 4 {
		look = 4
	}
	cands, err := cli.Candles(p.InstID, bar, look+10)
	if err != nil {
		return AddonDecision{}, err
	}

	win := closedWindow(cands, p.OpenTs, durMs)
	if len(win) < 2 {
		return AddonDecision{}, nil // 至少要两根才谈得上「先跌后涨」
	}

	insts, ierr := cli.Instruments(false)
	if ierr != nil {
		return AddonDecision{}, ierr
	}
	ins, ok := insts[p.InstID]
	if !ok {
		return AddonDecision{}, nil
	}
	return decideAddon(cfg, p, markPx, win, durMs, ins), nil
}

// closedWindow 只留「已收盘」且与持仓时间有交集的 K 线（开仓前就收完的不要）
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
// win 必须是从老到新、且已收盘的 K 线序列。
func decideAddon(cfg *conf.Config, p repo.OpenPos, markPx float64,
	win []Candle, durMs int64, ins Instrument) AddonDecision {

	a := cfg.Addon
	if a == nil || !a.Enabled || len(win) < 2 || p.EntryPx <= 0 || markPx <= 0 {
		return AddonDecision{}
	}

	// ② 先跌：出现过 ≤ 开仓价 ×(1 - drop_pct%) 的最低价
	dropPct := a.DropPct
	if dropPct <= 0 {
		dropPct = 0.5
	}
	threshold := p.EntryPx * (1 - dropPct/100)
	iLow, minLow := 0, math.MaxFloat64
	for i, c := range win {
		if c.L > 0 && c.L < minLow {
			minLow, iLow = c.L, i
		}
	}
	if minLow == math.MaxFloat64 {
		return AddonDecision{}
	}
	if markPx < minLow {
		minLow, iLow = markPx, len(win)-1
	}
	if minLow > threshold+1e-12 {
		return AddonDecision{} // 压根没跌够
	}

	// ④ 低点必须在最后一根之前（先跌 → 后涨，顺序不能反）
	if iLow >= len(win)-1 {
		return AddonDecision{}
	}

	// ③ 后涨：最后一根已收盘 K 线是阳线且收盘高于前一根
	last, prev := win[len(win)-1], win[len(win)-2]
	if !(last.C > last.O && last.C > prev.C) {
		return AddonDecision{}
	}

	// ⑤ 间隔
	if a.MinGapBars > 0 && p.LastAddonTs > 0 && durMs > 0 &&
		last.Ts-p.LastAddonTs < int64(a.MinGapBars)*durMs {
		return AddonDecision{}
	}

	// ⑥ 次数：加满了。
	//
	// 「先跌 0.5% → 重新转涨」这套加仓信号又成立了一次，但 3 次额度已经用完。
	//
	// ★ 2026-10-01 起：加满之后**只是不再补仓**，不再自动平仓。
	//   用户明确要求取消「加仓次数」这条出场条件 —— 出场只剩三条：
	//   +1% 止盈 / 6 小时超时 / 布林上轨，没有任何一条看加仓次数。
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
	realDrop := (p.EntryPx - minLow) / p.EntryPx * 100

	return AddonDecision{
		Add: true, AddPx: markPx, Sz: sz, Margin: used,
		NewSz: newSz, NewAvgPx: newAvg, NewMargin: newMargin,
		Count: p.AddonCount + 1, AllMargin: p.AddonMargin + used,
		Ts: last.Ts,
		Reason: fmt.Sprintf("15m 先跌 %.2f%%（低点 %.6f ≤ %.6f）后转涨（%.6f > %.6f）→ 补原仓位 1/3",
			realDrop, minLow, threshold, last.C, prev.C),
	}
}
