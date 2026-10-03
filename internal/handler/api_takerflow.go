package handler

// api_takerflow.go —— taker 买卖流向面板接口（**读预计算表**）
//
// 面板要的四列（用户口径，2026-10-03；第 4 列口径当日二次变更）：
//   1. 时间              —— 5 分钟切片
//   2. 5m takervol 总买卖比 —— 整个候选池（非美股非ETF、24h 成交额前 80）加总
//   3. ETH 下一根涨跌幅    —— 该切片之后一根 5m 的 ETH 涨跌幅
//   4. 该切片「涨幅最高」的合约 —— 合约名 + 它当根涨幅 + 它在下一根的涨跌幅
//      （★ 原口径「成交量最大」已按用户要求改为「涨幅最高」：同一个 5 分钟里
//      谁涨得最狠显示谁，例如 ETH 涨 2% 比谁都高就显示 ETH）
//
// ---------------------------------------------------------------------------
// ★ 2026-10-03 改造：不再实时算，改读 MySQL 预计算表
// ---------------------------------------------------------------------------
// 用户口径：「当前的 takervol 面板的数据 都保存在数据库这样方便直接取用
// 到时候参数指标直接从数据库调用 不用查询」。
//
// 所以本文件从「聚合 + 拼装」变成「SELECT 出来直接返回」。
// 真正算的地方在 service.TakerPanelRebuild（后台跑，见 backfill.go）：
//   taker_panel  ← 四列 + 下一根（每 5 分钟一根）
//   taker_macd   ← 买卖比上的 MACD(12,26,60)
//
// 保留「表空则回落实时算」的分支：首启动那几秒后台可能还没算完，
// 这时候面板不该是空白 —— 宁可慢一点也得有数据（且与后台算出同一口径，
// 因为两边调的是同一个 repo.QueryTakerAgg）。
//
// ★ 为什么要「下一根」的涨跌幅（前瞻对齐）：
//   面板的用途是「看这 5 分钟的买卖比，接下来一根发生了什么」。如果把同一
//   根的涨跌幅贴在旁边，那只是把 K 线和量并排，没有信息量。所以第 3、4 列
//   的时间轴比第 2 列**晚一根**，这一点必须在前端表头上写清楚（「→ 下一根」），
//   否则用户会以为数据错位了。

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"finally-main/internal/repo"
	"finally-main/internal/service"
)

// takerflowCache 面板结果缓存
//
// 现在数据源已经是预计算表（一次 SELECT），严格说缓存不是必需的了；
// 但 8640 行 JSON 有 ~800KB，30 秒轮询一次还是省下不少序列化与网络开销。
// 20 秒 TTL：5m 切片本身 5 分钟才更新一根，完全不影响实时性。
type takerflowCache struct {
	mu    sync.Mutex
	at    time.Time
	last  int64 // 上次读到的最后一根 ts，用于无需重读的快速判定
	limit int   // ★ 上次窗口大小：缓存命中要求请求窗口 ≤ 已读窗口
	days  int   // ★ days 也截窗口，同规则
	val   []takerFlowRow
	cand  []string
}

// takerFlowRow 面板一行
type takerFlowRow struct {
	Ts int64 `json:"ts"` // 切片起始时间（毫秒）

	// 第 2 列：全池买卖比
	BuyTotal  float64 `json:"buyTotal"`
	SellTotal float64 `json:"sellTotal"`
	Ratio     float64 `json:"ratio"`     // 买 / 卖
	InstCount int     `json:"instCount"` // 参与该切片的合约数

	// 第 3 列：ETH 下一根涨跌幅（%）
	EthNextPct float64 `json:"ethNextPct"`
	EthNextOk  bool    `json:"ethNextOk"` // 下一根 K 线是否已生成

	// 第 4 列：该切片当根涨幅最高的合约
	//   TopRise = 它当根的涨幅（挑选依据）；TopNextPct = 它下一根的涨跌幅
	TopInst    string  `json:"topInst"`
	TopRise    float64 `json:"topRise"`
	TopNextPct float64 `json:"topNextPct"`
	TopNextOk  bool    `json:"topNextOk"`
}

// handleTakerFlow GET /api/takerflow?limit=&days=
//
// limit：返回最近多少个 5m 切片（★ 默认 288×30 = 30 天全量，用户口径
// 「30天的数据要全部带上」——不再默认只给 10 天）
// 返回按时间**降序**（最新在上，直接喂分页表格）
func (s *Server) handleTakerFlow(w http.ResponseWriter, r *http.Request) (any, error) {
	q := r.URL.Query()
	limit := 288 * 30
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 288*30 {
		limit = 288 * 30
	}
	days := 30
	if v := q.Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			days = n
		}
	}
	if days > 30 {
		days = 30
	}

	nowMs := time.Now().UnixMilli()
	nowMs = nowMs / 300000 * 300000

	// 候选池：只用于「表空时回落实时算」以及响应里回报池大小
	cand := s.takerCandidatePool()

	// 缓存命中（同池 + 未跨根 + 请求窗口不超过已读窗口）
	//
	// ★ 窗口判定（2026-10-03 踩坑）：缓存里存的是「按上次 limit 读出来的
	//   全量升序 rows」。若上次按 10 天读、这次要 30 天，直接复用会静默
	//   少 20 天数据 —— 复现路径：面板轮询（2880）后 20 秒内点「加载全部
	//   30 天」（8640），实测 slices 从 8640 缩成 2883。所以命中条件必须
	//   加上 limit <= 已读窗口；反过来小请求复用大缓存没问题（resp 按
	//   limit 从尾巴截取）。
	s.tk.mu.Lock()
	if !s.tk.at.IsZero() && time.Since(s.tk.at) < 20*time.Second &&
		s.tk.last >= nowMs-300000 && s.tk.last > 0 && len(s.tk.val) > 0 &&
		limit <= s.tk.limit && days <= s.tk.days {
		cached := s.tk.val
		s.tk.mu.Unlock()
		return s.takerFlowResp(cached, cand, limit, days), nil
	}
	s.tk.mu.Unlock()

	fromMs := nowMs - int64(limit+2)*300000
	// ★ 窗口不能超过保留期
	if fromMs < nowMs-int64(days)*24*3600*1000 {
		fromMs = nowMs - int64(days)*24*3600*1000
	}

	rows, err := s.takerPanelRows(cand, fromMs, nowMs)
	if err != nil {
		return nil, err
	}

	// 缓存
	s.tk.mu.Lock()
	s.tk.at = time.Now()
	s.tk.last = nowMs
	s.tk.limit = limit
	s.tk.days = days
	s.tk.val = rows
	s.tk.cand = cand
	s.tk.mu.Unlock()

	return s.takerFlowResp(rows, cand, limit, days), nil
}

// takerPanelRows 读 taker_panel；表空时回落实时计算。
//
// 实时兜底只在「预计算表完全没数据」时触发（首启动的几秒），
// 一旦后台算过就永远走快路径。
func (s *Server) takerPanelRows(cand []string, fromMs, toMs int64) ([]takerFlowRow, error) {
	prs, err := s.db.QueryTakerPanel(service.TakerPanelMacdBar, fromMs, toMs)
	if err != nil {
		return nil, err
	}
	if len(prs) > 0 {
		out := make([]takerFlowRow, 0, len(prs))
		for _, p := range prs {
			out = append(out, takerFlowRow{
				Ts:         p.Ts,
				BuyTotal:   p.BuyTotal,
				SellTotal:  p.SellTotal,
				Ratio:      p.Ratio,
				InstCount:  p.InstCount,
				EthNextPct: p.EthNextPct,
				EthNextOk:  p.EthNextOk,
				TopInst:    p.TopInst,
				TopRise:    p.TopRise,
				TopNextPct: p.TopNextPct,
				TopNextOk:  p.TopNextOk,
			})
		}
		return out, nil
	}
	// ---- 兜底：表空 → 实时算一次（首启动窗口期）----
	return s.takerFlowLive(cand, fromMs, toMs)
}

// takerFlowLive 实时计算（预计算表还没铺好时的兜底）。
//
// 与 service.TakerPanelRebuild 用的是同一个 repo.QueryTakerAgg，
// 所以兜底结果和预计算结果口径完全一致，不会出现「刷新一下数字就变了」。
func (s *Server) takerFlowLive(cand []string, fromMs, toMs int64) ([]takerFlowRow, error) {
	aggs, err := s.db.QueryTakerAgg(cand, fromMs, toMs)
	if err != nil {
		return nil, err
	}
	ethNext := s.takerNextPctMap("ETH-USDT-SWAP", fromMs, toMs)

	out := make([]takerFlowRow, 0, len(aggs))
	for _, a := range aggs {
		row := takerFlowRow{
			Ts:         a.Ts,
			BuyTotal:   a.BuyTotal,
			SellTotal:  a.SellTotal,
			InstCount:  a.InstCount,
			TopInst:    a.TopInst,
			TopRise:    a.TopRise,
			TopNextPct: a.TopNextRise,
			TopNextOk:  a.TopNextOK,
		}
		if a.SellTotal > 0 {
			row.Ratio = a.BuyTotal / a.SellTotal
		}
		if v, ok := ethNext[a.Ts+300000]; ok {
			row.EthNextPct = v
			row.EthNextOk = true
		}
		out = append(out, row)
	}
	return out, nil
}

// takerFlowResp 组装响应（倒序取前 limit 条）
func (s *Server) takerFlowResp(rows []takerFlowRow, cand []string, limit, days int) map[string]any {
	out := make([]takerFlowRow, 0, limit)
	// rows 已是升序，从尾巴往头取
	for i := len(rows) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, rows[i])
	}
	// 汇总（用户明确要「都加起来，总和是多少」）
	var totBuy, totSell float64
	for _, r := range rows {
		totBuy += r.BuyTotal
		totSell += r.SellTotal
	}
	sum := map[string]any{
		"buyTotal":  totBuy,
		"sellTotal": totSell,
		"ratio":     0.0,
		"slices":    len(rows),
	}
	if totSell > 0 {
		sum["ratio"] = totBuy / totSell
	}
	return map[string]any{
		"ok":       true,
		"rows":     out,
		"summary":  sum,
		"poolSize": len(cand),
		"days":     days,
		"bar":      "5m",
		// 口径说明直接给前端，表头/悬浮框要用
		"note": "买:卖 = 主动买量:主动卖量；池=非美股非ETF 24h成交额前80；第4列=该切片涨幅最高合约，显示其当根涨幅与下一根涨跌幅",
	}
}

// takerCandidatePool 候选池：非美股非ETF（instCategory=1），按 24h 成交额取前 80
//
// 与实时扫描的候选池口径一致（scanner.go 阶段 2），保证面板和交易看的是同一批合约。
// ★ 排序与筛选逻辑放在 service.TakerPoolByVolume（唯一入口），
//   cmd 层的 taker 同步回调用的是同一个函数 —— 两处口径不可能漂移。
func (s *Server) takerCandidatePool() []string {
	insts, err := s.db.ListInstruments()
	if err != nil {
		return nil
	}
	tks, err := s.db.ListTickers()
	if err != nil {
		return nil
	}
	vol := make(map[string]float64, len(tks))
	for _, tk := range tks {
		vol[tk.InstID] = tk.QuoteVol24h
	}
	topN := s.cfg().TopNByVolume
	if topN <= 0 {
		topN = 80
	}
	// Instrument 来自 repo 包，service 侧要的是轻量输入结构
	conv := make([]service.TakerPoolInput, 0, len(insts))
	for _, it := range insts {
		conv = append(conv, service.TakerPoolInput{
			InstID:       it.InstID,
			InstCategory: it.InstCategory,
		})
	}
	return service.TakerPoolByVolume(conv, vol, topN)
}

// takerNextPctMap 取某合约 5m K 线，返回 {ts: 该根涨跌幅%}
//
// 与 service.RisePct 同公式：(c-o)/o*100
func (s *Server) takerNextPctMap(instID string, fromMs, toMs int64) map[int64]float64 {
	rows, err := s.db.QueryKlines(repo.KlineQuery{
		InstID: instID, Bar: "5m",
		FromTs: fromMs, ToTs: toMs + 300000,
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
