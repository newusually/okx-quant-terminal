package service

// engine.go —— 策略入口（handler/cron.go 里的 cron 直接调这些函数，签名一个没改）
//
// 「8 因子共振 + AI 解读」策略在 internal/service 下的实现分布：
//
//	market.go         OKX REST 客户端（公共 + 签名，限速 + 重试）
//	indicator.go      8 因子指标与共振位掩码
//	scanner.go        全合约三阶段扫描
//	trader.go         闸门 / 下单 / 出场 / 风控
//	ai.go             AI 解读（OpenAI 兼容）
//	account.go        账户操作（原 py 的 getcashbal / sellall / orderbuy …）
//	legacy.go         老策略的行情取数 + 买点判断（原 go 整体）
//	feed.go           行情/合约列表适配（原 web/okxdata.go）
//	backfill.go       历史 K 线回补（原 web/backfill.go）
//
// 配置在 internal/conf，日志在 internal/logx，落库在 internal/repo。

import (
	"fmt"
	"time"

	"finally-main/internal/logx"
)

// ---- 周期入口（与 handler/cron.go 的 cron 一一对应） ----

func Run1()   { Run("1m") }
func Run3()   { Run("3m") }
func Run5()   { Run("5m") }
func Run15()  { Run("15m") }
func Run1H()  { Run("1H") }
func Run2H()  { Run("2H") }
func Run4H()  { Run("4H") }
func Run6H()  { Run("6H") }
func Run12H() { Run("12H") }
func Run1D()  { Run("1D") }

// Savecsvfinal 把 15m 的买点日志导出成 CSV（原来是一个不存在的 savecsv.py）
func Savecsvfinal() {
	Savecsv("15m")
}

// Run 跑一轮策略。异常一律吞掉，绝不能让 cron 协程挂掉。
func Run(minute string) {
	start := time.Now()
	defer func() {
		if r := recover(); r != nil {
			logx.Logf("ERROR", "Run(%s) panic: %v\n%s", minute, r, logx.DebugStack())
		}
	}()
	if err := EngineRun(minute); err != nil {
		logx.Logf("ERROR", "Run(%s) 失败：%v", minute, err)
		return
	}
	if d := time.Since(start); d > 90*time.Second {
		fmt.Printf("  （本轮耗时 %s，偏长，检查网络或把 workers 调小）\n", d.Round(time.Second))
	}
}
