package service

// okxhistory.go —— 把 OKX 的「最近几天成交明细」合成到本地库
//
// ---------------------------------------------------------------------------
// 为什么需要
// ---------------------------------------------------------------------------
// 本地 trade / trade_event 表记录的是**程序自己下的单**。用户在 OKX 网页、
// App 上手工做的交易，本地库一无所知 —— 于是历史面板看起来永远只有寥寥几条。
//
// OKX 提供 /api/v5/trade/fills-history，保留最近 3 天的逐笔成交，
// 正好对上用户那句「历史持仓没有显示最近 3 天的持仓 / 拉最近三天的记录，保存」。
//
// 同步是幂等的：trade_event 上有唯一键 (inst_id, kind, ts)，
// 重复拉同一批成交只会 upsert，不会写重。

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"finally-main/internal/conf"
	"finally-main/internal/logx"
	"finally-main/internal/model"
	"finally-main/internal/repo"
)

// OKXFillRestPath 最近成交（保留 3 天）
const okxFillsPath = "/api/v5/trade/fills-history"

// okxFillsSyncInterval 同步间隔：5 分钟。OKX 该接口限频 10 次/2 秒，
// 5 分钟一次完全够用，也不会给 2 核机器添负担。
const okxFillsSyncInterval = 5 * time.Minute

// OKXFill 一笔成交
type OKXFill struct {
	InstID   string
	TradeID  string
	OrdID    string
	Side     string // buy / sell
	PosSide  string
	FillPx   float64
	FillSz   float64
	FillTime int64
	Fee      float64
	Pnl      float64
}

// FetchOKXFills 拉最近的成交明细（最多 3 页 × 100 条）
func FetchOKXFills(cli *OKXClient, pages int) ([]OKXFill, error) {
	if pages <= 0 {
		pages = 3
	}
	out := make([]OKXFill, 0, 300)
	before := "" // 向后翻页游标（tradeId）

	for i := 0; i < pages; i++ {
		path := fmt.Sprintf("%s?instType=SWAP&limit=100", okxFillsPath)
		if before != "" {
			path += "&after=" + before
		}
		raw, err := cli.Get(path, true)
		if err != nil {
			if i == 0 {
				return nil, err
			}
			break // 翻页中途失败就用已经拿到的
		}

		var rows []struct {
			InstID   string `json:"instId"`
			TradeID  string `json:"tradeId"`
			OrdID    string `json:"ordId"`
			Side     string `json:"side"`
			PosSide  string `json:"posSide"`
			FillPx   string `json:"fillPx"`
			FillSz   string `json:"fillSz"`
			FillTime string `json:"fillTime"`
			Fee      string `json:"fee"`
			Pnl      string `json:"pnl"`
		}
		if err := json.Unmarshal(raw, &rows); err != nil {
			return out, fmt.Errorf("解析成交明细失败：%w", err)
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			out = append(out, OKXFill{
				InstID: r.InstID, TradeID: r.TradeID, OrdID: r.OrdID,
				Side: r.Side, PosSide: r.PosSide,
				FillPx: atof(r.FillPx), FillSz: atof(r.FillSz),
				FillTime: atoi64(r.FillTime), Fee: atof(r.Fee), Pnl: atof(r.Pnl),
			})
		}
		before = rows[len(rows)-1].TradeID
		if len(rows) < 100 {
			break
		}
	}
	return out, nil
}

// FillToEvent 把一笔成交翻译成交易事件。
//
// 开仓 vs 平仓的判定：**只有平仓才产生已实现盈亏（pnl）**，
// 所以 pnl != 0 就是平仓。这比看 side 稳（做空时 sell 反而是开仓）。
//
// 关于 ctVal（合约面值）——这里踩过一次坑，务必别省：
//
//	OKX 的 fillSz 单位是「张」，不是「币」。一张 SAND-USDT-SWAP 值 10 个 SAND，
//	一张 BTC-USDT-SWAP 只值 0.01 个 BTC。少乘 ctVal 的话，
//	SAND 的「买入金额」会小一个数量级、BTC 反而大 100 倍，图上数字全是错的。
//
//	名义价值(USDT) = sz × ctVal × ctMult × px
//	保证金(USDT)   = 名义价值 ÷ 杠杆
func FillToEvent(f OKXFill, leverage int, ctVal, ctMult float64) model.TradeEventRow {
	kind := "open"
	if f.Pnl != 0 {
		kind = "close"
	}
	if ctVal <= 0 {
		ctVal = 1
	}
	if ctMult <= 0 {
		ctMult = 1
	}
	notional := f.FillPx * f.FillSz * ctVal * ctMult
	margin := 0.0
	if leverage > 0 {
		margin = notional / float64(leverage)
	}
	return model.TradeEventRow{
		InstID: f.InstID, Kind: kind, Ts: f.FillTime, Px: f.FillPx,
		Sz: f.FillSz, Margin: margin, Leverage: leverage,
		Pnl: f.Pnl, Reason: "OKX 成交明细同步",
		OrdID: f.OrdID, TradeID: atoi64(f.TradeID),
	}
}

// SyncOKXFills 拉一次成交明细并写进 trade_event，返回写入条数。
//
// 幂等：唯一键 (inst_id, kind, ts, trade_id) 冲突时只更新价格/张数/盈亏。
//
// 注意唯一键里**必须带 trade_id**：OKX 一笔大单会拆成多笔成交，
// 这些成交的 fillTime 常常落在同一毫秒上。只用 (inst_id, kind, ts) 的话，
// 同毫秒的后续成交会互相覆盖 —— 实测 100 笔成交只入库 49 条，
// 剩下的全被 ON DUPLICATE KEY UPDATE 吃掉了。
func SyncOKXFills(cli *OKXClient, store *repo.Store, leverage int) (int, error) {
	fills, err := FetchOKXFills(cli, 3)
	if err != nil {
		return 0, err
	}
	if len(fills) == 0 {
		return 0, nil
	}
	// 合约面值表：拿不到就退化成 ctVal=1（数字会偏，但不至于崩）
	ins := map[string]Instrument{}
	if m, e := cli.Instruments(false); e == nil && m != nil {
		ins = m
	}
	events := make([]model.TradeEventRow, 0, len(fills))
	for _, f := range fills {
		if f.FillTime <= 0 || f.InstID == "" {
			continue
		}
		ctVal, ctMult := 1.0, 1.0
		if it, ok := ins[f.InstID]; ok {
			ctVal, ctMult = it.CtVal, it.CtMult
		}
		events = append(events, FillToEvent(f, leverage, ctVal, ctMult))
	}
	if len(events) == 0 {
		return 0, nil
	}
	if err := store.Ingest(repo.StorePayload{Event: events}); err != nil {
		return 0, err
	}
	return len(events), nil
}

// StartOKXFillsSync 后台定时把 OKX 成交明细同步进本地库。
//
// 每次同步自己取一份 client/store（和 exitPass 一样的写法），所以它
// **不依赖自动交易是否开启** —— 用户只想在网页上看历史成交，也该有数据。
// 启动后先同步一次（让历史面板立刻有最近 3 天的记录），之后每 5 分钟一次。
func StartOKXFillsSync(ctx context.Context) {
	go func() {
		time.Sleep(25 * time.Second) // 错开启动高峰（那会儿回补、信号回算都在抢资源）
		for {
			syncOKXFillsOnce()
			select {
			case <-ctx.Done():
				return
			case <-time.After(okxFillsSyncInterval):
			}
		}
	}()
}

// syncOKXFillsOnce 同步一次（失败只记日志，不中断循环）
func syncOKXFillsOnce() {
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
	lever := cfg.Entry.Leverage
	if lever <= 0 {
		lever = 20
	}
	st := repo.NewStore(cfg)
	if n, err := SyncOKXFills(cli, st, lever); err != nil {
		logx.Logf("WARN", "[FILLS] 成交明细同步失败：%v", err)
	} else if n > 0 {
		logx.Logf("INFO", "[FILLS] 已同步最近成交明细 %d 笔到本地库", n)
	}
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func atof(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

func atoi64(s string) int64 {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}
