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
}

// QueryTakerAgg 按 5m 切片聚合合约池的 taker 量。
//
// insts 为空表示不限合约；非空则按 IN 过滤 —— 调用方传的是「非美股非ETF 的
// 24h 成交额 TopN」池。用 IN 而不是 JOIN 临时表：池子只有 80 个，SQL 文本
// 长度可控（80 × 24 字符 ≈ 2KB），且 (bar, ts) 索引能直接吃到。
//
// TopInst 用 MySQL 的窗口函数不好直接和 GROUP BY 混用（会造成二次聚合），
// 所以拆两步：先按 ts 聚合总量，再单独按 ts 取总量最大的合约，Go 侧合并。
func (d *DB) QueryTakerAgg(insts []string, fromTs, toTs int64) ([]TakerAgg, error) {
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
	sb2.WriteString(`SELECT t.ts, t.inst_id, t.buy_vol, t.sell_vol, t.rise FROM (
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
	sb2.WriteString(`) t WHERE rn=1`)

	rows2, err := d.sql.Query(sb2.String(), args2...)
	if err != nil {
		return nil, err
	}
	defer rows2.Close()
	for rows2.Next() {
		var ts int64
		var inst string
		var b, s float64
		var rise sql.NullFloat64
		if err := rows2.Scan(&ts, &inst, &b, &s, &rise); err != nil {
			return nil, err
		}
		if a, ok := byTs[ts]; ok {
			a.TopInst, a.TopBuy, a.TopSell = inst, b, s
			a.TopRise = rise.Float64
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
