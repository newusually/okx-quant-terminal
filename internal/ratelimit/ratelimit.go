package ratelimit

// ratelimit.go —— 进程内共享的 OKX 限速闸门
//
// 为什么必须「进程内共享」而不是每个客户端各一把：
//
//	okxweb 里同时有两拨人在打 OKX：
//	  ① 数据服务（K 线回补，10 个 worker 并发）
//	  ② 策略引擎（每 60 秒扫一遍全市场，拉几十个合约的 K 线）
//	OKX 对 /market/history-candles 的限制是 20 次 / 2 秒。
//	两边各持一把自己的限速器，速率就会叠加成 2 倍 → 一片 429，
//	回补中断、信号漏掉、买卖点全部错位。
//
// 更麻烦的是：这两拨人用的是**两套不同的 HTTP 客户端实现**
// （internal/repo/okx 一套，internal/service/market.go 另一套），
// 没法靠共用一个 struct 字段解决。所以把闸门提到独立包里，
// 两边都从这里取，才算真正共享。

import (
	"sync"
	"time"
)

// Limiter 滑动窗口限速器：任意 per 时间窗内最多放行 max 次。
type Limiter struct {
	mu    sync.Mutex
	times []time.Time
	max   int
	per   time.Duration
}

// New 建一个限速器
func New(max int, per time.Duration) *Limiter {
	if max <= 0 {
		max = 1
	}
	if per <= 0 {
		per = time.Second
	}
	return &Limiter{max: max, per: per}
}

// Wait 阻塞到「当前窗口还有配额」为止
func (l *Limiter) Wait() {
	for {
		l.mu.Lock()
		now := time.Now()
		cut := 0
		for cut < len(l.times) && now.Sub(l.times[cut]) >= l.per {
			cut++
		}
		if cut > 0 {
			l.times = append([]time.Time(nil), l.times[cut:]...)
		}
		if len(l.times) < l.max {
			l.times = append(l.times, now)
			l.mu.Unlock()
			return
		}
		sleep := l.per - now.Sub(l.times[0])
		l.mu.Unlock()
		if sleep < 5*time.Millisecond {
			sleep = 5 * time.Millisecond
		}
		time.Sleep(sleep)
	}
}

// ---------------------------------------------------------------------------
// 全进程共享的两把闸门
// ---------------------------------------------------------------------------

var (
	// candle 行情类：OKX /market/candles 与 /market/history-candles 限 20 次 / 2 秒。
	// 取满额 20/2s —— 因为回补和策略扫描现在是同一个池子，不会互相叠加了。
	candle = New(20, 2*time.Second)

	// trade 交易类：下单/撤单/查持仓。20 次 / 2 秒，比行情保守得多，
	// 下单宁可慢一点也不能丢。
	trade = New(8, 2*time.Second)

	// general 其余公共接口（合约列表、行情快照、公告）
	general = New(15, 2*time.Second)
)

// Candle 取行情类闸门
func Candle() *Limiter { return candle }

// Trade 取交易类闸门
func Trade() *Limiter { return trade }

// General 取公共接口闸门
func General() *Limiter { return general }

// WaitCandle 等一个行情配额
func WaitCandle() { candle.Wait() }
