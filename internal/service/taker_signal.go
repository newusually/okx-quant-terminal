package service

// taker_signal.go —— taker 买卖比 MACD 的「由负转正」买入信号
//
// 用户口径（2026-10-03）：
//   「盘中有信号就实时提醒买入，买入条件就是 macd>0 and ref macd<0
//    refref macd<0」
//
// 逐字拆开就是三个数比大小：
//   macd        —— 当前那一根的值
//   ref macd    —— 前一根（ref 一次）
//   refref macd —— 前前一根（ref 两次）
//   条件        —— val > 0 && prev < 0 && prev2 < 0
//
// ★ 为什么「前两根都必须 < 0」这个条件才是重点：
//   单看 val>0 && prev<0 只能说明「刚翻正」，如果前两根是一负一正地贴着
//   零轴抖，那这种翻正是噪声，一天能报几十次。加上 prev2<0 之后，要求
//   连续两根在零轴下方，报出来的就是**从下方上来的干净反转**。
//
// ---------------------------------------------------------------------------
// 三种口径（用户没说是哪条线，所以三条都算）
// ---------------------------------------------------------------------------
//   dif  —— 快线（DIF）上穿 0 轴。口语里「上穿 0 轴」通常指它。
//   dea  —— 慢线（DEA）上穿 0 轴。更钝、更少。
//   hist —— 柱（2*(DIF-DEA)）翻红。★ 默认
//
//   默认 hist 的原因：通达信 / 文华财经里变量 `MACD` 的定义就是
//   2*(DIFF-DEA)（正是本项目的 hist 列），用户写「macd>0」时按的
//   大概率是看图上那排柱子从绿翻红。三条都落库了，前端换个下拉即可，
//   不需要改后端、更不需要重新部署。
//
// ---------------------------------------------------------------------------
// 纪律
// ---------------------------------------------------------------------------
//   · 输入序列 = taker_macd（预计算表），与副图、与通知展示**同一条**数据，
//     不在前端另算一份（项目铁律：同一量只能有一条计算路径）。
//   · 只对**已收盘**的根出信号：正在走的那一根 MACD 会跟着价格跳，
//     拿它报警会在收盘前反复触发/撤销，同一根能报三次。
//   · 落库幂等（bar, ts, rule），重算窗口不会产生重复。清理由
//     TakerPanelRebuild 内的 PurgeTakerSignalBefore 负责，不会无限长。

import (
	"strings"

	"finally-main/internal/repo"
)

const (
	// TakerSignalRuleDif 快线 DIF 上穿 0 轴
	TakerSignalRuleDif = "dif"
	// TakerSignalRuleDea 慢线 DEA 上穿 0 轴
	TakerSignalRuleDea = "dea"
	// TakerSignalRuleHist 柱 2*(DIF-DEA) 由负转正
	TakerSignalRuleHist = "hist"

	// TakerSignalDefaultRule 默认口径（见文件头「默认 hist 的原因」）
	TakerSignalDefaultRule = TakerSignalRuleHist

	// TakerSignalMaxLimit 单次最多返回多少条（前端首屏 50 足够，防手改 URL 打爆）
	TakerSignalMaxLimit = 500
)

// TakerSignalRules 全部可选口径
//
// 前端下拉、接口校验、日志都从这里取 —— 三处各写一份迟早不一致。
func TakerSignalRules() []string {
	return []string{TakerSignalRuleDif, TakerSignalRuleDea, TakerSignalRuleHist}
}

// NormalizeTakerSignalRule 归一 rule（认不出来的走默认，不报错）
//
// 老经验：非法值直接报错会让「前端拼错一个参数」变成「整块功能不可用」，
// 而归一到默认至少还能用，且返回值会原样回给前端做核对。
func NormalizeTakerSignalRule(rule string) string {
	switch strings.ToLower(strings.TrimSpace(rule)) {
	case TakerSignalRuleDif:
		return TakerSignalRuleDif
	case TakerSignalRuleDea:
		return TakerSignalRuleDea
	case TakerSignalRuleHist:
		return TakerSignalRuleHist
	}
	return TakerSignalDefaultRule
}

// TakerSignalVal 按口径取 MACD 值
func TakerSignalVal(m repo.TakerMacdRow, rule string) float64 {
	switch rule {
	case TakerSignalRuleDif:
		return m.Dif
	case TakerSignalRuleDea:
		return m.Dea
	default:
		return m.Hist
	}
}

// TakerRiseAt 把面板行翻成「某根 ts → 这根的 ETH 涨跌幅%」。
//
// ★ 为什么绕一下：面板第 3 列存的是**下一根**的涨跌幅（用户要的是「看完
//   taker 之后下一根怎么走」），而信号要的是**触发那根自己**的涨跌幅。
//   面板里 ts 那一行记的是 ts+一根的涨幅，所以要往前挪一根才是「这根的涨幅」。
func TakerRiseAt(panel []repo.TakerPanelRow) map[int64]float64 {
	m := make(map[int64]float64, len(panel))
	for _, p := range panel {
		if p.EthNextOk {
			m[p.Ts+takerPanelBarMS] = p.EthNextPct
		}
	}
	return m
}

// TakerSignalsFromMacd 从 MACD 序列扫出信号（三种口径一次全扫）。
//
//	rows   —— MACD 序列，**必须按 ts 升序**（递推/ref 都依赖顺序）
//	riseAt —— 可选：ts → 该根 ETH 涨跌幅%（没有就不填）
//	nowMs  —— 当前时间，用于判「这根收盘了吗」
func TakerSignalsFromMacd(rows []repo.TakerMacdRow, riseAt map[int64]float64, nowMs int64) []repo.TakerSignalRow {
	if len(rows) < 3 {
		return nil
	}
	rules := TakerSignalRules()
	out := make([]repo.TakerSignalRow, 0, 64)
	for i := 2; i < len(rows); i++ {
		cur := rows[i]
		// 收盘判据：这根走完了才认（见文件头纪律第二条）
		if cur.Ts+takerPanelBarMS > nowMs {
			continue
		}
		// ★ 连续性判据：最近三根必须**一根不差**（步长正好一根）。
		//   如果中间缺了一根（taker_vol 该切片没数据 / 新合约入池导致聚合缺口），
		//   rows[i-1] 就不是「上一根」而是「上上根」—— ref 静默指错根，
		//   报出来的信号根本不存在。宁可漏报也不能错报。
		if rows[i-1].Ts != cur.Ts-takerPanelBarMS || rows[i-2].Ts != cur.Ts-2*takerPanelBarMS {
			continue
		}
		for _, rule := range rules {
			val := TakerSignalVal(cur, rule)
			prev := TakerSignalVal(rows[i-1], rule)
			prev2 := TakerSignalVal(rows[i-2], rule)
			if !(val > 0 && prev < 0 && prev2 < 0) {
				continue
			}
			s := repo.TakerSignalRow{
				Bar: cur.Bar, Ts: cur.Ts, Rule: rule,
				Val: val, Prev: prev, Prev2: prev2, Ratio: cur.SrcVal,
			}
			if riseAt != nil {
				if v, ok := riseAt[cur.Ts]; ok {
					s.EthRise = v
					s.EthRiseOK = true
				}
			}
			out = append(out, s)
		}
	}
	return out
}

// TakerSignalStats 按口径统计条数（给日志用：一行看清三个口径各报了多少）
func TakerSignalStats(rows []repo.TakerSignalRow) map[string]int {
	m := make(map[string]int, 3)
	for _, r := range rows {
		m[r.Rule]++
	}
	return m
}
