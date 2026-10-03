package service

// taker_panel.go —— taker 面板 & MACD 指标**预计算落库**
//
// 用户口径（2026-10-03）：
//   「当前的 takervol 面板的数据 都保存在数据库这样方便直接取用
//    到时候参数指标直接从数据库调用 不用查询」
//
// 于是把「面板四列 + 下一根涨跌幅」和「买卖比上的 MACD(12,26,60)」
// 全部算好写进 MySQL（taker_panel / taker_macd），接口只 SELECT。
//
// ---------------------------------------------------------------------------
// 为什么值得预计算（真实成本，不是拍脑袋）
// ---------------------------------------------------------------------------
//   面板一行 = 80 合约聚合 + JOIN kline 算涨幅 + 取涨幅王 + 再算它下一根。
//   实时算一次要：2 条聚合 SQL（跨 66 万行）+ 81 次 K 线查询（ETH + 每个
//   涨幅王各一次）+ Go 侧合并。
//   而数据本身**每 5 分钟才变一次**。前端却 30 秒轮询一次 —— 等于把同一份
//   结果重算 10 遍。落库后每次请求就一次 SELECT（8640 行，几十毫秒）。
//
// ---------------------------------------------------------------------------
// 口径纪律（沿用项目铁律）
// ---------------------------------------------------------------------------
//   · 买卖比 = 主动买量 ÷ 主动卖量，池 = 非美股非ETF 的 24h 成交额 TopN
//     （唯一入口 TakerPoolByVolume，与实时扫描同一批合约）
//   · 涨幅 = (c-o)/o*100，与 K 线图、信号里用的同一个公式
//   · 「下一根」= ts + 一根；还没生成时 ok=false（前端显示「—」而不是 0%）
//   · MACD 的输入序列 = 面板的 ratio（用户明确「把比例放到 macd 里面，
//     close 的值改成这个 takervol 值」），参数 12 / 26 / 60

import (
	"fmt"
	"sort"
	"time"

	"finally-main/internal/logx"
	"finally-main/internal/repo"
)

const (
	// takerMacdFast / Slow / Signal 用户指定参数（60 不是常见的 9）
	takerMacdFast   = 12
	takerMacdSlow   = 26
	takerMacdSignal = 60

	// TakerPanelMacdBar MACD 只挂 5 分钟（用户口径「5分钟才能显示这个指标」）
	TakerPanelMacdBar = "5m"

	takerPanelBarMS = int64(5 * 60 * 1000)
)

// TakerMacdFast / Slow / Signal 参数对外只读（handler 要回报给前端做图例，
// 写死在两处迟早不一致）
func TakerMacdFast() int   { return takerMacdFast }
func TakerMacdSlow() int   { return takerMacdSlow }
func TakerMacdSignal() int { return takerMacdSignal }

// TakerPanelRebuild 重建三张预计算表：面板 + MACD + 买入信号。
//
//   - d     数据库
//   - pool  候选池（非美股非ETF 24h 成交额 TopN），空则不限合约
//   - days  窗口天数（一般 30）
//
// 返回 (面板行数, MACD 行数, error)。
//
// ★ 三张表是一条流水线，不是三件独立的事：
//     taker_vol（原始买卖量）→ taker_panel（聚合 + 四列）
//                            → taker_macd（买卖比上的 MACD 12/26/60）
//                            → taker_signal（MACD 由负转正 = 买入信号）
//   信号必须在 MACD 落库之后立刻重扫，否则会出现「图上翻正了但没提醒」。
//
// ★ 幂等：两张表都是 upsert，重复跑不会重复插入，只会把同名点覆盖成
//   最新算出来的值（涨幅王会因为 K 线补齐而变化，必须能覆盖）。
func TakerPanelRebuild(d *repo.DB, pool []string, days int) (int, int, error) {
	if days <= 0 {
		days = 30
	}
	nowMs := time.Now().UnixMilli()
	nowMs = nowMs / takerPanelBarMS * takerPanelBarMS
	fromMs := nowMs - int64(days)*24*3600*1000

	aggs, err := d.QueryTakerAgg(pool, fromMs, nowMs)
	if err != nil {
		return 0, 0, fmt.Errorf("聚合 taker 量失败：%w", err)
	}
	if len(aggs) == 0 {
		return 0, 0, nil
	}

	// ---- ETH 下一根涨跌幅（第 3 列）----
	// ETH 是固定一个合约，单独查一次最省事（不进上面的 80 合约循环）。
	ethNext := takerKlineRiseMap(d, "ETH-USDT-SWAP", fromMs, nowMs)

	rows := make([]repo.TakerPanelRow, 0, len(aggs))
	for _, a := range aggs {
		r := repo.TakerPanelRow{
			Bar:       takerBar,
			Ts:        a.Ts,
			BuyTotal:  a.BuyTotal,
			SellTotal: a.SellTotal,
			InstCount: a.InstCount,
			TopInst:   a.TopInst,
			TopRise:   a.TopRise,
			// 涨幅王下一根：已在 QueryTakerAgg 的 SQL 里 JOIN 出来
			TopNextPct: a.TopNextRise,
			TopNextOk:  a.TopNextOK,
		}
		if a.SellTotal > 0 {
			r.Ratio = a.BuyTotal / a.SellTotal
		}
		// 下一根 = 本切片 + 一根
		if v, ok := ethNext[a.Ts+takerPanelBarMS]; ok {
			r.EthNextPct = v
			r.EthNextOk = true
		}
		rows = append(rows, r)
	}

	// 按 ts 升序（QueryTakerAgg 已保证，这里再兜一次，MACD 递推依赖顺序）
	sort.Slice(rows, func(i, j int) bool { return rows[i].Ts < rows[j].Ts })

	nPanel, err := d.UpsertTakerPanel(rows)
	if err != nil {
		return 0, 0, fmt.Errorf("写 taker_panel 失败：%w", err)
	}

	// ---- MACD(12,26,60) on ratio ----
	macdRows := takerMacdFromPanel(rows)
	nMacd, err := d.UpsertTakerMacd(macdRows)
	if err != nil {
		return nPanel, 0, fmt.Errorf("写 taker_macd 失败：%w", err)
	}

	// ---- 二十二期·四：买入信号（macd>0 and ref macd<0 and refref macd<0）----
	//
	// ★ 为什么放在这里而不是单独开一个后台任务：信号是 MACD 的**纯函数**——
	//   MACD 变了信号才可能变。同一份数据两条计算路径，迟早出现「图上翻正了
	//   但没报警」或者反过来。所以 MACD 一落库就顺手把信号重扫一遍。
	// ★ 成本：8640 根 × 3 个口径 = 2.6 万次比较，微秒级；落库的只有**命中**
	//   的那些根（信号点很少），不是 8640 行。
	// ★ 这里用**未对齐**的当前毫秒：判「这根收盘了吗」要的是真实时刻，
	//   拿上面那个对齐到 5m 边界的 nowMs 会把「正在走的这一根」当成已收盘。
	nowRaw := time.Now().UnixMilli()
	sigRows := TakerSignalsFromMacd(macdRows, TakerRiseAt(rows), nowRaw)
	if n, serr := d.UpsertTakerSignals(sigRows); serr != nil {
		logx.Logf("ERR", "[TAKER] 写买入信号失败：%v", serr)
	} else if n > 0 {
		// 日志只报「近两根」的条数（真正即时的那部分）；窗口内累计 30 天
		// 的量每 5 分钟打一遍是噪声，出问题要查的时候用接口看更准。
		fresh := 0
		for _, r := range sigRows {
			if r.Ts >= nowRaw-2*takerPanelBarMS {
				fresh++
			}
		}
		logx.Logf("INFO", "[TAKER] 买入信号已同步：窗口 %d 条 / 近两根 %d 条", n, fresh)
	}
	// 保留期：窗口外的信号删掉（与面板、MACD 表保持同一个窗口，
	// 否则信号表会只增不减 —— 这机器内存和磁盘都得省着用）。
	if n, perr := d.PurgeTakerSignalBefore(fromMs); perr != nil {
		logx.Logf("ERR", "[TAKER] 清理过期信号失败：%v", perr)
	} else if n > 0 {
		logx.Logf("INFO", "[TAKER] 清理过期信号 %d 条（早于窗口起点 %d）", n, fromMs)
	}
	return nPanel, nMacd, nil
}

// takerMacdFromPanel 用面板行的 ratio 序列算 MACD(12,26,60)。
//
// ★ 为什么用 ratio 而不是买卖量：用户明确「把比例放到 macd 指标里面，
//   close 的值改成这个 takervol 值」—— 输入就是那条比值线。
//
// ★ 逐段覆盖而不是整表重建：这里传入的就是完整窗口（最多 30 天 / 8640 根），
//   算完 upsert 即可。EMA 是单向递推，新增一根不会改历史值，所以
//   「只追加」和「整体重算」结果一致 —— 但整体重算更稳（万一中间有缺口，
//   递推会自己纠正回来）。
func takerMacdFromPanel(rows []repo.TakerPanelRow) []repo.TakerMacdRow {
	n := len(rows)
	if n == 0 {
		return nil
	}
	src := make([]float64, n)
	for i, r := range rows {
		src[i] = r.Ratio
	}

	dif := make([]float64, n)
	dea := make([]float64, n)
	hist := make([]float64, n)
	ef := make([]float64, n)
	es := make([]float64, n)
	// 复用指标层的 emaInto（与 K 线 MACD 同一份实现，口径不会漂）
	emaInto(ef, src, n, takerMacdFast)
	emaInto(es, src, n, takerMacdSlow)
	for i := 0; i < n; i++ {
		dif[i] = ef[i] - es[i]
	}
	emaInto(dea, dif, n, takerMacdSignal)
	for i := 0; i < n; i++ {
		// 与 macdHist 一致：hist = 2*(DIF-DEA)
		hist[i] = 2 * (dif[i] - dea[i])
	}

	out := make([]repo.TakerMacdRow, 0, n)
	for i, r := range rows {
		out = append(out, repo.TakerMacdRow{
			Bar: takerBar, Ts: r.Ts, SrcVal: src[i],
			Dif: dif[i], Dea: dea[i], Hist: hist[i],
		})
	}
	return out
}

// takerKlineRiseMap 取某合约 5m K 线 → {ts: 该根涨跌幅%}
//
// 与 handler 的 takerNextPctMap 同公式 (c-o)/o*100。
// 放在 service 是为了让「预计算」和「接口」共用同一份实现
// —— 两处各写一份迟早会漂。
func takerKlineRiseMap(d *repo.DB, instID string, fromMs, toMs int64) map[int64]float64 {
	rows, err := d.QueryKlines(repo.KlineQuery{
		InstID: instID, Bar: takerBar,
		FromTs: fromMs, ToTs: toMs + takerPanelBarMS,
		Asc: true,
	})
	if err != nil {
		return nil
	}
	m := make(map[int64]float64, len(rows))
	for _, k := range rows {
		if k.O > 0 {
			m[k.Ts] = (k.C - k.O) / k.O * 100
		}
	}
	return m
}

// TakerPanelEnsure 启动时保证预计算表「跟得上」。
//
// 判据不是「表里有没有行」，而是「最新一根是不是本根」——
// 服务重启时表里往往有昨天算的行，但已经过期了，必须重算补齐。
// 这与 K 线回补的「水位线判已铺够」是同一套思路。
//
// ★ 二十二期·四改过判据（原来是 maxTs < now-2 根）：
//   老判据要等**过了 2 根**才重算，等于面板、MACD、买入信号全都慢 2 根
//   （最多 15 分钟）。买入提醒的原料就是这张表 —— 慢 15 分钟的信号等于没有。
//   现在改成「只要有新的一根开始走就重算」：60 秒轮询在每根收盘后 1 分钟内
//   必然撞上一次重建，那一根的数据已经定稿（TakerSyncLatest 拿的是收盘值），
//   MACD 与信号随之定稿，提醒延迟 ≈ 1 分钟。
//   代价是重算频率从 ~15 分钟一次变成 ~5 分钟一次（同一份聚合 SQL 多跑两遍），
//   实测秒级、与「每 60 秒重算」的旧隐患相比仍是数量级的节省。
func TakerPanelEnsure(d *repo.DB, pool []string, days int) (int, int, bool, error) {
	_, maxTs, cnt, err := d.TakerPanelRange(takerBar)
	if err != nil {
		return 0, 0, false, err
	}
	nowMs := time.Now().UnixMilli()
	nowMs = nowMs / takerPanelBarMS * takerPanelBarMS
	// 表里最新一根不是「当前这一根」→ 过期，重算
	stale := cnt == 0 || maxTs < nowMs
	if !stale {
		return 0, 0, false, nil
	}
	n, m, err := TakerPanelRebuild(d, pool, days)
	if err != nil {
		return 0, 0, true, err
	}
	logx.Logf("INFO", "[TAKER] 预计算表已刷新：面板 %d 行 / MACD %d 行（原 max_ts=%d）", n, m, maxTs)
	return n, m, true, nil
}
