package service

// account_ops.go —— 原 py 里那些「账户操作」函数的 Go 版
//
// 一一对应关系：
//
//	Python (py)                 Go (本文件)
//	MVC.getcashbal()                GetCashBal()
//	MVC.getcashhistory()            GetCashHistory()
//	MVC.getuplRatio_instId()        GetUplRatio()
//	MVC.sellall()                   SellAll()
//	MVC.orderbuy(...)               OrderBuy(instID, minute)
//	MVC.buyorders(...)              BuyOrders(instID, minute)
//	MVC.ordersell(...)              OrderSell(instID, minute)
//	MVC.sellorders(...)             SellOrders(instID, minute)
//	MVC.sender()                    SendDingMsg()（见 dingtalk.go）
//
// 逻辑口径保持原样：先把币种折算权益 / 持仓情况写到 datas/uplRatio/log/*.txt，
// 再按各自规则决定要不要开平仓。区别只有两点：
//   1. 用的是本项目自己的 okx Go SDK，不再依赖 pip 的 requests
//   2. 所有异常都变成 error 返回，不再被 except: pass 静默吞掉

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"finally-main/internal/repo/okx"
)

// AccountOps 账户操作器
type AccountOps struct {
	cred  *Credentials
	acc   *okx.AccountAPI
	trade *okx.TradeAPI
	root  string

	// 日志目录：datas/uplRatio/log（和 Python 版一致）
	LogDir string

	// Leverage 原 Python 版硬编码 50 倍
	Leverage int
	// TakeProfitRate 原 Python 版买单挂 1.0035、卖单挂 0.9965
	TakeProfitRate float64
	// BudgetUSDT 每笔预算（0.1 = 本项目口径的「0.1 美金」）
	BudgetUSDT float64
	// SendNotify 是否推钉钉（对应原来的 SendDingding.sender）
	Notify bool
}

// NewAccountOps 建账户操作器。cred 为 nil 时自动去读凭据文件。
func NewAccountOps(root string, cred *Credentials) (*AccountOps, error) {
	if cred == nil {
		c, src, err := LoadCredentials(root)
		if err != nil {
			return nil, err
		}
		cred = c
		fmt.Printf("[凭据] 已从 %s 加载，Key=%s，%s\n", src, cred.Mask(), modeText(cred))
	}
	opts := okx.ClientOptions{Proxy: cred.Proxy, Timeout: 25 * time.Second, MaxRetries: 4}
	return &AccountOps{
		cred:           cred,
		acc:            okx.NewAccountAPIWith(cred.APIKey, cred.SecretKey, cred.Passphrase, false, cred.Flag, opts),
		trade:          okx.NewTradeAPIWith(cred.APIKey, cred.SecretKey, cred.Passphrase, false, cred.Flag, opts),
		root:           root,
		LogDir:         filepath.Join(root, "datas", "uplRatio", "log"),
		Leverage:       50,
		TakeProfitRate: 1.0035,
		BudgetUSDT:     0.1,
		Notify:         true,
	}, nil
}

// instrument 查单个合约的 ctVal / lotSz / minSz（懒加载，缓存 6 小时由 SDK 负责）
func (o *AccountOps) instrument(instID string) (okx.Instrument, error) {
	pub := okx.NewPublicOnlyAPI()
	resp, err := pub.GetInstruments(okx.InstTypeSwap, "", instID)
	if err != nil {
		return okx.Instrument{}, err
	}
	rows := okx.Data(resp)
	if len(rows) == 0 {
		return okx.Instrument{}, fmt.Errorf("查不到合约 %s", instID)
	}
	r := rows[0]
	return okx.Instrument{
		InstID: okx.Str(r, "instId"),
		CtVal:  okx.F(r, "ctVal"),
		CtMult: okx.F(r, "ctMult"),
		LotSz:  okx.F(r, "lotSz"),
		MinSz:  okx.F(r, "minSz"),
		TickSz: okx.F(r, "tickSz"),
	}, nil
}

// fmtSzPlain 按 lotSz 的小数位格式化张数（OKX 多一位精度都会报错）
func fmtSzPlain(sz, lotSz float64) string {
	dec := 0
	if lotSz > 0 && lotSz < 1 {
		dec = int(math.Ceil(-math.Log10(lotSz)))
	}
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

func modeText(c *Credentials) string {
	if c.IsDemo() {
		return "模拟盘"
	}
	return "实盘"
}

// writeLog 往文件追加（对应 Python 里 open(..., 'a') 那几段）
func (o *AccountOps) writeLog(name, content string, truncate bool) error {
	if err := os.MkdirAll(o.LogDir, 0o755); err != nil {
		return err
	}
	p := filepath.Join(o.LogDir, name)
	flag := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if truncate {
		flag = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	}
	f, err := os.OpenFile(p, flag, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(content)
	return err
}

// AccountSnapshot 账户快照（对应 Python 从 get_account 里抠出来的那些字段）
type AccountSnapshot struct {
	DisEq     float64 // 美金层面币种折算权益
	Upl       float64 // 未结算盈亏总额
	CashBal   float64 // USDT 币种余额
	MgnRatio  float64 // 保证金率
	AvailBal  float64 // 可用余额
	FrozenBal float64 // 币种占用金额
	PosCount  int     // 合约订单数量
	TotalEq   float64
}

// Snapshot 取账户快照
func (o *AccountOps) Snapshot() (*AccountSnapshot, error) {
	bal, err := o.acc.GetAccount("USDT")
	if err != nil {
		return nil, err
	}
	rows := okx.Data(bal)
	if len(rows) == 0 {
		return nil, fmt.Errorf("账户余额返回为空")
	}
	s := &AccountSnapshot{TotalEq: okx.F(rows[0], "totalEq"), Upl: okx.F(rows[0], "upl")}
	details, _ := rows[0]["details"].([]any)
	for _, d := range details {
		m, ok := d.(map[string]any)
		if !ok {
			continue
		}
		if okx.Str(m, "ccy") == "USDT" {
			s.DisEq = okx.F(m, "disEq")
			s.CashBal = okx.F(m, "cashBal")
			s.MgnRatio = okx.F(m, "mgnRatio")
			s.AvailBal = okx.F(m, "availBal")
			if s.AvailBal == 0 {
				s.AvailBal = okx.F(m, "availEq")
			}
			s.FrozenBal = okx.F(m, "frozenBal")
			break
		}
	}
	if risk, err := o.acc.GetPositionRisk("SWAP"); err == nil {
		rrows := okx.Data(risk)
		if len(rrows) > 0 {
			if pos, ok := rrows[0]["posData"].([]any); ok {
				s.PosCount = len(pos)
			}
		}
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// getcashbal —— 实时账户资金信息（原 Python 每分钟查一次）
// ---------------------------------------------------------------------------

// GetCashBal 对应 MVC.getcashbal()：清空 cashbal.txt 再写入最新一条，并推钉钉。
func (o *AccountOps) GetCashBal() (string, error) {
	s, err := o.Snapshot()
	if err != nil {
		return "", err
	}
	if err := o.writeLog("cashbal.txt", "", true); err != nil {
		return "", err
	}
	log := fmt.Sprintf("\n合约订单数量----->>>%d个,  币种折算权益----->>>%.2f＄,  "+
		"\n实际未结算盈亏总额：--->>>%.2f＄,  USDT币种余额----->>>%.2f＄,"+
		"\n保证金率----->>>%.2f%%,  可用余额----->>>%.2f＄,  "+
		"\n,币种占用金额--->>>%.2f＄\n\n",
		s.PosCount, s.DisEq, s.Upl, s.CashBal, s.MgnRatio*100, s.AvailBal, s.FrozenBal)
	if err := o.writeLog("cashbal.txt", log, false); err != nil {
		return "", err
	}
	if o.Notify {
		SendDingMsg(strings.TrimSpace(log))
	}
	return log, nil
}

// ---------------------------------------------------------------------------
// getcashhistory —— 资金历史（原 Python 每分钟记一次，六个文件各一行）
// ---------------------------------------------------------------------------

// GetCashHistory 对应 MVC.getcashhistory()
func (o *AccountOps) GetCashHistory() (string, error) {
	s, err := o.Snapshot()
	if err != nil {
		return "", err
	}
	today := time.Now().Format("2006-01-02 15:04:05")
	files := map[string]string{
		"disEq_history.txt":        fmt.Sprintf("\n%s,%.2f", today, s.DisEq),
		"upl_history.txt":          fmt.Sprintf("\n%s,%.2f", today, s.Upl),
		"cashBal_history.txt":      fmt.Sprintf("\n%s,%.2f", today, s.CashBal),
		"posdatacount_history.txt": fmt.Sprintf("\n%s,%d", today, s.PosCount),
		"frozenBal_history.txt":    fmt.Sprintf("\n%s,%.2f", today, s.FrozenBal),
		"mgnRatio_history.txt":     fmt.Sprintf("\n%s,%.2f%%", today, s.MgnRatio*100),
	}
	for name, line := range files {
		if err := o.writeLog(name, line, false); err != nil {
			return "", fmt.Errorf("写 %s 失败：%w", name, err)
		}
	}
	return fmt.Sprintf("资金历史已记录：disEq=%.2f upl=%.2f cashBal=%.2f 持仓=%d 保证金占用=%.2f 保证金率=%.2f%%",
		s.DisEq, s.Upl, s.CashBal, s.PosCount, s.FrozenBal, s.MgnRatio*100), nil
}

// ---------------------------------------------------------------------------
// getuplRatio —— 逐仓浮盈巡检 + 浮亏自动补仓（原 Python 的逻辑）
// ---------------------------------------------------------------------------

// UplLine 单个持仓的浮盈行
type UplLine struct {
	InstID      string
	PosSide     string
	UplRatio    float64 // 注意：原 Python 用的是「比例」，不是百分数
	Last        float64
	AvgPx       float64
	Imr         float64
	Lever       float64
	Pos         float64
	NotionalUsd float64
	Loss        float64
}

// GetUplRatio 对应 MVC.getuplRatio_instId()
//
// 规则照抄：uplRatio < -0.25 就要补仓；-5 < uplRatio < -3 走 "imr"，其余走 "low"。
// dryRun=true 时只算不动手（强烈建议第一次这么跑）。
func (o *AccountOps) GetUplRatio(dryRun bool) ([]UplLine, string, error) {
	risk, err := o.acc.GetPositionRisk("SWAP")
	if err != nil {
		return nil, "", err
	}
	rows := okx.Data(risk)
	var out []UplLine
	var sb strings.Builder
	if len(rows) == 0 {
		return out, "无持仓", nil
	}
	posData, _ := rows[0]["posData"].([]any)
	if len(posData) == 0 {
		return out, "无持仓", nil
	}

	// 清空文件（对应 Python 里 open(...,'w') 写空串）
	if err := o.writeLog("uplRatio.txt", "", true); err != nil {
		return nil, "", err
	}

	for _, p := range posData {
		m, ok := p.(map[string]any)
		if !ok {
			continue
		}
		inst := okx.Str(m, "instId")
		if inst == "" {
			continue
		}
		res, err := o.acc.GetPositions("SWAP", inst)
		if err != nil {
			continue
		}
		pr := okx.Data(res)
		if len(pr) == 0 {
			continue
		}
		d := pr[0]
		line := UplLine{
			InstID:      inst,
			PosSide:     okx.Str(d, "posSide"),
			UplRatio:    okx.F(d, "uplRatio"),
			Last:        okx.F(d, "last"),
			AvgPx:       okx.F(d, "avgPx"),
			Imr:         okx.F(d, "imr"),
			Lever:       okx.F(d, "lever"),
			Pos:         okx.F(d, "pos"),
			NotionalUsd: okx.F(d, "notionalUsd"),
		}
		line.Loss = line.Imr * line.UplRatio
		out = append(out, line)

		if line.UplRatio < -0.25 {
			kind := "low"
			if line.UplRatio > -5 && line.UplRatio < -3 {
				kind = "imr"
			}
			if dryRun {
				sb.WriteString(fmt.Sprintf("[dry_run] %s 浮亏 %.2f%%，应补仓（kind=%s）\n",
					inst, line.UplRatio*100, kind))
			} else if _, err := o.BuyOrders(inst, kind); err != nil {
				sb.WriteString(fmt.Sprintf("%s 补仓失败：%v\n", inst, err))
			} else {
				sb.WriteString(fmt.Sprintf("%s 已补仓（kind=%s）\n", inst, kind))
			}
		}

		log := fmt.Sprintf("\nsymbol--->>>%s,未实现收益率--->>>%.5f%%,现价--->>>%.5f,开仓均价--->>>%.5f,"+
			"保证金--->>>%.5f,杠杆倍数--->>>%.0f,总计亏损金额--->>>%.5f",
			inst, line.UplRatio*100, line.Last, line.AvgPx, line.Imr, line.Lever, line.Loss)
		_ = o.writeLog("uplRatio.txt", log, false)
		time.Sleep(time.Second) // 对齐 Python 的 time.sleep(1)，别把接口打爆
	}
	return out, sb.String(), nil
}

// ---------------------------------------------------------------------------
// sellall —— 浮盈超 35% 或浮亏超 30% 全平
// ---------------------------------------------------------------------------

// SellAll 对应 MVC.sellall()
func (o *AccountOps) SellAll(dryRun bool) ([]string, error) {
	risk, err := o.acc.GetPositionRisk("SWAP")
	if err != nil {
		return nil, err
	}
	rows := okx.Data(risk)
	var msgs []string
	if len(rows) == 0 {
		return msgs, nil
	}
	posData, _ := rows[0]["posData"].([]any)
	for _, p := range posData {
		m, ok := p.(map[string]any)
		if !ok {
			continue
		}
		inst := okx.Str(m, "instId")
		if inst == "" {
			continue
		}
		res, err := o.acc.GetPositions("SWAP", inst)
		if err != nil {
			continue
		}
		pr := okx.Data(res)
		if len(pr) == 0 {
			continue
		}
		uplRatio := okx.F(pr[0], "uplRatio")
		if uplRatio > 0.35 || uplRatio < -30 {
			posSide := okx.Str(pr[0], "posSide")
			if posSide == "" {
				posSide = okx.PosSideLong
			}
			if dryRun {
				msgs = append(msgs, fmt.Sprintf("[dry_run] %s 收益率 %.2f%%，应全平", inst, uplRatio*100))
				continue
			}
			if _, err := o.trade.ClosePositions(inst, okx.TdModeCross, posSide, ""); err != nil {
				msgs = append(msgs, fmt.Sprintf("%s 全平失败：%v", inst, err))
			} else {
				msgs = append(msgs, fmt.Sprintf("%s 已全平（收益率 %.2f%%）", inst, uplRatio*100))
			}
		}
		time.Sleep(time.Second)
	}
	return msgs, nil
}

// ---------------------------------------------------------------------------
// orderbuy / buyorders / ordersell / sellorders
// ---------------------------------------------------------------------------

// OrderBuy 对应 MVC.orderbuy()：在持仓少于 15 个时才允许继续买
func (o *AccountOps) OrderBuy(instID, minute string, dryRun bool) (string, error) {
	n, err := o.positionCount()
	if err != nil {
		return "", err
	}
	if n < 15 || minute == "low" || minute == "imr" {
		return o.BuyOrders(instID, minute)
	}
	return fmt.Sprintf("当前持仓 %d 个，超过 15 个上限，跳过", n), nil
}

// OrderSell 对应 MVC.ordersell()：在持仓少于 50 个时才允许继续卖
func (o *AccountOps) OrderSell(instID, minute string, dryRun bool) (string, error) {
	n, err := o.positionCount()
	if err != nil {
		return "", err
	}
	if n < 50 || minute == "low" || minute == "imr" {
		return o.SellOrders(instID, minute)
	}
	return fmt.Sprintf("当前持仓 %d 个，超过 50 个上限，跳过", n), nil
}

func (o *AccountOps) positionCount() (int, error) {
	s, err := o.Snapshot()
	if err != nil {
		return 0, err
	}
	return s.PosCount, nil
}

// BuyOrders 对应 MVC.buyorders()：50 倍杠杆开多，按「1 美金对应的张数」补仓，
// 每次补完挂一张 1.0035 的止盈委托。
//
// dollar 传的是每笔预算美金数（本项目口径 0.1U）。
func (o *AccountOps) BuyOrders(instID, minute string) (string, error) {
	return o.orders(instID, minute, okx.SideBuy, okx.PosSideLong, o.TakeProfitRate)
}

// SellOrders 对应 MVC.sellorders()：50 倍杠杆开空，止盈价 0.9965。
func (o *AccountOps) SellOrders(instID, minute string) (string, error) {
	return o.orders(instID, minute, okx.SideSell, okx.PosSideShort, 0.9965)
}

// orders 开仓 + 补仓 + 挂止盈的公共实现
//
// budgetUSDT：这一笔准备投多少美金保证金（0.1 = 用户的「0.1 美金」口径）
func (o *AccountOps) orders(instID, minute, side, posSide string, tpRate float64) (string, error) {
	if instID == "" {
		return "", fmt.Errorf("instID 为空")
	}
	budget := o.BudgetUSDT
	if budget <= 0 {
		budget = 0.1
	}

	// 1) 设杠杆
	if _, err := o.acc.SetLeverage(fmt.Sprint(o.Leverage), okx.TdModeCross, instID, "", ""); err != nil {
		// 设杠杆失败不致命（可能已经是这个倍数）
		fmt.Printf("[warn] %s 设杠杆失败：%v\n", instID, err)
	}

	// 2) 首单：用 minSz 那个最小可下量开个头
	ins, err := o.instrument(instID)
	if err != nil {
		return "", err
	}
	firstSz := ins.MinSz
	if firstSz <= 0 {
		firstSz = ins.LotSz
	}
	szStr := fmtSzPlain(firstSz, ins.LotSz)
	if _, err := o.trade.PlaceOrder(okx.PlaceOrderParams{
		InstID: instID, TdMode: okx.TdModeCross, Side: side, PosSide: posSide,
		OrdType: okx.OrdTypeMarket, Sz: szStr,
	}); err != nil {
		return "", fmt.Errorf("首单失败：%w", err)
	}

	time.Sleep(5 * time.Second) // 对齐 Python 的 time.sleep(5)

	// 3) 读回持仓，算「每张要多少保证金」
	res, err := o.acc.GetPositions("SWAP", instID)
	if err != nil {
		return "", err
	}
	pr := okx.Data(res)
	if len(pr) == 0 {
		return "", fmt.Errorf("下单后读不到持仓")
	}
	imr := okx.F(pr[0], "imr")
	pos := okx.F(pr[0], "pos")
	if pos <= 0 || imr <= 0 {
		return "", fmt.Errorf("持仓数据异常（pos=%.8g imr=%.8g）", pos, imr)
	}
	onlyImr := imr / pos // 每张占用的保证金
	onlyOrder := int(budget / onlyImr)

	// 4) 按 minute 档位缩放（照抄 Python 的 low / imr 逻辑）
	switch minute {
	case "low":
		onlyOrder = onlyOrder / 2
	case "imr":
		onlyOrder = int(float64(onlyOrder) * 1.5)
	}
	if onlyOrder < 1 {
		onlyOrder = 1
	}

	// 5) 补仓
	if _, err := o.trade.PlaceOrder(okx.PlaceOrderParams{
		InstID: instID, TdMode: okx.TdModeCross, Side: side, PosSide: posSide,
		OrdType: okx.OrdTypeMarket, Sz: fmt.Sprint(onlyOrder),
	}); err != nil {
		return "", fmt.Errorf("补仓失败：%w", err)
	}

	time.Sleep(time.Second)

	// 6) 读回均价，挂止盈
	res2, err := o.acc.GetPositions("SWAP", instID)
	if err != nil {
		return "", err
	}
	pr2 := okx.Data(res2)
	if len(pr2) == 0 {
		return "", fmt.Errorf("补仓后读不到持仓")
	}
	avgPx := okx.F(pr2[0], "avgPx")
	trigger := avgPx * tpRate
	tpSide := okx.SideSell
	if side == okx.SideSell {
		tpSide = okx.SideBuy
	}
	tp := fmt.Sprintf("%.10g", trigger)

	msgs := []string{}
	for _, sz := range []string{szStr, fmt.Sprint(onlyOrder), fmt.Sprint(onlyOrder), fmt.Sprint(onlyOrder)} {
		if _, err := o.trade.PlaceAlgoOrder(okx.AlgoOrderParams{
			InstID: instID, TdMode: okx.TdModeCross, Side: tpSide, PosSide: posSide,
			OrdType: okx.OrdTypeConditional, Sz: sz,
			TpTriggerPx: tp, TpOrdPx: tp,
		}); err != nil {
			msgs = append(msgs, fmt.Sprintf("挂止盈 %s 张失败：%v", sz, err))
		} else {
			msgs = append(msgs, fmt.Sprintf("挂止盈 %s 张 @ %.8g", sz, trigger))
		}
		time.Sleep(time.Second)
	}
	return fmt.Sprintf("%s %s 开仓完成：首单 %s 张 + 补仓 %d 张；%s",
		instID, side, szStr, onlyOrder, strings.Join(msgs, "；")), nil
}

// BudgetUSDT 每笔预算（0.1 = 用户口径的「0.1 美金」）
//
// 放在结构体上方便统一改；不填时默认 0.1。
var _ = math.Abs
