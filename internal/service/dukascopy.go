package service

// dukascopy.go —— NQ（纳斯达克100）只读行情接入
//
// 【这个模块的定位】
//
//	给网页加一个「只能看、不能买」的 NQ 板块：拉外部数据源的历史 K 线，
//	算指标与买卖信号展示出来，但**永不进入下单链路**。
//
// 【数据源：Dukascopy .bi5】
//
//	URL: https://datafeed.dukascopy.com/datafeed/{SYMBOL}/{YYYY}/{MM}/{DD}/BID_candles_min_1.bi5
//	  · 一个文件 = **一整天**的 1 分钟 K 线（1440 条 × 24 字节 = 34560 字节解压后）
//	  · **MM 是 0-based**（10 月 = 09）—— 官方 wiki 明确说明，踩过一次
//	  · 压缩格式是 **LZMA raw**，但文件自带完整 13 字节 .lzma 头
//	    （5D | dictSize=4MB | uncompressedSize=34560），所以标准 lzma.Reader 直接能读；
//	  · 记录 24 字节、**大端**：uint32 off秒, uint32 O, uint32 C, uint32 L, uint32 H, float32 V
//	    ⚠ 字段顺序是 O/C/L/H 而不是 O/H/L/C —— 解出来必须验 OHLC 自洽（实测 1440/1440 通过）
//	  · 价格 = 整数值 / 1000（point value = 3）
//	  · 周末/节假日 → **文件不存在（404）**，这是「休市」不是错误
//
// 【两个必须处理的坑】
//
//	1) ★ 本机 DNS 把 datafeed.dukascopy.com 解析到 194.8.15.180（连接超时），
//	   而 DoH 查出来的正确地址是 AWS 上的 16.62.x.x。所以这里不走系统 DNS，
//	   自己用 DoH 解析 + 内置兜底 IP，再通过自定义 DialContext 定向连接
//	   （URL 仍是域名，TLS SNI 不变）。
//	2) ★ Dukascopy 从 2018 年起严格限流（429 / 503 / 直接挂住不响应），
//	   所以下载**必须串行 + 每次间隔**，失败指数退避。并发加速只会全盘失败。

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"finally-main/internal/conf"
	"finally-main/internal/model"
	"finally-main/internal/repo"

	"github.com/ulikunitz/xz/lzma"
)

// ---------------------------------------------------------------------------
// 常量：NQ 只读板块
// ---------------------------------------------------------------------------

const (
	// NQInstID 只读板块在库里的「合约」标识。
	// 刻意不用 -USDT-SWAP 后缀：它不是 OKX 合约，任何按后缀判断的代码都不该把它当交易标的。
	NQInstID = "NQ-INDEX"

	// nqDukaSymbol Dukascopy 侧的品种代码（US Tech 100 = 纳斯达克100）
	nqDukaSymbol = "USATECHIDXUSD"

	// nqPointDiv Dukascopy 整数价格 → 真实价格。指数类 point value = 3（÷1000）
	nqPointDiv = 1000.0

	// nqDays 回补天数（用户口径：最近 30 天）
	nqDays = 30

	// nqReqGap 两次下载之间的固定间隔。
	//
	// ★ 实测教训：Dukascopy 的限流比社区文档描述的更狠 ——
	//   连续几十次请求后会进入「惩罚冷却」，此后**任何**请求都不返回，
	//   直接挂住到客户端超时（HTTP code=000），Cool 期可达十几分钟。
	//   所以这里必须给足间隔；快 ≠ 好，被打进冷却反而要等更久。
	nqReqGap = 3 * time.Second

	// nqMaxPerRound 单轮最多下载几个新文件。
	// 一次拉满 30 天必被打进冷却，所以按「每次一小批、多轮补齐」来做 ——
	// 配合 nqMissingDays 的缺口检测，被打断也不会重复下载。
	nqMaxPerRound = 8

	// nqCooldownGap 连续失败后的长冷却（限流惩罚期远长于普通重试间隔）
	nqCooldownGap = 90 * time.Second

	// dukaHost 数据源域名
	dukaHost = "datafeed.dukascopy.com"

	// dukaDoH DoH 解析端点（系统 DNS 被污染，必须绕开）
	dukaDoH = "https://dns.google/resolve?name=" + dukaHost + "&type=A"
)

// dukaFallbackIPs 内置兜底 IP：DoH 不可用时直接用。
// 这两个地址是 dukaHost 的 CNAME 终结点（AWS），实测可连。
var dukaFallbackIPs = []string{"16.62.244.190", "16.62.187.25"}

// errNQLimited 数据源限流哨兵错误。
//
// 实测：Dukascopy 在连续请求后会进入惩罚冷却 —— 表现为 HTTP 503
// 或者干脆「挂住不响应」（客户端超时,curl code=000）。
// 这是 **IP 级、全局性的**：同一时刻换日期、换文件都拿不到，
// 重试只会让冷却期更久。所以遇到它就立刻停手，把等待交给定时器。
var errNQLimited = errors.New("数据源限流（冷却中）")

// NQBars NQ 板块展示的周期（用户口径：3 / 5 / 15 分钟）
var NQBars = []string{"3m", "5m", "15m"}

// ---------------------------------------------------------------------------
// 只读合约登记（全项目唯一判据入口）
// ---------------------------------------------------------------------------

// ReadonlyInstIDs 只读板块合约集合：有 K 线、有信号，但**永不交易**。
//
// 任何「能不能下单」的判断都要先问这里，不要在别处另写一份名单
// （否则又是「同一个量两条路算」的老坑）。
var ReadonlyInstIDs = map[string]bool{
	NQInstID: true,
}

// IsReadonlyInst 是否只读合约
func IsReadonlyInst(instID string) bool {
	return ReadonlyInstIDs[instID]
}

// ReadonlyInstName 只读合约的展示名（前端标题 / 列表用）
func ReadonlyInstName(instID string) string {
	if instID == NQInstID {
		return "NQ / 纳斯达克100"
	}
	return instID
}

// ---------------------------------------------------------------------------
// DNS 覆盖 + HTTP 客户端
// ---------------------------------------------------------------------------

var (
	dukaIPMu   sync.RWMutex
	dukaIPList []string // 当前可用 IP（首个优先）
)

// resolveDukaIPs 用 DoH 解析数据源域名的真实 IP，失败则退回内置列表。
//
// 为什么要这么麻烦：本机系统 DNS 把它解析到一个连不通的地址（实测 TLS 握手超时），
// 而 curl --resolve 到 DoH 查出的地址立刻 200。这类「DNS 污染」只能自己绕。
func resolveDukaIPs(ctx context.Context) []string {
	ips := fetchDoHIPs(ctx)
	if len(ips) == 0 {
		ips = dukaFallbackIPs
	}
	dukaIPMu.Lock()
	dukaIPList = ips
	dukaIPMu.Unlock()
	return ips
}

func fetchDoHIPs(ctx context.Context) []string {
	c := &http.Client{Timeout: 8 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dukaDoH, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return nil
	}
	var out struct {
		Answer []struct {
			Type int    `json:"type"`
			Data string `json:"data"`
		} `json:"Answer"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil
	}
	var ips []string
	for _, a := range out.Answer {
		if a.Type != 1 { // 1 = A 记录
			continue
		}
		if net.ParseIP(a.Data) != nil {
			ips = append(ips, a.Data)
		}
	}
	return ips
}

// currentDukaIPs 取当前 IP 列表（空则先解析一次）
func currentDukaIPs(ctx context.Context) []string {
	dukaIPMu.RLock()
	ips := dukaIPList
	dukaIPMu.RUnlock()
	if len(ips) > 0 {
		return ips
	}
	return resolveDukaIPs(ctx)
}

// newDukaClient 造一个「把 dukaHost 定向到指定 IP」的 HTTP 客户端。
//
// 关键：只改 TCP 连接目标，URL 里的域名不动 —— 这样 TLS SNI / Host 头
// 仍是 datafeed.dukascopy.com，证书校验照常，对服务端看起来是正常请求。
func newDukaClient(ctx context.Context, timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err == nil && strings.EqualFold(host, dukaHost) {
				ips := currentDukaIPs(ctx)
				var lastErr error
				for _, ip := range ips {
					conn, derr := dialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
					if derr == nil {
						return conn, nil
					}
					lastErr = derr
				}
				if lastErr != nil {
					return nil, lastErr
				}
			}
			return dialer.DialContext(ctx, network, addr)
		},
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 25 * time.Second,
		IdleConnTimeout:       60 * time.Second,
		MaxIdleConns:          4,
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

// ---------------------------------------------------------------------------
// 下载单日文件
// ---------------------------------------------------------------------------

// dukaDayURL 拼下载地址。★ 月份 0-based ★
func dukaDayURL(day time.Time) string {
	y := day.Year()
	m := int(day.Month()) - 1 // 0-based
	d := day.Day()
	return fmt.Sprintf("https://%s/datafeed/%s/%04d/%02d/%02d/BID_candles_min_1.bi5",
		dukaHost, nqDukaSymbol, y, m, d)
}

// fetchDukaDay 下载一天的 .bi5。
//
// 返回：raw（解压前字节）/ exists（false = 当天休市，文件不存在，**不是错误**）/ err
//
// 限流应对：429 / 503 / 5xx / 网络错误 → 指数退避重试；
// 404 直接判「休市」，不重试（这是正常结果，重试只会白耗限流配额）。
func fetchDukaDay(ctx context.Context, hc *http.Client, day time.Time, logf func(string, ...any)) ([]byte, bool, error) {
	url := dukaDayURL(day)
	backoff := 15 * time.Second
	var lastErr error

	for attempt := 1; attempt <= 3; attempt++ {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, false, err
		}
		// 不加 UA / Referer 会被更早限流，实测带上后成功率明显改善
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36")
		req.Header.Set("Referer", "https://www.dukascopy.com/")
		req.Header.Set("Accept", "*/*")

		resp, err := hc.Do(req)
		if err != nil {
			// ★ 连接失败 / 超时：限流最典型的表现就是「挂住不响应」。
			//   不重试 —— 这属于 IP 级冷却，同一轮里再打也是白打。
			return nil, false, fmt.Errorf("%w: %v", errNQLimited, err)
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if rerr != nil {
			lastErr = rerr
			continue
		}

		switch {
		case resp.StatusCode == http.StatusOK:
			if len(body) < 13 {
				// 太短：不是有效的 LZMA 流（Dukascopy 偶尔回空体）
				lastErr = fmt.Errorf("响应体过短 %d 字节", len(body))
				if attempt < 3 {
					sleepCtx(ctx, backoff)
					backoff *= 2
				}
				continue
			}
			return body, true, nil

		case resp.StatusCode == http.StatusNotFound:
			// ★ 休市（周末/节假日）—— 官方 wiki：文件缺失 ≠ 错误
			return nil, false, nil

		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			// ★ 429 / 503 是明确的限流信号：立刻交棒给上层冷却，不原地重试
			return nil, false, fmt.Errorf("%w: HTTP %d", errNQLimited, resp.StatusCode)

		default:
			return nil, false, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
	}
	return nil, false, fmt.Errorf("重试 3 次仍失败：%w", lastErr)
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// ---------------------------------------------------------------------------
// 解码 .bi5
// ---------------------------------------------------------------------------

// dukaMin 一条 1 分钟记录
type dukaMin struct {
	Off int // 距当天 00:00 UTC 的秒偏移
	O   float64
	H   float64
	L   float64
	C   float64
	V   float64
}

// decodeBi5 解压并解析 .bi5。
//
// 文件自带 13 字节 .lzma 头（props 5D + dictSize + uncompressedSize），
// 所以这里用标准 lzma.NewReader 直接读，**不要**再自己拼头
// —— 拼了反而会把真实头当成数据，解出一堆垃圾。
func decodeBi5(raw []byte) ([]dukaMin, error) {
	lr, err := lzma.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("lzma 头解析失败：%w", err)
	}
	buf, err := io.ReadAll(lr)
	if err != nil {
		return nil, fmt.Errorf("lzma 解压失败：%w", err)
	}
	if len(buf)%24 != 0 {
		return nil, fmt.Errorf("解压后长度 %d 不是 24 的整数倍", len(buf))
	}
	n := len(buf) / 24
	out := make([]dukaMin, 0, n)
	for i := 0; i < n; i++ {
		b := buf[i*24 : (i+1)*24]
		// ★ 字段顺序 O / C / L / H（不是 O/H/L/C）
		off := int(binary.BigEndian.Uint32(b[0:4]))
		o := float64(binary.BigEndian.Uint32(b[4:8])) / nqPointDiv
		c := float64(binary.BigEndian.Uint32(b[8:12])) / nqPointDiv
		l := float64(binary.BigEndian.Uint32(b[12:16])) / nqPointDiv
		h := float64(binary.BigEndian.Uint32(b[16:20])) / nqPointDiv
		v := float64(math.Float32frombits(binary.BigEndian.Uint32(b[20:24])))

		// 基本自洽校验：不自洽的一条直接丢，别让它污染指标
		if o <= 0 || h <= 0 || l <= 0 || c <= 0 || h < l {
			continue
		}
		out = append(out, dukaMin{Off: off, O: o, H: h, L: l, C: c, V: v})
	}
	if len(out) == 0 {
		return nil, errors.New("解压后没有有效记录")
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Off < out[j].Off })
	return out, nil
}

// ---------------------------------------------------------------------------
// 聚合 1m → 3m / 5m / 15m
// ---------------------------------------------------------------------------

// barSeconds 周期 → 秒
func barSeconds(bar string) int {
	switch bar {
	case "3m":
		return 180
	case "5m":
		return 300
	case "15m":
		return 900
	}
	return 0
}

// aggregateNQDay 把一天的分时聚合成指定周期的 K 线。
//
// 分桶按「距当天 00:00 UTC 的秒偏移 ÷ 周期秒数」—— 因为 1440 能被 3/5/15 整除，
// 桶边界天然落在 UTC 整点/整刻，与 OKX K 线的时间轴口径**完全一致**
// （OKX 的 ts 也是「开盘时间 + 毫秒 UTC」），所以两边的图可以直接对齐看。
//
// dayStartMs 必须是当天 00:00 UTC 的毫秒时间戳（由调用方用 UTC 构造）。
func aggregateNQDay(recs []dukaMin, dayStartMs int64, bar string) []model.Kline {
	step := barSeconds(bar)
	if step <= 0 || len(recs) == 0 {
		return nil
	}
	out := make([]model.Kline, 0, len(recs)/step*step/60+4)
	var cur model.Kline
	curBucket := int64(-1)
	has := false

	flush := func() {
		if has {
			out = append(out, cur)
		}
	}
	for _, r := range recs {
		b := int64(r.Off) / int64(step)
		if b != curBucket {
			flush()
			cur = model.Kline{
				InstID: NQInstID,
				Bar:    bar,
				Ts:     dayStartMs + b*int64(step)*1000, // ★ 毫秒，且对齐整秒（IsValidKline 要求 ts%1000==0）
				O:      r.O, H: r.H, L: r.L, C: r.C, V: r.V,
			}
			curBucket = b
			has = true
			continue
		}
		if r.H > cur.H {
			cur.H = r.H
		}
		if r.L < cur.L {
			cur.L = r.L
		}
		cur.C = r.C
		cur.V += r.V
	}
	flush()
	return out
}

// ---------------------------------------------------------------------------
// 同步一次
// ---------------------------------------------------------------------------

// NQSyncResult 一次同步的结果（给日志/接口用）
type NQSyncResult struct {
	Missing   int            // 缺口天数（库里还没有的天数）
	Days      int            // 本**轮**实际尝试下载的天数（受 nqMaxPerRound 限制）
	Skipped   int            // 因单轮限量而留待下轮的天数
	Empty     int            // 休市（404）的天数
	Failed    int            // 失败天数
	Limited   bool           // 本轮是否因数据源限流而提前中止
	Bars      map[string]int // 各周期写入行数
	Signals   int            // 产出的信号条数
	EarliestT int64          // 最早 ts（毫秒）
	LatestT   int64          // 最新 ts（毫秒）
	Elapsed   time.Duration
}

// nqMissingDays 算出最近 nqDays 天里**库里还没有数据**的那些天，从旧到新返回。
//
// 最近 2 天（今天 / 昨天）永远算「缺」，强制每轮刷新 ——
// 当天数据还在持续生成，昨天也可能在收盘后才补齐尾部。
func nqMissingDays(db *repo.DB, today time.Time) ([]time.Time, error) {
	from := today.AddDate(0, 0, -(nqDays - 1)).UnixMilli()
	ks, err := db.QueryKlines(model.KlineQuery{
		InstID: NQInstID, Bar: "15m", FromTs: from, Asc: true,
	})
	if err != nil {
		return nil, err
	}
	have := make(map[int64]bool, len(ks))
	for _, k := range ks {
		if k.Ts <= 0 {
			continue
		}
		d := time.UnixMilli(k.Ts).UTC().Truncate(24 * time.Hour)
		have[d.UnixMilli()] = true
	}
	miss := make([]time.Time, 0, nqDays)
	for i := nqDays - 1; i >= 0; i-- { // 从旧到新：保证图的左侧先连续
		d := today.AddDate(0, 0, -i)
		if !have[d.UnixMilli()] || i <= 1 {
			miss = append(miss, d)
		}
	}
	return miss, nil
}

// nqAllDays 最近 nqDays 天（旧 → 新）。库里读不出来时按「全缺」处理。
func nqAllDays(today time.Time) []time.Time {
	out := make([]time.Time, 0, nqDays)
	for i := nqDays - 1; i >= 0; i-- {
		out = append(out, today.AddDate(0, 0, -i))
	}
	return out
}

var nqSyncBusy sync.Mutex

// SyncNQOnce 拉最近 nqDays 天的 Dukascopy 数据 → 聚合 3 个周期 → 写 kline → 算信号。
//
// 增量策略：库里已有数据时只补「最近 2 天」（当天可能还在生成），
// 历史缺口靠 days 参数第一次回补。这样每小时跑一轮的开销极小。
func SyncNQOnce(ctx context.Context, db *repo.DB, logf func(string, ...any)) (NQSyncResult, error) {
	res := NQSyncResult{Bars: map[string]int{}}
	start := time.Now()

	nqSyncBusy.Lock()
	defer nqSyncBusy.Unlock()

	// 解析一次 DNS（DoH），失败退内置 IP
	ips := resolveDukaIPs(ctx)
	logf("[NQ] 数据源 %s 解析到 %v", dukaHost, ips)

	hc := newDukaClient(ctx, 60*time.Second)

	// ★ 先把「只读合约」写进 inst 表 —— 这一步**不能**排在下载之后 ★
	//
	// 原先是放在同步末尾，结果是：Dukascopy 一限流（首轮几乎必然），
	// 前端的 NQ 入口就要等到下载成功才出现，看起来像功能根本没上。
	// 「注册」和「数据」是两件事：注册无条件先做，数据慢慢补。
	if err := ensureNQInstrument(db); err != nil {
		logf("[NQ] 注册只读合约失败：%v", err)
	}

	// ---- 决定要拉哪些天 ----
	//
	// ★ 关键设计：按「缺口」补，而不是「每次重拉最近 N 天」。
	//   因为 Dukascopy 会在连续请求后进入惩罚冷却（所有请求挂住），
	//   一轮几乎不可能拉满 30 天。按缺口补 + 单轮限量，就能：
	//     被打断 → 已拿到的不重拉，下轮从缺口继续 → 多轮收敛。
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	missing, merr := nqMissingDays(db, today)
	if merr != nil {
		logf("[NQ] 读已有数据失败（按全部缺失处理）：%v", merr)
		missing = nqAllDays(today)
	}
	res.Missing = len(missing)

	// 单轮限量：一次打太多必进冷却，反而更慢
	batch := missing
	if len(batch) > nqMaxPerRound {
		batch = batch[:nqMaxPerRound]
	}
	res.Days = len(batch)
	res.Skipped = len(missing) - len(batch)

	allByBar := make(map[string][]model.Kline, len(NQBars))
	var minTs, maxTs int64

	for _, day := range batch {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		raw, exists, err := fetchDukaDay(ctx, hc, day, logf)
		if err != nil {
			res.Failed++
			logf("[NQ] %s 下载失败：%v", day.Format("2006-01-02"), err)
			// ★ 限流是 IP 级、全局性的：同一轮里换个日期再打也是白打，
			//   只会把冷确期拖得更长。立刻中止本轮，缺口留给下一轮 —— 慢就是快。
			if errors.Is(err, errNQLimited) {
				res.Limited = true
				logf("[NQ] 判定数据源限流冷却中，本轮中止（缺口 %d 天留待下轮）", len(missing)-res.Empty)
				break
			}
			sleepCtx(ctx, nqCooldownGap)
			continue
		}
		if !exists {
			res.Empty++ // 周末/休市，正常
			sleepCtx(ctx, nqReqGap)
			continue
		}
		recs, derr := decodeBi5(raw)
		if derr != nil {
			res.Failed++
			logf("[NQ] %s 解码失败：%v", day.Format("2006-01-02"), derr)
			sleepCtx(ctx, nqReqGap)
			continue
		}
		dayStartMs := day.UnixMilli()
		for _, bar := range NQBars {
			ks := aggregateNQDay(recs, dayStartMs, bar)
			if len(ks) == 0 {
				continue
			}
			allByBar[bar] = append(allByBar[bar], ks...)
			if minTs == 0 || ks[0].Ts < minTs {
				minTs = ks[0].Ts
			}
			if last := ks[len(ks)-1].Ts; last > maxTs {
				maxTs = last
			}
		}
		sleepCtx(ctx, nqReqGap) // ★ 限流：必须的间隔
	}

	// ---- 落库 ----
	for _, bar := range NQBars {
		rows := allByBar[bar]
		if len(rows) == 0 {
			continue
		}
		n, err := db.UpsertKlines(rows)
		if err != nil {
			logf("[NQ] %s 写库失败：%v", bar, err)
			continue
		}
		res.Bars[bar] = n
	}
	res.EarliestT, res.LatestT = minTs, maxTs

	// ---- 算信号 ----
	//
	// ★ 不能用现有的 RunSignalBackfillOnce：它只遍历 TradeableInstIDs()，
	//   而 NQ 刻意是 tradeable=0（不可交易），永远不会被它扫到。
	//   所以这里**显式**给 NQ 自己跑一遍 —— 信号照样算、照样展示，只是不下单。
	cfg := loadStrategyConfigForNQ()
	if cfg != nil {
		for _, bar := range NQBars {
			n, err := BackfillSignalsFor(cfg, db, NQInstID, bar)
			if err != nil {
				logf("[NQ] %s 信号回算失败：%v", bar, err)
				continue
			}
			res.Signals += n
		}
	}

	res.Elapsed = time.Since(start)
	limitMark := ""
	if res.Limited {
		limitMark = " · **限流中止**"
	}
	logf("[NQ] 同步完成：缺口 %d 天 · 本轮拉 %d 天 · 留待下轮 %d 天 · 休市 %d · 失败 %d%s · 写入 %v · 信号 %d 条 · 耗时 %s",
		res.Missing, res.Days, res.Skipped, res.Empty, res.Failed, limitMark, res.Bars, res.Signals,
		res.Elapsed.Round(time.Millisecond))
	return res, nil
}

// ensureNQInstrument 把 NQ 写进 inst 表（tradeable=0，reason=readonly）。
//
// 为什么必须进表：前端的合约列表、图表标题、K 线查询的元信息都走 inst 表，
// 不进表就只是个"孤儿 K 线"，界面上什么都查不到。
func ensureNQInstrument(db *repo.DB) error {
	rows := []model.Instrument{{
		InstID:        NQInstID,
		BaseCcy:       "NQ",
		QuoteCcy:      "USD",
		SettleCcy:     "USD",
		State:         "live",
		InstCategory:  "9", // 自定义：外部指数（不是 OKX 的 1/3/4）
		Tradeable:     0,
		ExcludeReason: "readonly",
		TickSz:        0.25,
		ListTime:      0,
	}}
	_, err := db.UpsertInstruments(rows)
	if err != nil {
		return err
	}
	// UpsertInstruments 不写 tradeable/排除原因（那是准入过滤的职责），这里单独钉死
	return db.UpdateTradeable(rows)
}

// ---------------------------------------------------------------------------
// 定时任务
// ---------------------------------------------------------------------------

// nqSyncInterval 稳态下的增量同步间隔。
// 数据是 1 分钟粒度，且 Dukascopy 限流很硬 —— 每小时一次完全够，
// 也不会给限流配额添压力。
const nqSyncInterval = 60 * time.Minute

// nqFillInterval 还在「补历史缺口」阶段用的间隔。
// 首轮受 nqMaxPerRound 限制拿不全 30 天，用短间隔多跑几轮尽快补齐；
// 补满后自动切回 nqSyncInterval。
const nqFillInterval = 5 * time.Minute

// nqLimitedInterval 被限流中止后的等待时间。
// 惩罚冷却通常要十几分钟，5 分钟去敲多半还是 503 —— 白跑还加深冷却。
const nqLimitedInterval = 10 * time.Minute

// StartNQSync 启动 NQ 只读行情同步（启动时回补，之后增量）。
//
// 照抄 StartOKXFillsSync 的形状：先错开启动高峰，失败只记日志不中断循环。
// 唯一的不同是**间隔动态**：有缺口就快跑（补齐），无缺口就慢跑（稳态）。
func StartNQSync(ctx context.Context, db *repo.DB, logf func(string, ...any)) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logf("[NQ] 同步协程退出：%v", r)
			}
		}()

		// 错开启动高峰：OKX 合约同步 / K 线回补都在抢网络
		select {
		case <-ctx.Done():
			return
		case <-time.After(40 * time.Second):
		}

		for {
			wait := nqSyncInterval
			func() {
				defer func() {
					if r := recover(); r != nil {
						logf("[NQ] 单轮同步 panic：%v", r)
					}
				}()
				res, err := SyncNQOnce(ctx, db, logf)
				if err != nil && ctx.Err() == nil {
					logf("[NQ] 同步出错（下轮重试）：%v", err)
					wait = nqFillInterval
					return
				}
				if res.Skipped > 0 || res.Failed > 0 {
					wait = nqFillInterval // 还有缺口，短间隔继续补
				}
				if res.Limited {
					wait = nqLimitedInterval // 被限流：等冷却期过去再敲
				}
			}()

			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}()
}

// loadStrategyConfigForNQ 取策略配置给信号回算用。
//
// 直接用 conf.LoadConfig()（与下单/准入同源的那一份），不另建解析器 ——
// 否则「图上算了信号、实际口径却不同」这种问题会再次出现。
func loadStrategyConfigForNQ() *conf.Config {
	return conf.LoadConfig()
}
