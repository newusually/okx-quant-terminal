package repo

// taker_repo.go —— taker 买卖量（主动买 / 主动卖）仓储
//
// 表结构见 mysql.go 的 taker_vol DDL。与 kline 的差别：
//   - 只有 5m 一个周期（用户口径：面板固定看 5 分钟切片）
//   - src 字段区分精度来源：'5m' = 真 5 分钟，'1Hfill' = 1 小时前向填充
//
// 消费方是 /api/takerflow（面板），查询形态固定为
// 「某段时间窗内、某个合约池、按 ts 聚合」，所以二级索引建 (bar, ts)。
//
// 历史精度约束（实测，别改代码试图绕过）：
//   OKX priapi indicators 的 takerBuySellVol 只能回溯 5 天（1439 根 5m），
//   更早的 25 天只能用 1H 粒度前向填充。详见 service/taker.go 顶部注释。

import (
	"database/sql"
	"strings"
	"time"
)

// TakerVol 一行 taker 量
type TakerVol struct {
	InstID  string
	Bar     string
	Ts      int64
	BuyVol  float64
	SellVol float64
	Src     string
}

// takerCols taker_vol 表列序
var takerCols = []string{"inst_id", "bar", "ts", "buy_vol", "sell_vol", "src"}

// takerUpdateCols 冲突时覆盖的字段
//
// ★ src 放进覆盖列表是有意的：同一根 K 线先用 1Hfill 填过，之后拿到真 5m
// 数据时必须能**升级**精度（1Hfill → 5m）。如果 src 不覆盖，就会出现
// 「先填的粗数据永远压着后来的真数据」——这正是本项目「归一化反压显式值」
// 那类坑的同构形态。
var takerUpdateCols = []string{"buy_vol", "sell_vol", "src"}

// UpsertTakerVols 批量写入（幂等：主键相同则覆盖）
func (d *DB) UpsertTakerVols(rows []TakerVol) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	args := make([][]any, 0, len(rows))
	for _, r := range rows {
		if r.InstID == "" || r.Bar == "" || r.Ts <= 0 {
			continue
		}
		if r.Src == "" {
			r.Src = "5m"
		}
		args = append(args, []any{r.InstID, r.Bar, r.Ts, r.BuyVol, r.SellVol, r.Src})
	}
	if len(args) == 0 {
		return 0, nil
	}
	return d.bulkUpsert("taker_vol", takerCols, args, takerUpdateCols)
}

// TakerAgg 一个 5m 切片上的聚合结果
//
//   - BuyTotal / SellTotal：该切片内**整个合约池**的买量 / 卖量总和
//   - TopInst / TopBuy / TopSell / TopRise：该切片内「当根涨幅最高」的合约
//     （★ 2026-10-03 口径变更：原来是成交量最大，用户改为涨幅最高；
//     TopRise = 它当根的涨幅%，作为「为什么选它」的依据一并返回）
type TakerAgg struct {
	Ts        int64
	BuyTotal  float64
	SellTotal float64
	InstCount int
	TopInst   string
	TopBuy    float64
	TopSell   float64
	TopRise   float64
	// TopNextRise 涨幅王**下一根**的涨跌幅（%）；TopNextOK=false 表示下一根还没生成
	//
	// ★ 二十二期：从「handler 里对每个 TopInst 各查一次 K 线」改成
	//   在第二条 SQL 里 LEFT JOIN 一张 ts+barMs 的 kline 直接取。
	//   原来 80 个涨幅王就要 80 次查询，30 天窗口下是 81 次往返；
	//   现在固定 2 条 SQL。这就是「预计算」能省下来的真实成本。
	TopNextRise float64
	TopNextOK   bool
}

// QueryTakerAgg 按 5m 切片聚合合约池的 taker 量。
//
// insts 为空表示不限合约；非空则按 IN 过滤 —— 调用方传的是「非美股非ETF 的
// 24h 成交额 TopN」池。用 IN 而不是 JOIN 临时表：池子只有 80 个，SQL 文本
// 长度可控（80 × 24 字符 ≈ 2KB），且 (bar, ts) 索引能直接吃到。
//
// TopInst 用 MySQL 的窗口函数不好直接和 GROUP BY 混用（会造成二次聚合），
// 所以拆两步：先按 ts 聚合总量，再单独按 ts 取总量最大的合约，Go 侧合并。
//
// ★ withTop=false 只跑第一步（按 ts 聚合总量）。第二个消费方是**美股/ETF 池
//   的 MACD2**（见 taker_macd_us）：它只要「每 5m 的 takervol 总和」，不需要
//   涨幅王 —— 而第二步要 JOIN kline 再开窗口函数，190 个合约 × 8640 根
//   的成本没必要白付。用参数而不是另写一条 SQL，是为了让两个消费方**共用
//   同一段聚合 SQL 文本**（同一个量只有一条计算路径，见项目铁律）。
func (d *DB) QueryTakerAgg(insts []string, fromTs, toTs int64, withTop bool) ([]TakerAgg, error) {
	// ---- 第一步：按 ts 聚合总量 ----
	sb := strings.Builder{}
	sb.WriteString(`SELECT ts, SUM(buy_vol), SUM(sell_vol), COUNT(DISTINCT inst_id)
	                FROM taker_vol WHERE bar='5m'`)
	args := []any{}
	if len(insts) > 0 {
		sb.WriteString(" AND inst_id IN (" + placeholders(len(insts)) + ")")
		for _, s := range insts {
			args = append(args, s)
		}
	}
	if fromTs > 0 {
		sb.WriteString(" AND ts>=?")
		args = append(args, fromTs)
	}
	if toTs > 0 {
		sb.WriteString(" AND ts<=?")
		args = append(args, toTs)
	}
	sb.WriteString(" GROUP BY ts ORDER BY ts ASC")

	rows, err := d.sql.Query(sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byTs := make(map[int64]*TakerAgg, 512)
	order := make([]int64, 0, 512)
	for rows.Next() {
		var a TakerAgg
		if err := rows.Scan(&a.Ts, &a.BuyTotal, &a.SellTotal, &a.InstCount); err != nil {
			return nil, err
		}
		byTs[a.Ts] = &a
		order = append(order, a.Ts)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(order) == 0 {
		return []TakerAgg{}, nil
	}
	if !withTop {
		out := make([]TakerAgg, 0, len(order))
		for _, ts := range order {
			out = append(out, *byTs[ts])
		}
		return out, nil
	}

	// ---- 第二步：每个 ts 取「当根涨幅最高的合约」----
	//
	// ★ 口径变更（2026-10-03）：原来是「成交量（买+卖）最大的合约」，
	//   用户要求改成「涨幅最高的合约」——
	//   面板的用途是「这 5 分钟里池子里谁最强，它下一根还涨不涨」，
	//   所以判据应该是价格涨幅，不是成交量。
	//
	// 涨幅必须和 kline 联表算（(c-o)/o），taker_vol 表里没有价格。
	// JOIN 条件用 (inst_id, ts)，两边 (inst_id, bar, ts) 主键前缀都能吃到。
	//
	// ★ 用 kline 的 5m 收盘价算，与 /api/takerflow 第 3 列 ETH 的算法完全一致
	//   （同一个 (c-o)/o 公式），否则「最高合约的涨幅」和「ETH 涨幅」两个
	//   数字会来自两套口径，并列显示时对不上。
	sb2 := strings.Builder{}
	// ★ 第二条查询同时把「涨幅王下一根的涨跌幅」LEFT JOIN 出来。
	//   下一根 = 同一合约、ts + 一根(300000ms)。LEFT JOIN 而不是 JOIN：
	//   最新那根的下一根还没生成，JOIN 会把它整行丢掉，面板就会少最后一行。
	sb2.WriteString(`SELECT t.ts, t.inst_id, t.buy_vol, t.sell_vol, t.rise,
	                          (k2.c - k2.o) / NULLIF(k2.o, 0) * 100 AS next_rise,
	                          k2.ts AS next_ts
	                   FROM (
	                   SELECT v.ts AS ts, v.inst_id AS inst_id, v.buy_vol AS buy_vol, v.sell_vol AS sell_vol,
	                          (k.c - k.o) / NULLIF(k.o, 0) * 100 AS rise,
	                          ROW_NUMBER() OVER (PARTITION BY v.ts ORDER BY (k.c - k.o) / NULLIF(k.o, 0) DESC, v.inst_id ASC) AS rn
	                   FROM taker_vol v
	                   JOIN kline k ON k.inst_id = v.inst_id AND k.ts = v.ts AND k.bar = '5m'
	                   WHERE v.bar='5m'`)
	args2 := []any{}
	if len(insts) > 0 {
		sb2.WriteString(" AND v.inst_id IN (" + placeholders(len(insts)) + ")")
		for _, s := range insts {
			args2 = append(args2, s)
		}
	}
	if fromTs > 0 {
		sb2.WriteString(" AND v.ts>=?")
		args2 = append(args2, fromTs)
	}
	if toTs > 0 {
		sb2.WriteString(" AND v.ts<=?")
		args2 = append(args2, toTs)
	}
	// 300000 = 一根 5m。表里 bar 恒为 '5m'，所以这里写死是安全的（也顺手
	// 传进参数位，避免将来有人把 takerBar 改成别的周期时忘了同步）。
	sb2.WriteString(`) t
	                   LEFT JOIN kline k2
	                     ON k2.inst_id = t.inst_id AND k2.bar = '5m' AND k2.ts = t.ts + ?
	                   WHERE rn=1`)
	args2 = append(args2, int64(300000))

	rows2, err := d.sql.Query(sb2.String(), args2...)
	if err != nil {
		return nil, err
	}
	defer rows2.Close()
	for rows2.Next() {
		var ts int64
		var inst string
		var b, s float64
		var rise, nextRise sql.NullFloat64
		var nextTs sql.NullInt64
		if err := rows2.Scan(&ts, &inst, &b, &s, &rise, &nextRise, &nextTs); err != nil {
			return nil, err
		}
		if a, ok := byTs[ts]; ok {
			a.TopInst, a.TopBuy, a.TopSell = inst, b, s
			a.TopRise = rise.Float64
			// 下一根存在才算 ok：nextTs 为 NULL 说明还没生成
			if nextTs.Valid && nextTs.Int64 > 0 {
				a.TopNextRise = nextRise.Float64
				a.TopNextOK = true
			}
		}
	}
	if err := rows2.Err(); err != nil {
		return nil, err
	}

	out := make([]TakerAgg, 0, len(order))
	for _, ts := range order {
		out = append(out, *byTs[ts])
	}
	return out, nil
}

// TakerRange 查某合约某周期的已有数据范围（回补水位线用）
func (d *DB) TakerRange(instID, bar string) (minTs, maxTs, cnt int64, err error) {
	row := d.sql.QueryRow(
		`SELECT COALESCE(MIN(ts),0), COALESCE(MAX(ts),0), COUNT(*) FROM taker_vol WHERE inst_id=? AND bar=?`,
		instID, bar)
	err = row.Scan(&minTs, &maxTs, &cnt)
	if err == sql.ErrNoRows {
		return 0, 0, 0, nil
	}
	return
}

// TakerState 读水位线
func (d *DB) TakerState(instID, bar string) (minTs, maxTs, cnt int64, src string, ok bool) {
	row := d.sql.QueryRow(
		`SELECT min_ts, max_ts, rows_cnt, src FROM taker_scan_state WHERE inst_id=? AND bar=?`, instID, bar)
	if err := row.Scan(&minTs, &maxTs, &cnt, &src); err != nil {
		return 0, 0, 0, "", false
	}
	return minTs, maxTs, cnt, src, true
}

// SaveTakerState 写水位线
func (d *DB) SaveTakerState(instID, bar string, minTs, maxTs, cnt int64, src string, updatedAt int64) error {
	_, err := d.sql.Exec(
		`INSERT INTO taker_scan_state (inst_id,bar,min_ts,max_ts,rows_cnt,src,updated_at)
		 VALUES (?,?,?,?,?,?,?)
		 ON DUPLICATE KEY UPDATE min_ts=VALUES(min_ts), max_ts=VALUES(max_ts),
		   rows_cnt=VALUES(rows_cnt), src=VALUES(src), updated_at=VALUES(updated_at)`,
		instID, bar, minTs, maxTs, cnt, src, updatedAt)
	return err
}

// PurgeTakerVolBefore 按时间删老数据（保留窗口用）
func (d *DB) PurgeTakerVolBefore(ts int64) (int64, error) {
	res, err := d.sql.Exec(`DELETE FROM taker_vol WHERE ts < ?`, ts)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// placeholders 生成 "?,?,?" 
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

/* ==========================================================================
 * 二十二期：预计算结果表（taker_panel / taker_macd）
 *
 * 用户口径原话：「当前的 takervol 面板的数据 都保存在数据库这样方便直接取用
 * 到时候参数指标直接从数据库调用 不用查询」。
 *
 * 所以面板每次请求**不再实时聚合**，改成：
 *   后台 TakerPanelRebuild() 算好写入 taker_panel（四列 + 下一根）
 *                         与 taker_macd（买卖比上的 MACD 12/26/60）
 *   接口 SELECT 出来直接返回。
 *
 * 下方 QueryTakerAgg（实时聚合）保留 —— 它是重建的**计算内核**，
 * 不能删；发布路径改成只读表。
 * ========================================================================== */

// TakerPanelRow taker_panel 一行（= 面板表格的一行，字段与前端一一对应）
type TakerPanelRow struct {
	Bar        string
	Ts         int64
	BuyTotal   float64
	SellTotal  float64
	Ratio      float64
	InstCount  int
	EthNextPct float64
	EthNextOk  bool
	TopInst    string
	TopRise    float64
	TopNextPct float64
	TopNextOk  bool
}

// takerPanelCols 列序（与 DDL 保持一致）
var takerPanelCols = []string{
	"bar", "ts", "buy_total", "sell_total", "ratio", "inst_count",
	"eth_next_pct", "eth_next_ok", "top_inst", "top_rise", "top_next_pct", "top_next_ok", "updated_at",
}

// UpsertTakerPanel 批量写面板行（幂等：同 (bar,ts) 覆盖）
//
// ★ 覆盖所有业务列：面板行是「算出来的快照」，后算的必须能盖掉先算的
//   （例如涨幅王的 K 线补齐后 top_inst 会变）。只覆盖部分列会残留旧值。
func (d *DB) UpsertTakerPanel(rows []TakerPanelRow) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	now := time.Now().UnixMilli()
	args := make([][]any, 0, len(rows))
	for _, r := range rows {
		if r.Bar == "" || r.Ts <= 0 {
			continue
		}
		args = append(args, []any{
			r.Bar, r.Ts, r.BuyTotal, r.SellTotal, r.Ratio, r.InstCount,
			r.EthNextPct, boolToTiny(r.EthNextOk), r.TopInst, r.TopRise,
			r.TopNextPct, boolToTiny(r.TopNextOk), now,
		})
	}
	if len(args) == 0 {
		return 0, nil
	}
	return d.bulkUpsert("taker_panel", takerPanelCols, args, []string{
		"buy_total", "sell_total", "ratio", "inst_count",
		"eth_next_pct", "eth_next_ok", "top_inst", "top_rise",
		"top_next_pct", "top_next_ok", "updated_at",
	})
}

// QueryTakerPanel 读面板行（升序），bar 固定传 "5m"
func (d *DB) QueryTakerPanel(bar string, fromTs, toTs int64) ([]TakerPanelRow, error) {
	sb := strings.Builder{}
	sb.WriteString(`SELECT bar, ts, buy_total, sell_total, ratio, inst_count,
	                       eth_next_pct, eth_next_ok, top_inst, top_rise, top_next_pct, top_next_ok
	                FROM taker_panel WHERE bar=?`)
	args := []any{bar}
	if fromTs > 0 {
		sb.WriteString(" AND ts>=?")
		args = append(args, fromTs)
	}
	if toTs > 0 {
		sb.WriteString(" AND ts<=?")
		args = append(args, toTs)
	}
	sb.WriteString(" ORDER BY ts ASC")

	rows, err := d.sql.Query(sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TakerPanelRow, 0, 1024)
	for rows.Next() {
		var r TakerPanelRow
		var ethOk, topOk int
		if err := rows.Scan(&r.Bar, &r.Ts, &r.BuyTotal, &r.SellTotal, &r.Ratio, &r.InstCount,
			&r.EthNextPct, &ethOk, &r.TopInst, &r.TopRise, &r.TopNextPct, &topOk); err != nil {
			return nil, err
		}
		r.EthNextOk = ethOk != 0
		r.TopNextOk = topOk != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// TakerPanelRange 面板表已有的时间范围与行数（判「要不要重建」用）
func (d *DB) TakerPanelRange(bar string) (minTs, maxTs, cnt int64, err error) {
	row := d.sql.QueryRow(
		`SELECT COALESCE(MIN(ts),0), COALESCE(MAX(ts),0), COUNT(*) FROM taker_panel WHERE bar=?`, bar)
	err = row.Scan(&minTs, &maxTs, &cnt)
	if err == sql.ErrNoRows {
		return 0, 0, 0, nil
	}
	return
}

// TakerMacdRow taker_macd 一行
//
// SrcVal = MACD 的输入序列（= 买卖比 ratio），存下来是为了：
//   ① 前端副图能把「比值线」和「DIF/DEA」画在一起做对照；
//   ② 换参数重建时不用回头再查 taker_panel。
type TakerMacdRow struct {
	Bar    string
	Ts     int64
	SrcVal float64
	Dif    float64
	Dea    float64
	Hist   float64
}

var takerMacdCols = []string{"bar", "ts", "src_val", "dif", "dea", "hist", "updated_at"}

// UpsertTakerMacd 批量写 MACD 行（幂等）
func (d *DB) UpsertTakerMacd(rows []TakerMacdRow) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	now := time.Now().UnixMilli()
	args := make([][]any, 0, len(rows))
	for _, r := range rows {
		if r.Bar == "" || r.Ts <= 0 {
			continue
		}
		args = append(args, []any{r.Bar, r.Ts, r.SrcVal, r.Dif, r.Dea, r.Hist, now})
	}
	if len(args) == 0 {
		return 0, nil
	}
	return d.bulkUpsert("taker_macd", takerMacdCols, args,
		[]string{"src_val", "dif", "dea", "hist", "updated_at"})
}

// QueryTakerMacd 读 MACD 序列（升序）
func (d *DB) QueryTakerMacd(bar string, fromTs, toTs int64) ([]TakerMacdRow, error) {
	sb := strings.Builder{}
	sb.WriteString(`SELECT bar, ts, src_val, dif, dea, hist FROM taker_macd WHERE bar=?`)
	args := []any{bar}
	if fromTs > 0 {
		sb.WriteString(" AND ts>=?")
		args = append(args, fromTs)
	}
	if toTs > 0 {
		sb.WriteString(" AND ts<=?")
		args = append(args, toTs)
	}
	sb.WriteString(" ORDER BY ts ASC")

	rows, err := d.sql.Query(sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TakerMacdRow, 0, 1024)
	for rows.Next() {
		var r TakerMacdRow
		if err := rows.Scan(&r.Bar, &r.Ts, &r.SrcVal, &r.Dif, &r.Dea, &r.Hist); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

/* ==========================================================================
 * 二十二期·五：美股/ETF 池的 takervol 总和 MACD（taker_macd_us）
 *
 * 用户口径：「NQ 的 macd2 绑定为所有美股+ETF 数据的每 5 分钟的 takervol
 * 总和，还是 12 26 60 参数，绑定在 NQ 的 macd 副图下面，还是数据库保存」。
 *
 * 与 taker_macd 结构几乎一样，多存了 buy_total / sell_total / inst_count ——
 * 因为这张表的输入序列本身就是「全池买卖量的总和」，把总和落库才能：
 *   ① 换 MACD 参数时不用回头再聚合一次 190 个合约；
 *   ② 前端图例能直接显示「这 5 分钟全池买了多少 / 卖了多少」。
 *
 * ★ 为什么可以和 taker_macd 合表加个 pool 列却偏要分表：
 *   两张表的**生命周期不同** —— 加密池跟着 80 合约的 taker_vol 走，
 *   美股池跟着 190 合约的池子走，重建频率、保留期、失败影响面都不一样。
 *   合表会让「美股池拉挂了」直接污染主图副图的数据（本项目铁律：
 *   一个数据源出问题不许牵连另一条链路）。
 * ========================================================================== */

// TakerUSMacdRow taker_macd_us 一行（= 美股/ETF 池每 5m 的聚合 + MACD）
type TakerUSMacdRow struct {
	Bar       string
	Ts        int64
	BuyTotal  float64
	SellTotal float64
	// SrcVal = MACD 的输入序列（= 全池买卖比 buy_total/sell_total）
	SrcVal    float64
	InstCount int
	Dif       float64
	Dea       float64
	Hist      float64
}

var takerUSMacdCols = []string{
	"bar", "ts", "buy_total", "sell_total", "src_val", "inst_count",
	"dif", "dea", "hist", "updated_at",
}

// UpsertTakerMacdUS 批量写美股池 MACD 行（幂等：同 (bar,ts) 覆盖全业务列）
//
// ★ 覆盖所有业务列而不是只覆盖指标：池子会变（新上美股合约、老的下线），
//   某一根的总和会随池子变化而变化，只覆盖 dif/dea/hist 会留下旧的总和值。
func (d *DB) UpsertTakerMacdUS(rows []TakerUSMacdRow) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	now := time.Now().UnixMilli()
	args := make([][]any, 0, len(rows))
	for _, r := range rows {
		if r.Bar == "" || r.Ts <= 0 {
			continue
		}
		args = append(args, []any{
			r.Bar, r.Ts, r.BuyTotal, r.SellTotal, r.SrcVal, r.InstCount,
			r.Dif, r.Dea, r.Hist, now,
		})
	}
	if len(args) == 0 {
		return 0, nil
	}
	return d.bulkUpsert("taker_macd_us", takerUSMacdCols, args, []string{
		"buy_total", "sell_total", "src_val", "inst_count",
		"dif", "dea", "hist", "updated_at",
	})
}

// QueryTakerMacdUS 读美股池 MACD 序列（升序）
func (d *DB) QueryTakerMacdUS(bar string, fromTs, toTs int64) ([]TakerUSMacdRow, error) {
	sb := strings.Builder{}
	sb.WriteString(`SELECT bar, ts, buy_total, sell_total, src_val, inst_count, dif, dea, hist
	                FROM taker_macd_us WHERE bar=?`)
	args := []any{bar}
	if fromTs > 0 {
		sb.WriteString(" AND ts>=?")
		args = append(args, fromTs)
	}
	if toTs > 0 {
		sb.WriteString(" AND ts<=?")
		args = append(args, toTs)
	}
	sb.WriteString(" ORDER BY ts ASC")

	rows, err := d.sql.Query(sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TakerUSMacdRow, 0, 1024)
	for rows.Next() {
		var r TakerUSMacdRow
		if err := rows.Scan(&r.Bar, &r.Ts, &r.BuyTotal, &r.SellTotal, &r.SrcVal,
			&r.InstCount, &r.Dif, &r.Dea, &r.Hist); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TakerMacdUSRange 美股池 MACD 表已有的时间范围与行数（判「要不要重建」用）
func (d *DB) TakerMacdUSRange(bar string) (minTs, maxTs, cnt int64, err error) {
	row := d.sql.QueryRow(
		`SELECT COALESCE(MIN(ts),0), COALESCE(MAX(ts),0), COUNT(*) FROM taker_macd_us WHERE bar=?`, bar)
	err = row.Scan(&minTs, &maxTs, &cnt)
	if err == sql.ErrNoRows {
		return 0, 0, 0, nil
	}
	return
}

/* ==========================================================================
 * 二十二期·四：taker MACD 买入信号（taker_signal）
 *
 * 判据（用户原话）：「macd>0 and ref macd<0 and refref macd<0」——
 * 当前根 > 0、前一根 < 0、前两根 < 0，即由负转正的上穿。
 *
 * 表里存的是**判定结果 + 判定依据**（val/prev/prev2 三个数都留着），
 * 这样前端弹提醒时能把「为什么报」原样写出来，排查时也不用回头再算。
 * ========================================================================== */

// TakerSignalRow taker_signal 一行
type TakerSignalRow struct {
	Bar       string
	Ts        int64
	Rule      string // dif / dea / hist
	Val       float64
	Prev      float64
	Prev2     float64
	Ratio     float64 // 触发那根的买卖比（MACD 的输入值）
	EthRise   float64 // 触发那根 ETH 的涨跌幅%（(c-o)/o*100）
	EthRiseOK bool
}

var takerSignalCols = []string{
	"bar", "ts", "rule", "val", "prev", "prev2", "ratio",
	"eth_rise", "eth_rise_ok", "created_at",
}

// UpsertTakerSignals 批量写信号（幂等：同 (bar,ts,rule) 覆盖）
//
// ★ 覆盖业务列：同一根的 MACD 会因为「补进来更老的 K 线 / 重算窗口」
//   而变化，后算的必须能盖掉先算的（否则会残留上一次的取值）。
func (d *DB) UpsertTakerSignals(rows []TakerSignalRow) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	now := time.Now().UnixMilli()
	args := make([][]any, 0, len(rows))
	for _, r := range rows {
		if r.Bar == "" || r.Ts <= 0 || r.Rule == "" {
			continue
		}
		args = append(args, []any{
			r.Bar, r.Ts, r.Rule, r.Val, r.Prev, r.Prev2, r.Ratio,
			r.EthRise, boolToTiny(r.EthRiseOK), now,
		})
	}
	if len(args) == 0 {
		return 0, nil
	}
	return d.bulkUpsert("taker_signal", takerSignalCols, args, []string{
		"val", "prev", "prev2", "ratio", "eth_rise", "eth_rise_ok", "created_at",
	})
}

// QueryTakerSignals 读信号。
//
//	fromTs > 0 → 返回 (fromTs, toTs] 区间内**升序**的最多 limit 条
//	             （前端轮询用：只要比上次看到的更新，就是新信号）
//	fromTs == 0 → 返回**最新** limit 条（前端首屏用：初始化「已看到」水位线）
//
// ★ 两个分支方向不同不是随意写的：轮询必须从旧到新（否则同一轮多条信号
//   的提醒顺序会倒过来）；首屏必须从新往回取（要的是最近这批，不是最早这批）。
func (d *DB) QueryTakerSignals(bar, rule string, fromTs, toTs int64, limit int) ([]TakerSignalRow, error) {
	if limit <= 0 {
		limit = 50
	}
	sb := strings.Builder{}
	sb.WriteString(`SELECT bar, ts, rule, val, prev, prev2, ratio, eth_rise, eth_rise_ok
	                FROM taker_signal WHERE bar=? AND rule=?`)
	args := []any{bar, rule}
	if fromTs > 0 {
		sb.WriteString(" AND ts>?")
		args = append(args, fromTs)
	}
	if toTs > 0 {
		sb.WriteString(" AND ts<=?")
		args = append(args, toTs)
	}
	if fromTs > 0 {
		sb.WriteString(" ORDER BY ts ASC")
	} else {
		sb.WriteString(" ORDER BY ts DESC")
	}
	sb.WriteString(" LIMIT ?")
	args = append(args, limit)

	rows, err := d.sql.Query(sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TakerSignalRow, 0, limit)
	for rows.Next() {
		var r TakerSignalRow
		var ok int
		if err := rows.Scan(&r.Bar, &r.Ts, &r.Rule, &r.Val, &r.Prev, &r.Prev2,
			&r.Ratio, &r.EthRise, &ok); err != nil {
			return nil, err
		}
		r.EthRiseOK = ok != 0
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 首屏分支是倒序取的，这里翻回升序 —— 调用方（前端）只需要按时间正序
	// 消费，不该关心 SQL 是正着查还是倒着查。
	if fromTs <= 0 {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, nil
}

// TakerSignalRange 信号表的时间范围与条数（诊断 / 覆盖率展示用）
func (d *DB) TakerSignalRange(bar, rule string) (minTs, maxTs, cnt int64, err error) {
	row := d.sql.QueryRow(
		`SELECT COALESCE(MIN(ts),0), COALESCE(MAX(ts),0), COUNT(*) FROM taker_signal WHERE bar=? AND rule=?`,
		bar, rule)
	err = row.Scan(&minTs, &maxTs, &cnt)
	if err == sql.ErrNoRows {
		return 0, 0, 0, nil
	}
	return
}

// PurgeTakerSignalBefore 删掉早于 ts 的信号（保留期清理）
func (d *DB) PurgeTakerSignalBefore(ts int64) (int64, error) {
	res, err := d.sql.Exec(`DELETE FROM taker_signal WHERE ts < ?`, ts)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// boolToTiny bool → MySQL TINYINT(0/1)
func boolToTiny(b bool) int {
	if b {
		return 1
	}
	return 0
}
