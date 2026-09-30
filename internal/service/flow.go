package service

// 本文件由原 mvc.go（1023 行大杂烩）按「分类」拆出，见 docs/架构说明.md

import (
	"crypto/tls"
	"fmt"
	"github.com/tidwall/gjson"
	"io/ioutil"
	"net/http"
	"strconv"
	"time"
)

func GetBuySellVol(symbol string, minute string) ([]string, []float64, []float64) {
	time.Sleep(time.Millisecond * 10)

	// 获取当前时间 或者使用 time.Date(year, month, ...)
	t := time.Now()
	timeStamp := t.Unix()
	client := &http.Client{

		Transport: &http.Transport{

			TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
		},
	}

	req, err := http.NewRequest("GET", "https://www.okx.com/priapi/v5/rubik/public/stat/indicators?bar="+minute+"&instId="+symbol+"&unit=2&limit=1030&before=1725000000000&indicators=takerBuySellVol&t="+strconv.Itoa(int(timeStamp)), nil)
	if err != nil {
		panic(err)

	}
	req.Header.Set("authority", "www.okx.com")
	req.Header.Set("timeout", "10000")
	req.Header.Set("x-cdn", "https://static.okx.com")
	req.Header.Set("devid", "a5ceb850-4efb-4a3f-baff-21da4fce8858")
	req.Header.Set("accept-language", "zh-CN")
	req.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; WOW64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/80.0.3987.87 Safari/537.36 SE 2.X MetaSr 1.0")
	req.Header.Set("accept", "application/json")
	req.Header.Set("x-utc", "8")
	req.Header.Set("sec-fetch-dest", "empty")
	req.Header.Set("app-type", "web")
	req.Header.Set("sec-fetch-site", "same-origin")
	req.Header.Set("sec-fetch-mode", "cors")
	req.Header.Set("referer", "https://www.okx.com/trade-swap/btc-usdt-swap")
	req.Header.Set("cookie", "locale=zh_CN; defaultLocale=zh_CN; _gcl_au=1.1.1520807996."+strconv.Itoa(int(timeStamp))+"; _ga=GA1.2.1752370991."+strconv.Itoa(int(timeStamp))+"; _gid=GA1.2.650161560."+strconv.Itoa(int(timeStamp))+"; amp_56bf9d=y9J2I5hN4sKjIiyZROsSAs...1g1isehgq.1g1isehgs.2.0.2; _gat_UA-35324627-3=1")
	resp, err := client.Do(req)
	if err != nil {
		panic(err)

	}
	defer resp.Body.Close()
	bodyText, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		panic(err)
	}

	src := string(bodyText)
	//fmt.Println(src)
	//"date", "open", "high", "low", "close", "p", "vol"
	dates := gjson.Get(src, "data.takerBuySellVol.#.0").Array()
	buys := gjson.Get(src, "data.takerBuySellVol.#.1").Array()
	sells := gjson.Get(src, "data.takerBuySellVol.#.2").Array()
	day := make([]string, len(dates))
	buy := make([]float64, len(buys))
	sell := make([]float64, len(sells))
	for i := 0; i < len(dates); i++ {
		a := dates[len(dates)-i-1].Str
		e, _ := strconv.ParseInt(a, 10, 64)
		day[i] = time.Unix(0, e*int64(time.Millisecond)).Format("2006-01-02 15:04:05")

		b := buys[len(buys)-i-1].Str
		f, _ := strconv.ParseFloat(b, 64)
		buy[i] = f

		g := sells[len(sells)-i-1].Str
		j, _ := strconv.ParseFloat(g, 64)
		sell[i] = j

	}

	return day, buy, sell
}

func GetRatio(symbol string, minute string) ([]string, []float64) {
	time.Sleep(time.Millisecond * 10)

	// 获取当前时间 或者使用 time.Date(year, month, ...)
	t := time.Now()
	timeStamp := t.Unix()
	client := &http.Client{

		Transport: &http.Transport{

			TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
		},
	}
	//https://www.okx.com/priapi/v5/rubik/public/stat/indicators?bar=4H&instId=XRP-USDT-SWAP&unit=2&limit=103&before=1725000000000&indicators=eliteLSAccountRatio&t=1725940771406
	req, err := http.NewRequest("GET", "https://www.okx.com/priapi/v5/rubik/public/stat/indicators?bar="+minute+"&instId="+symbol+"&unit=2&limit=1030&before=1725000000000&indicators=eliteLSAccountRatio&t="+strconv.Itoa(int(timeStamp)), nil)
	if err != nil {
		panic(err)

	}
	req.Header.Set("authority", "www.okx.com")
	req.Header.Set("timeout", "10000")
	req.Header.Set("x-cdn", "https://static.okx.com")
	req.Header.Set("devid", "a5ceb850-4efb-4a3f-baff-21da4fce8858")
	req.Header.Set("accept-language", "zh-CN")
	req.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; WOW64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/80.0.3987.87 Safari/537.36 SE 2.X MetaSr 1.0")
	req.Header.Set("accept", "application/json")
	req.Header.Set("x-utc", "8")
	req.Header.Set("sec-fetch-dest", "empty")
	req.Header.Set("app-type", "web")
	req.Header.Set("sec-fetch-site", "same-origin")
	req.Header.Set("sec-fetch-mode", "cors")
	req.Header.Set("referer", "https://www.okx.com/trade-swap/btc-usdt-swap")
	req.Header.Set("cookie", "locale=zh_CN; defaultLocale=zh_CN; _gcl_au=1.1.1520807996."+strconv.Itoa(int(timeStamp))+"; _ga=GA1.2.1752370991."+strconv.Itoa(int(timeStamp))+"; _gid=GA1.2.650161560."+strconv.Itoa(int(timeStamp))+"; amp_56bf9d=y9J2I5hN4sKjIiyZROsSAs...1g1isehgq.1g1isehgs.2.0.2; _gat_UA-35324627-3=1")
	resp, err := client.Do(req)
	if err != nil {
		panic(err)

	}
	defer resp.Body.Close()
	bodyText, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		panic(err)
	}

	src := string(bodyText)
	//fmt.Println(src)
	//"date", "open", "high", "low", "close", "p", "vol"
	dates := gjson.Get(src, "data.eliteLSAccountRatio.#.0").Array()
	ratios := gjson.Get(src, "data.eliteLSAccountRatio.#.3").Array()
	day := make([]string, len(dates))
	ratio := make([]float64, len(ratios))
	for i := 0; i < len(dates); i++ {
		a := dates[len(dates)-i-1].Str
		e, _ := strconv.ParseInt(a, 10, 64)
		day[i] = time.Unix(0, e*int64(time.Millisecond)).Format("2006-01-02 15:04:05")

		b := ratios[len(ratios)-i-1].Str
		f, _ := strconv.ParseFloat(b, 64)
		ratio[i] = f

	}
	return day, ratio
}

func GetKline(symbol string, minute string) (int, []float64, float64, float64, []float64, []float64, []float64, []float64, []string) {

	//fmt.Println(symboldemo, symbol)
	time.Sleep(time.Millisecond * 10)

	// 获取当前时间 或者使用 time.Date(year, month, ...)
	t := time.Now()
	timeStamp := t.Unix()
	client := &http.Client{

		Transport: &http.Transport{

			TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
		},
	}
	req, err := http.NewRequest("GET", "https://www.okx.com/priapi/v5/market/candles?instId="+symbol+"&bar="+minute+"&after=&limit=1400&t="+strconv.Itoa(int(timeStamp)), nil)
	if err != nil {
		panic(err)

	}
	req.Header.Set("authority", "www.okx.com")
	req.Header.Set("timeout", "10000")
	req.Header.Set("x-cdn", "https://static.okx.com")
	req.Header.Set("devid", "a5ceb850-4efb-4a3f-baff-21da4fce8858")
	req.Header.Set("accept-language", "zh-CN")
	req.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; WOW64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/80.0.3987.87 Safari/537.36 SE 2.X MetaSr 1.0")
	req.Header.Set("accept", "application/json")
	req.Header.Set("x-utc", "8")
	req.Header.Set("sec-fetch-dest", "empty")
	req.Header.Set("app-type", "web")
	req.Header.Set("sec-fetch-site", "same-origin")
	req.Header.Set("sec-fetch-mode", "cors")
	req.Header.Set("referer", "https://www.okx.com/trade-swap/btc-usdt-swap")
	req.Header.Set("cookie", "locale=zh_CN; defaultLocale=zh_CN; _gcl_au=1.1.1520807996."+strconv.Itoa(int(timeStamp))+"; _ga=GA1.2.1752370991."+strconv.Itoa(int(timeStamp))+"; _gid=GA1.2.650161560."+strconv.Itoa(int(timeStamp))+"; amp_56bf9d=y9J2I5hN4sKjIiyZROsSAs...1g1isehgq.1g1isehgs.2.0.2; _gat_UA-35324627-3=1")
	resp, err := client.Do(req)
	if err != nil {
		panic(err)

	}
	defer resp.Body.Close()
	bodyText, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		panic(err)
	}

	src := string(bodyText)
	//"date", "open", "high", "low", "close", "p", "vol"
	closes := gjson.Get(src, "data.#.4").Array()
	dates := gjson.Get(src, "data.#.0").Array()
	opens := gjson.Get(src, "data.#.1").Array()
	highs := gjson.Get(src, "data.#.2").Array()
	lows := gjson.Get(src, "data.#.3").Array()
	vols := gjson.Get(src, "data.#.6").Array()

	if len(closes) < 500 || closes[10].Float() < 0.0001 {
		return 0, []float64{}, 0, 0, []float64{}, []float64{}, []float64{}, []float64{}, []string{}
	}

	if len(closes) > 500 && closes[10].Float() > 0.0001 {

		day := make([]string, len(dates))
		c := make([]float64, len(closes))
		o := make([]float64, len(opens))
		h := make([]float64, len(highs))
		l := make([]float64, len(lows))
		v := make([]float64, len(vols))

		for i := 0; i < len(closes); i++ {

			a := dates[len(dates)-i-1].Str
			e, _ := strconv.ParseInt(a, 10, 64)
			day[i] = time.Unix(0, e*int64(time.Millisecond)).Format("2006-01-02 15:04:05")

			b := closes[len(closes)-i-1].Str
			f, _ := strconv.ParseFloat(b, 64)
			c[i] = f

			d := opens[len(opens)-i-1].Str
			g, _ := strconv.ParseFloat(d, 64)
			o[i] = g

			p := highs[len(highs)-i-1].Str
			rs, _ := strconv.ParseFloat(p, 64)
			h[i] = rs

			ll := lows[len(lows)-i-1].Str
			ll1, _ := strconv.ParseFloat(ll, 64)
			l[i] = ll1

			vv := vols[len(vols)-i-1].Str
			vol1, _ := strconv.ParseFloat(vv, 64)
			v[i] = vol1

		}

		x := len(c)

		return x, c, c[x-2] / o[x-2], c[x-1] / o[x-1], o, l, v, h, day
	}
	return 0, []float64{}, 0, 0, []float64{}, []float64{}, []float64{}, []float64{}, []string{}
}

//https://www.okx.com/priapi/v5/rubik/stat/taker-volume?instType=SPOT&period=1H&ccy=ETH&t=1742707202163

func GetTakerVolume(symbol string, minute string) bool {
	defer func() {
		if r := recover(); r != nil {
			// 处理异常
			fmt.Println("Exception caught:", r)
		}
	}()

	//fmt.Println(symboldemo, symbol)
	time.Sleep(time.Millisecond * 10)

	// 获取当前时间 或者使用 time.Date(year, month, ...)
	t := time.Now()
	timeStamp := t.Unix()
	client := &http.Client{

		Transport: &http.Transport{

			TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
		},
	}
	//https://www.okx.com/priapi/v5/public/liquidation-orders?instType=SWAP&instFamily=ETH-USDT&state=filled&limit=100&t=1739051712008
	req, err := http.NewRequest("GET", "https://www.okx.com/priapi/v5/rubik/stat/taker-volume?instType=SPOT&period="+minute+"&ccy="+symbol+"&t="+strconv.Itoa(int(timeStamp)), nil)
	if err != nil {
		panic(err)

	}
	req.Header.Set("authority", "www.okx.com")
	req.Header.Set("timeout", "10000")
	req.Header.Set("x-cdn", "https://static.okx.com")
	req.Header.Set("devid", "a5ceb850-4efb-4a3f-baff-21da4fce8858")
	req.Header.Set("accept-language", "zh-CN")
	req.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; WOW64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/80.0.3987.87 Safari/537.36 SE 2.X MetaSr 1.0")
	req.Header.Set("accept", "application/json")
	req.Header.Set("x-utc", "8")
	req.Header.Set("sec-fetch-dest", "empty")
	req.Header.Set("app-type", "web")
	req.Header.Set("sec-fetch-site", "same-origin")
	req.Header.Set("sec-fetch-mode", "cors")
	req.Header.Set("referer", "https://www.okx.com/trade-swap/btc-usdt-swap")
	req.Header.Set("cookie", "locale=zh_CN; defaultLocale=zh_CN; _gcl_au=1.1.1520807996."+strconv.Itoa(int(timeStamp))+"; _ga=GA1.2.1752370991."+strconv.Itoa(int(timeStamp))+"; _gid=GA1.2.650161560."+strconv.Itoa(int(timeStamp))+"; amp_56bf9d=y9J2I5hN4sKjIiyZROsSAs...1g1isehgq.1g1isehgs.2.0.2; _gat_UA-35324627-3=1")
	resp, err := client.Do(req)
	if err != nil {
		panic(err)

	}
	defer resp.Body.Close()
	bodyText, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		panic(err)
	}

	src := string(bodyText)

	dates := gjson.Get(src, "data.#.0").Array()
	buys := gjson.Get(src, "data.#.2").Array()
	sells := gjson.Get(src, "data.#.1").Array()
	day := make([]string, len(dates))
	buy := make([]float64, len(buys))
	sell := make([]float64, len(sells))
	for i := 0; i < len(dates); i++ {
		a := dates[len(dates)-i-1].Str
		e, _ := strconv.ParseInt(a, 10, 64)
		day[i] = time.Unix(0, e*int64(time.Millisecond)).Format("2006-01-02 15:04:05")

		b := buys[len(buys)-i-1].Str
		f, _ := strconv.ParseFloat(b, 64)
		buy[i] = f

		g := sells[len(sells)-i-1].Str
		j, _ := strconv.ParseFloat(g, 64)
		sell[i] = j

	}

	x := len(day)

	if x < 10 {
		return false
	} else {
		buys1 := (buy[x-1] + buy[x-2] + buy[x-3] + buy[x-4]) - (sell[x-1] + sell[x-2] + sell[x-3] + sell[x-4])
		buys2 := (buy[x-5] + buy[x-6] + buy[x-7] + buy[x-8]) - (sell[x-5] + sell[x-6] + sell[x-7] + sell[x-8])

		if buys1 > 0 && buys2 < 0 {
			return true
		} else {
			return false
		}
	}

}

func GetLiquidation() string {
	defer func() {
		if r := recover(); r != nil {
			// 处理异常
			fmt.Println("Exception caught:", r)
		}
	}()

	//fmt.Println(symboldemo, symbol)
	time.Sleep(time.Millisecond * 10)

	// 获取当前时间 或者使用 time.Date(year, month, ...)
	t := time.Now()
	timeStamp := t.Unix()
	client := &http.Client{

		Transport: &http.Transport{

			TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
		},
	}
	//https://www.okx.com/priapi/v5/public/liquidation-orders?instType=SWAP&instFamily=ETH-USDT&state=filled&limit=100&t=1739051712008
	req, err := http.NewRequest("GET", "https://www.okx.com/priapi/v5/public/liquidation-orders?instType=SWAP&instFamily=ETH-USDT&state=filled&limit=100&t="+strconv.Itoa(int(timeStamp)), nil)
	if err != nil {
		panic(err)

	}
	req.Header.Set("authority", "www.okx.com")
	req.Header.Set("timeout", "10000")
	req.Header.Set("x-cdn", "https://static.okx.com")
	req.Header.Set("devid", "a5ceb850-4efb-4a3f-baff-21da4fce8858")
	req.Header.Set("accept-language", "zh-CN")
	req.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; WOW64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/80.0.3987.87 Safari/537.36 SE 2.X MetaSr 1.0")
	req.Header.Set("accept", "application/json")
	req.Header.Set("x-utc", "8")
	req.Header.Set("sec-fetch-dest", "empty")
	req.Header.Set("app-type", "web")
	req.Header.Set("sec-fetch-site", "same-origin")
	req.Header.Set("sec-fetch-mode", "cors")
	req.Header.Set("referer", "https://www.okx.com/trade-swap/btc-usdt-swap")
	req.Header.Set("cookie", "locale=zh_CN; defaultLocale=zh_CN; _gcl_au=1.1.1520807996."+strconv.Itoa(int(timeStamp))+"; _ga=GA1.2.1752370991."+strconv.Itoa(int(timeStamp))+"; _gid=GA1.2.650161560."+strconv.Itoa(int(timeStamp))+"; amp_56bf9d=y9J2I5hN4sKjIiyZROsSAs...1g1isehgq.1g1isehgs.2.0.2; _gat_UA-35324627-3=1")
	resp, err := client.Do(req)
	if err != nil {
		panic(err)

	}
	defer resp.Body.Close()
	bodyText, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		panic(err)
	}

	src := string(bodyText)
	//fmt.Println(src)

	prices := gjson.Get(src, "data.#.details.#.price").Array()
	times := gjson.Get(src, "data.#.details.#.time").Array()
	szs := gjson.Get(src, "data.#.details.#.sz").Array()
	sides := gjson.Get(src, "data.#.details.#.side").Array()

	// 获取当前时间
	now := time.Now()

	// 将二维数组展平成一维数组
	var flatSzs []float64
	var flatTimes []string
	var flatSides []string
	var flatPrices []float64
	var isbuy string

	// 合并循环，同时遍历szs和times
	for i := 0; i < len(szs); i++ {
		innerSzs := szs[i].Array()
		innerTimes := times[i].Array()
		innerSides := sides[i].Array()
		innerPrices := prices[i].Array()

		// 遍历内部数组，并同时处理szs和times的元素
		for j := 0; j < len(innerSzs); j++ {
			szNum, _ := strconv.ParseFloat(innerSzs[j].String(), 64)

			flatSzs = append(flatSzs, szNum)

			side := innerSides[j].String()
			flatSides = append(flatSides, side)

			priceNum, _ := strconv.ParseFloat(innerPrices[j].String(), 64)

			flatPrices = append(flatPrices, priceNum)

			timeNum, _ := strconv.ParseFloat(innerTimes[j].String(), 64)

			// 假设这是你从JSON中获取的时间戳（毫秒级）
			timestampInMilliseconds := int64(timeNum)

			// 将毫秒级时间戳转换为秒级时间戳
			timestampInSeconds := timestampInMilliseconds / 1000

			// 将秒级时间戳转换为time.Time类型
			utcTime := time.Unix(timestampInSeconds, 0)

			// 将毫秒级时间戳转换为time.Time类型
			parsedTime := time.Unix(0, timestampInMilliseconds*int64(time.Millisecond))

			// 计算与当前时间的差距
			duration := now.Sub(parsedTime)

			// 格式化并打印时间
			formattedTime := utcTime.Format("2006-01-02 15:04:05")

			flatTimes = append(flatTimes, formattedTime)

			// 检查时间差距是否不超过5分钟，并且sz值是否大于100
			if duration.Minutes() <= 5 && szNum > 3000 && side == "sell" {

				log := "买入ETH--->>> ,time--->>>" + formattedTime +
					",sz--->>>" + fmt.Sprintf("%.5f", szNum) + ",price--->>>" + fmt.Sprintf("%.5f", priceNum)

				fmt.Println(log)
				GetWriter(log, "3m")

				isbuy = "buy"
			} else if duration.Minutes() <= 5 && szNum > 3000 && side == "buy" {

				log := "卖出ETH--->>> ,time--->>>" + formattedTime +
					",sz--->>>" + fmt.Sprintf("%.5f", szNum) + ",price--->>>" + fmt.Sprintf("%.5f", priceNum)

				fmt.Println(log)
				GetWriter(log, "3m")

				isbuy = "sell"
			}

		}
	}

	/**

	for i := 0; i < len(flatSzs); i++ {

		if flatSides[i]=="sell" && flatSzs[i]>100{

			fmt.Println("Flat Times:", flatTimes[i],"Flat Sides:", flatSides[i],"Flat Szs:", flatSzs[i],"Flat Prices:", flatPrices[i])
		}

	}
	*/

	// 打印结果
	//fmt.Println("Flat Szs:", flatSzs)
	//fmt.Println("Flat Times:", flatTimes)
	//fmt.Println("isbuy:", isbuy)
	return isbuy

}
