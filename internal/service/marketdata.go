package service

// 本文件由原 mvc.go（1023 行大杂烩）按「分类」拆出，见 docs/架构说明.md

import (
	"crypto/tls"
	"fmt"
	"github.com/tidwall/gjson"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"os"
	"regexp"
	"runtime/debug"
	"strconv"
	"time"
)

func Getsymbollist() []string {

	defer func() {
		if err := recover(); err != nil {
			log.Printf("[ERROR]程序异常: %v\n堆栈:%s", err, debug.Stack())
			os.Exit(1) // 非0退出码
		}
	}()
	time.Sleep(time.Millisecond)

	// 获取当前时间 或者使用 time.Date(year, month, ...)
	t := time.Now()
	timeStamp := t.Unix()
	client := &http.Client{

		Transport: &http.Transport{

			TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
		},
	}
	req, err := http.NewRequest("GET", "https://www.okx.com/api/v5/public/instruments?instType=SWAP", nil)
	if err != nil {
		panic(err)
	}
	req.Header.Set("authority", "www.okx.com")
	req.Header.Set("timeout", "10000")
	req.Header.Set("x-cdn", "https://static.okx.com")
	req.Header.Set("devid", "8ccf140e-e4ab-4a46-8582-738445cad57c")
	req.Header.Set("accept-language", "zh-CN")
	req.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; WOW64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/80.0.3987.87 Safari/537.36 SE 2.X MetaSr 1.0")
	req.Header.Set("accept", "application/json")
	req.Header.Set("x-utc", "8")
	req.Header.Set("sec-fetch-dest", "empty")
	req.Header.Set("app-type", "web")
	req.Header.Set("sec-fetch-site", "same-origin")
	req.Header.Set("sec-fetch-mode", "cors")
	req.Header.Set("referer", "https://www.okx.com/trade-swap/btc-usdt-swap")
	req.Header.Set("cookie", "locale=zh_CN; defaultLocale=zh_CN; _gcl_au=1.1.2108489782."+strconv.Itoa(int(timeStamp))+"; _ga=GA1.2.1119126875."+strconv.Itoa(int(timeStamp))+"; _gid=GA1.2.1241734301."+strconv.Itoa(int(timeStamp))+"; amp_56bf9d=4KsK9IqNpGzx7Sx3l_9DPR...1g2ehgo0j.1g2ej7b4i.4.0.4")
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

	data := gjson.Get(src, "data").Array()
	regex := regexp.MustCompile(`^.*-USDT-SWAP$`)
	var validInstIDs []string

	for _, item := range data {
		// 解析基础参数
		instID := item.Get("instId").String()
		if !regex.MatchString(instID) {
			continue
		}

		// 转换为数值类型
		ctMult, _ := strconv.ParseFloat(item.Get("ctMult").String(), 64)
		ctVal, _ := strconv.ParseFloat(item.Get("ctVal").String(), 64)
		lever, _ := strconv.ParseFloat(item.Get("lever").String(), 64)

		// 杠杆上限处理
		lever = math.Min(lever, 50)

		// 获取标记价格
		x, c, _, _, _, _, _, _, _ := GetKline(instID, "1m")

		if x > 50 {
			price := c[x-1]
			// 计算初始保证金
			initialMargin := (ctVal * ctMult * price) / lever

			// 筛选条件
			if initialMargin < 1 {
				//fmt.Printf("符合条件: %-20s 保证金=%.4f USD\n", instID, initialMargin)
				validInstIDs = append(validInstIDs, instID)
			}
		}

	}

	return validInstIDs
}

func Getprice(symbol string) float64 {

	time.Sleep(time.Millisecond)

	// 获取当前时间 或者使用 time.Date(year, month, ...)
	t := time.Now()
	timeStamp := t.Unix()
	client := &http.Client{

		Transport: &http.Transport{

			TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
		},
	}
	req, err := http.NewRequest("GET", "https://www.okx.com/priapi/v5/market/mult-tickers?instIds="+symbol+"&t="+strconv.Itoa(int(timeStamp)), nil)
	if err != nil {
		panic(err)
	}
	req.Header.Set("authority", "www.okx.com")
	req.Header.Set("timeout", "10000")
	req.Header.Set("x-cdn", "https://static.okx.com")
	req.Header.Set("devid", "8ccf140e-e4ab-4a46-8582-738445cad57c")
	req.Header.Set("accept-language", "zh-CN")
	req.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; WOW64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/80.0.3987.87 Safari/537.36 SE 2.X MetaSr 1.0")
	req.Header.Set("accept", "application/json")
	req.Header.Set("x-utc", "8")
	req.Header.Set("sec-fetch-dest", "empty")
	req.Header.Set("app-type", "web")
	req.Header.Set("sec-fetch-site", "same-origin")
	req.Header.Set("sec-fetch-mode", "cors")
	req.Header.Set("referer", "https://www.okx.com/trade-swap/btc-usdt-swap")
	req.Header.Set("cookie", "locale=zh_CN; defaultLocale=zh_CN; _gcl_au=1.1.2108489782."+strconv.Itoa(int(timeStamp))+"; _ga=GA1.2.1119126875."+strconv.Itoa(int(timeStamp))+"; _gid=GA1.2.1241734301."+strconv.Itoa(int(timeStamp))+"; amp_56bf9d=4KsK9IqNpGzx7Sx3l_9DPR...1g2ehgo0j.1g2ej7b4i.4.0.4")
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
	//"instType":"SWAP","instId":"GODS-USDT-SWAP","last":"0.7141","lastSz":"13",
	lastprice := gjson.Get(src, "data.#.last").Array()
	//fmt.Println((lastprice[0]).Float())
	fmt.Println(symbol, lastprice)

	return lastprice[0].Float()

}

func Getsymbols() []gjson.Result {
	time.Sleep(time.Millisecond)

	// 获取当前时间 或者使用 time.Date(year, month, ...)
	t := time.Now()
	timeStamp := t.Unix()
	client := &http.Client{

		Transport: &http.Transport{

			TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
		},
	}
	req, err := http.NewRequest("GET", "https://www.okx.com/priapi/v5/market/tickers?instType=SWAP&t="+strconv.Itoa(int(timeStamp))+"&instType=SWAP", nil)
	if err != nil {
		panic(err)
	}
	req.Header.Set("authority", "www.okx.com")
	req.Header.Set("timeout", "10000")
	req.Header.Set("x-cdn", "https://static.okx.com")
	req.Header.Set("devid", "8ccf140e-e4ab-4a46-8582-738445cad57c")
	req.Header.Set("accept-language", "zh-CN")
	req.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; WOW64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/80.0.3987.87 Safari/537.36 SE 2.X MetaSr 1.0")
	req.Header.Set("accept", "application/json")
	req.Header.Set("x-utc", "8")
	req.Header.Set("sec-fetch-dest", "empty")
	req.Header.Set("app-type", "web")
	req.Header.Set("sec-fetch-site", "same-origin")
	req.Header.Set("sec-fetch-mode", "cors")
	req.Header.Set("referer", "https://www.okx.com/trade-swap/btc-usdt-swap")
	req.Header.Set("cookie", "locale=zh_CN; defaultLocale=zh_CN; _gcl_au=1.1.2108489782."+strconv.Itoa(int(timeStamp))+"; _ga=GA1.2.1119126875."+strconv.Itoa(int(timeStamp))+"; _gid=GA1.2.1241734301."+strconv.Itoa(int(timeStamp))+"; amp_56bf9d=4KsK9IqNpGzx7Sx3l_9DPR...1g2ehgo0j.1g2ej7b4i.4.0.4")
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
	//"instType":"SWAP","instId":"GODS-USDT-SWAP","last":"0.7141","lastSz":"13",
	instId := gjson.Get(src, "data.#.instId").Array()
	//fmt.Println(instId)
	return instId
}
