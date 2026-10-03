package service

// taker_us.go —— 美股/ETF 池的 takervol 总和 MACD（NQ 图第二个副图，「MACD2」）
//
// 用户口径（2026-10-03 二十二期·五，原话）：
//   「NQ 这个 macd2 数据绑定为所有美股+ETF 数据的每 5 分钟的 takervol 总和，
//    还是 12 26 60 参数，给我把这个 macd2 副图绑定到 NQ 的 macd 副图下面给
//    我呆着，还是数据库保存，绑定 NQ 主图 K 线数据，移动缩小放大都跟随主图，
//    和副图 1 的 macd 一样。」
//
// 拆成五条实现约束：
//   ① 池 = **所有**美股 + ETF（OKX 口径 inst_category='3'，本站实测 190 个），
//      不按成交额截断 —— 用户说的是「所有」，不是 TopN
//   ② 输入序列 = 该池每 5 分钟的 takervol 总和（SUM(buy) / SUM(sell)，
//      即聚合买卖比），与主图那套（加密池）**同一个聚合 SQL、同一个 MACD
//      算路**，只有池子不同
//   ③ 参数 12 / 26 / 60，与副图 1 完全一致
//   ④ 落库 taker_macd_us（读接口纯 SELECT，不实时聚合）
//   ⑤ 挂在 NQ 主图的 timeScale 上 —— 前端共享时间轴，缩放平移天然跟随
//
// ---------------------------------------------------------------------------
// 为什么原始数据复用 taker_vol 而不新开一张表
// ---------------------------------------------------------------------------
//   两个池的合约集合不重叠（category 1 加密 vs 3 美股/ETF），主键
//   (inst_id, bar, ts) 天然隔离：加密池聚合时传自己的 80 个 instID，
//   美股池传 190 个，互相看不见对方。于是回补 / 增量 / 水位线 / 清理
//   四条链路全部直接复用，不用维护第二份拷贝。
//
//   实测该池 190 个合约 30 天 ≈ 164 万行原始数据 —— 只落「聚合 + 指标」
//   （taker_macd_us，8640 行）就是几十 KB 与一两百 MB 的差别。

import (
	"fmt"
	"sort"
	"time"

	"finally-main/internal/logx"
	"finally-main/internal/repo"
)

// TakerUSCategory OKX instCategory：3 = 美股 / ETF
//
// （1 = 加密，4 = 商品；见 model.Instrument.InstCategory 的注释。）
const TakerUSCategory = "3"

// TakerUSPool 取「所有美股 + ETF」合约，按 instID 升序。
//
// ★ 不按成交额排序 / 截断：用户口径是「所有美股+ETF 数据」。即使某个标的
//   当天成交清淡，它也是这个池子的一员 —— 少一个就少一份总和。
//   排序只为稳定（同一份输入永远得到同一个池子，便于比对日志与复算）。
func TakerUSPool(insts []TakerPoolInput) []string {
	out := make([]string, 0, 256)
	for _, it := range insts {
		if it.InstCategory != TakerUSCategory {
			continue
		}
		out = append(out, it.InstID)
	}
	sort.Strings(out)
	return out
}

// TakerUSRebuild 重建美股/ETF 池的聚合 + MACD，写 taker_macd_us。
//
//   - d     数据库
//   - pool  美股/ETF 合约池（TakerUSPool 的产物），空则返回 0 不写
//   - days  窗口天数（一般 30）
//
// 返回写入行数。
//
// ★ 幂等：upsert，重复跑只会把同 ts 的行覆盖成最新算出来的值。
// ★ 整体重算而不是增量追加：EMA 是单向递推，理论上新增一根不改历史值，
//   但池子会变（新上美股合约会让最近几根的总和变大），整体重算能自动
//   把这种「历史值的口径悄悄变了」纠正回来（与 TakerPanelRebuild 同款纪律）。
func TakerUSRebuild(d *repo.DB, pool []string, days int) (int, error) {
	if len(pool) == 0 {
		return 0, nil
	}
	if days <= 0 {
		days = 30
	}
	nowMs := time.Now().UnixMilli()
	nowMs = nowMs / takerPanelBarMS * takerPanelBarMS
	fromMs := nowMs - int64(days)*24*3600*1000

	// withTop=false：美股池不需要「涨幅王」那一列（第二步要 JOIN kline
	// 再开窗口函数，190 合约 × 8640 根的成本没必要白付）。
	aggs, err := d.QueryTakerAgg(pool, fromMs, nowMs, false)
	if err != nil {
		return 0, fmt.Errorf("聚合美股池 taker 量失败：%w", err)
	}
	if len(aggs) == 0 {
		return 0, nil
	}

	src := make([]float64, len(aggs))
	for i, a := range aggs {
		if a.SellTotal > 0 {
			src[i] = a.BuyTotal / a.SellTotal
		}
	}
	// ★ 与加密池共用同一条 MACD 算路（macdCalcOn，见 taker_panel.go）——
	//   两张副图的参数与实现必须逐字一致，否则零轴位置对不上。
	dif, dea, hist := macdCalcOn(src, takerMacdFast, takerMacdSlow, takerMacdSignal)

	rows := make([]repo.TakerUSMacdRow, 0, len(aggs))
	for i, a := range aggs {
		rows = append(rows, repo.TakerUSMacdRow{
			Bar: takerBar, Ts: a.Ts,
			BuyTotal: a.BuyTotal, SellTotal: a.SellTotal,
			SrcVal: src[i], InstCount: a.InstCount,
			Dif: dif[i], Dea: dea[i], Hist: hist[i],
		})
	}
	n, err := d.UpsertTakerMacdUS(rows)
	if err != nil {
		return 0, fmt.Errorf("写 taker_macd_us 失败：%w", err)
	}
	return n, nil
}

// TakerUSEnsure 保证美股池预计算表「跟得上」，需要时**先同步原始数据再重算**。
//
// 与 TakerPanelEnsure 的差别（重要）：
//   加密池的原始量由 realtimeLoop 每 60 秒的 TakerSyncLatest 负责，
//   Ensure 只管「要不要重算」；
//   美股池有 190 个合约，每 60 秒打一轮 190 个请求是给 priapi 送人头
//   （实测 6 并发就整片 429）。所以把「同步」并入这里，并且用
//   **跨根判据**卡住频率：5m 数据 5 分钟才出一根，跨根之前一条查询就返回，
//   跨根那一次才真拉 190 个请求 —— 平均 0.63 请求/秒，很温柔。
//
// ★ 两种补数据路径（不能只留一种）：
//   连续（距上次只差 ≤3 根）→ TakerSyncLatest：只拉最近 2 小时，便宜；
//   有洞（服务停过 / 上次失败太久，相差 >3 根）→ TakerBackfillInsts：
//   按 30 天窗口整体重拉（幂等覆盖）。只用增量的话，服务停半天再起来，
//   中间那半天会永久缺一段总和，MACD2 的形状就废了。
//   阈值取 3 根 = 15 分钟：TakerSyncLatest 的窗口是 2 小时，远大于它，
//   不会出现「判成连续但增量够不到」的缝。
//
// 返回 (写入行数, 本轮是否真的干了活, error)。
func TakerUSEnsure(d *repo.DB, pool []string, days int, workers int) (int, bool, error) {
	if len(pool) == 0 {
		return 0, false, nil
	}
	_, maxTs, cnt, err := d.TakerMacdUSRange(takerBar)
	if err != nil {
		return 0, false, err
	}
	nowMs := time.Now().UnixMilli()
	nowMs = nowMs / takerPanelBarMS * takerPanelBarMS
	// 表里最新一根已是「当前这一根」→ 不用动
	if cnt > 0 && maxTs >= nowMs {
		return 0, false, nil
	}

	gap := nowMs - maxTs
	if cnt > 0 && gap > 3*takerPanelBarMS {
		logx.Logf("INFO", "[TAKER-US] 检测到数据缺口 %.0f 分钟（max_ts=%d），走 30 天整体重拉",
			float64(gap)/60000, maxTs)
		ok, rows := TakerBackfillInsts(d, pool, days, workers)
		logx.Logf("INFO", "[TAKER-US] 缺口重拉完成：%d/%d 个合约，%d 行", ok, len(pool), rows)
	} else if ok, rows := TakerSyncLatest(d, pool, workers); rows > 0 {
		logx.Logf("INFO", "[TAKER-US] 原始增量：%d/%d 个合约，%d 行", ok, len(pool), rows)
	}

	n, err := TakerUSRebuild(d, pool, days)
	if err != nil {
		return 0, true, err
	}
	logx.Logf("INFO", "[TAKER-US] 美股/ETF 池预计算已刷新：%d 行（池 %d 个合约，原 max_ts=%d）",
		n, len(pool), maxTs)
	return n, true, nil
}
