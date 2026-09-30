package service

// 本文件由原 mvc.go（1023 行大杂烩）按「分类」拆出，见 docs/架构说明.md

import (
	"fmt"
)

func Buy(symbol string, minute string) {
	fmt.Println("-------------------------------买入--------------------------------->>>")
	ops, err := DefaultOps()
	if err != nil {
		fmt.Println("[买入] 初始化账户失败：", err)
		return
	}
	msg, err := ops.OrderBuy(symbol, normalizeMinute(minute), false)
	if err != nil {
		fmt.Printf("[买入] %s %s 失败：%v\n", symbol, minute, err)
		return
	}
	fmt.Println(msg)
}

// Getcashbal 对应 cash.py
func Getcashbal() {
	ops, err := DefaultOps()
	if err != nil {
		fmt.Println("[资金] 初始化账户失败：", err)
		return
	}
	msg, err := ops.GetCashBal()
	if err != nil {
		fmt.Println("[资金] 查询失败：", err)
		return
	}
	fmt.Println(msg)
}

// Getcashhistory 对应 cashhistory.py
func Getcashhistory() {
	ops, err := DefaultOps()
	if err != nil {
		fmt.Println("[资金历史] 初始化账户失败：", err)
		return
	}
	msg, err := ops.GetCashHistory()
	if err != nil {
		fmt.Println("[资金历史] 记录失败：", err)
		return
	}
	fmt.Println(msg)
}

// SellAll 对应 sells.py
func SellAll() {
	ops, err := DefaultOps()
	if err != nil {
		fmt.Println("[全平] 初始化账户失败：", err)
		return
	}
	msgs, err := ops.SellAll(false)
	if err != nil {
		fmt.Println("[全平] 执行失败：", err)
		return
	}
	if len(msgs) == 0 {
		fmt.Println("[全平] 没有触发条件的持仓")
		return
	}
	for _, m := range msgs {
		fmt.Println("[全平]", m)
	}
}

// GetuplRatio 对应 getuplRatio.py
func GetuplRatio() {
	ops, err := DefaultOps()
	if err != nil {
		fmt.Println("[浮盈巡检] 初始化账户失败：", err)
		return
	}
	lines, report, err := ops.GetUplRatio(false)
	if err != nil {
		fmt.Println("[浮盈巡检] 执行失败：", err)
		return
	}
	fmt.Println(report)
	for _, l := range lines {
		fmt.Printf("[浮盈巡检] %s 收益率 %.2f%% 现价 %.5f 均价 %.5f 保证金 %.4f\n",
			l.InstID, l.UplRatio*100, l.Last, l.AvgPx, l.Imr)
	}
}

// Savecsv 把 datas/log/buylog_<minute>.txt 里的两行式记录汇总成一份 CSV。
// （原 Python 调的 savecsv.py 在仓库里根本不存在，这里按真实日志格式补上。）
func Savecsv(minute string) {
	out, err := ExportBuyLogCSV(minute)
	if err != nil {
		fmt.Printf("[CSV] %s 导出失败：%v\n", minute, err)
		return
	}
	fmt.Printf("[CSV] 已导出 %s\n", out)
}
