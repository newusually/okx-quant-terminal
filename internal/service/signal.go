package service

// 本文件由原 mvc.go（1023 行大杂烩）按「分类」拆出，见 docs/架构说明.md

import (
	"fmt"

	"github.com/markcheno/go-talib"
	"time"
)

func GetisBtcMacd(symbol string, minute string) bool {
	x, c, _, _, _, _, _, _, _ := GetKline(symbol, minute)
	//x, c, c[x-2] / o[x-2], c[x-1] / o[x-1], o, l, v,h
	if x < 500 {
		return false

	} else if symbol == "USDC-USDT-SWAP" || symbol == "USTC-USDT-SWAP" {
		return false

	} else {
		diff, dea, _ := talib.Macd(c, 12, 26, 60)
		macd1 := 2 * (diff[x-1] - dea[x-1])
		macd2 := 2 * (diff[x-2] - dea[x-2])
		macd3 := 2 * (diff[x-3] - dea[x-3])
		macd4 := 2 * (diff[x-4] - dea[x-4])

		if c[x-3] > 0.001 && macd1 > 0 && macd2 < 0 && macd3 < 0 && macd4 < 0 {

			return true
		}
		return false
	}
}

func GetisEthisBuy(minute string) bool {
	//_, c1, _, co1_btc, _, _, _ := GetKline("BTC-USDT-SWAP", minute)
	_, _, co2, co1, _, _, _, _, _ := GetKline("ETH-USDT-SWAP", minute)
	//diff1, dea1, _ := talib.Macd(c1, 12, 26, 60)

	if co1 < 0.9 || co2 < 0.9 {

		return true
	} else {
		return false
	}
}

func GetIsBuy(symbol string, minute string) bool {

	//x, c, c[x-2] / o[x-2], c[x-1] / o[x-1], o, l, v,h
	isbuy := GetisBtcMacd(symbol, minute)
	if isbuy {

		_, buy, sell := GetBuySellVol(symbol, minute)

		buy1 := buy[len(buy)-1]
		sell1 := sell[len(sell)-1]
		buy2 := buy[len(buy)-2]
		sell2 := sell[len(sell)-2]
		buy3 := buy[len(buy)-3]
		sell3 := sell[len(sell)-3]
		buy4 := buy[len(buy)-4]
		sell4 := sell[len(sell)-4]

		buys1 := (buy1 + buy2 + buy3) / (sell1 + sell2 + sell3)
		buys2 := (buy2 + buy3 + buy4) / (sell2 + sell3 + sell4)

		if buys1/buys2 > 1 && buys1/buys2 < 2 && buy1/sell1 > 1 && buy1/sell1 < 2 {

			log := "\n" + time.Now().Format("2006-1-2 15:04:02") +
				",symbol--->>" + symbol +
				"\n,buys1/buys2--->>" + fmt.Sprintf("%.5f", buys1/buys2) +
				",buys1-->>" + fmt.Sprintf("%.5f", buys1) +
				",buys2-->>" + fmt.Sprintf("%.5f", buys2) +
				"\n,buy1/sell1--->>" + fmt.Sprintf("%.5f", buy1/sell1) +
				",buy2/sell2--->>" + fmt.Sprintf("%.5f", buy2/sell2) +
				",buy3/sell3-->>" + fmt.Sprintf("%.5f", buy3/sell3) +
				",minute--->>" + minute
			fmt.Println(log)
			GetWriter(log, minute)
			return true
		}

		return false
	}
	return false
}
