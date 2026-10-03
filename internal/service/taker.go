package service

// taker.go —— taker 买卖量（主动买 / 主动卖）接入
//
// 数据源：OKX priapi indicators 的 takerBuySellVol
//   https://www.okx.com/priapi/v5/rubik/public/stat/indicators
//       ?bar=5m&instId=ETH-USDT-SWAP&unit=2&limit=1030&indicators=takerBuySellVol
//
// 返回 [ts, buyVol, sellVol]（★ 顺序是「买在前」，与老 flow.go 里
// taker-volume 接口的「卖在前」相反，别抄错）。
//
// ---------------------------------------------------------------------------
// 历史精度约束（2026-10-03 实测，写死在代码里的硬事实）
// ---------------------------------------------------------------------------
//   - 该接口**最多回溯 5 天**，且单次上限 1030 根。翻页靠 `after` 游标
//     （往老的一侧翻），`before` 是真正的「截止点」，`limit` 只影响单页条数。
//     实测 5m：1030 + 409 = 1439 根 = 5.00 天，再翻就是空。
//   - 1H 粒度能回溯 60 天（1440 根到底），足够覆盖 30 天窗口。
//
// 所以 30 天窗口的真实构成是：
//   [now-5天, now]        → 真 5m 数据，src='5m'
//   [now-30天, now-5天]   → 1H 前向填充，src='1Hfill'
//
// ★ 前向填充必须滞后 1 小时。
//   1H 的第 T 根统计的区间是 [T, T+1h)，如果把它填给 T 时刻的 12 个 5m 格，
//   就等于「5m 格子提前知道了未来 55 分钟的量」——典型未来函数。
//   正确做法是把 1H 的第 T 根填给 [T+1h, T+2h) 区间（滞后一整根），
//   与二十期 taker 1H 回补的口径保持一致。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"finally-main/internal/logx"
	"finally-main/internal/repo"
)

const (
	// taker5mMaxLookbackDays 5m 真数据的最大回溯天数（实测 5.00 天，留 30 分钟余量）
	taker5mMaxLookbackDays = 5

	// takerBar 面板固定看 5 分钟切片
	takerBar = "5m"

	// takerPageLimit priapi 单页上限（实测 1030 生效）
	takerPageLimit = 1030

	// takerLagMS 1H 前向填充的滞后量（1 小时）
	takerLagMS = int64(time.Hour / time.Millisecond)

	// takerSrc5m / takerSrc1H 精度来源标记
	takerSrc5m   = "5m"
	takerSrc1H   = "1Hfill"
	takerBarMS   = int64(5 * time.Minute / time.Millisecond)
)

// takerIndURL 拼 priapi indicators 的 URL
func takerIndURL(instID, bar string, afterMs int64, limit int) string {
	u := "https://www.okx.com/priapi/v5/rubik/public/stat/indicators?bar=" + bar +
		"&instId=" + instID + "&unit=2&limit=" + strconv.Itoa(limit) +
		"&indicators=takerBuySellVol&t=" + strconv.FormatInt(time.Now().UnixMilli(), 10)
	if afterMs > 0 {
		u += "&after=" + strconv.FormatInt(afterMs, 10)
	}
	return u
}

// takerHTTPGet 一次 GET，带项目一贯的 priapi 头
func takerHTTPGet(rawurl string) ([]byte, error) {
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("authority", "www.okx.com")
	req.Header.Set("accept", "application/json")
	req.Header.Set("accept-language", "zh-CN")
	req.Header.Set("app-type", "web")
	req.Header.Set("x-utc", "8")
	req.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("referer", "https://www.okx.com/trade-swap/"+lowerInst(rawurl))
	cli := &http.Client{Timeout: 25 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// lowerInst 从 URL 里抠出 instId 拼 referer（对 OKX 反爬更友好）
func lowerInst(u string) string {
	const key = "instId="
	i := indexOf(u, key)
	if i < 0 {
		return "btc-usdt-swap"
	}
	rest := u[i+len(key):]
	j := indexOf(rest, "&")
	if j < 0 {
		j = len(rest)
	}
	return rest[:j]
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// takerFetchPage 拉一页，返回 [ts, buy, sell] 升序。
//
// ★ 429 退避重试（2026-10-03 实测踩到）：
//   priapi 对同一 IP 的限频很紧，6 并发 + 连续翻页必吃 429。
//   不加退避的话整轮白跑 —— 而且失败点在「调用方拿数据前」，
//   已经翻好的页也一起丢了，代价很大。
//   这里做 3 次尝试，间隔 1.2s / 2.5s / 5s（指数退避）。
func takerFetchPage(instID, bar string, afterMs int64) ([][3]float64, error) {
	var lastErr error
	backoff := []time.Duration{1200 * time.Millisecond, 2500 * time.Millisecond, 5 * time.Second}
	for attempt := 0; attempt <= len(backoff); attempt++ {
		body, err := takerHTTPGet(takerIndURL(instID, bar, afterMs, takerPageLimit))
		if err != nil {
			lastErr = err
			// 只有 429 才重试；其它错误（404/解析失败）立刻返回，重试没意义
			if !strings.Contains(err.Error(), "429") {
				return nil, err
			}
			if attempt < len(backoff) {
				time.Sleep(backoff[attempt])
				continue
			}
			return nil, err
		}
		var raw struct {
			Code string `json:"code"`
			Data struct {
				Rows [][]string `json:"takerBuySellVol"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, err
		}
		if raw.Code != "0" {
			return nil, fmt.Errorf("code=%s", raw.Code)
		}
		out := make([][3]float64, 0, len(raw.Data.Rows))
		// 接口返回新 → 老，翻成老 → 新
		for i := len(raw.Data.Rows) - 1; i >= 0; i-- {
			r := raw.Data.Rows[i]
			if len(r) < 3 {
				continue
			}
			ts, _ := strconv.ParseFloat(r[0], 64)
			buy, _ := strconv.ParseFloat(r[1], 64)
			sell, _ := strconv.ParseFloat(r[2], 64)
			out = append(out, [3]float64{ts, buy, sell})
		}
		return out, nil
	}
	return nil, lastErr
}

// TakerFetchResult 单个合约的拉取结果
type TakerFetchResult struct {
	InstID string
	Rows    []repo.TakerVol
	Err     error
}

// fetchTakerHistory 拉一个合约的 [fromMs, nowMs] 区间数据。
//
// 策略：先拉 5m（最多 5 天），拿到的最老一根早于 fromMs 就停；
// 否则用 1H 补剩余的老区间，前向填充 + 滞后 1h。
//
// ★ 顺序即优先级（来源：二十期 NQ 下载血泪教训）：必须先拉 5m 真数据，
// 再拉 1H 填充。反过来的话 1H 会先占住主键位置，虽然 src 覆盖能救回来，
// 但会白白多写一遍、多一次 ON DUPLICATE 争用。
func fetchTakerHistory(instID string, fromMs, nowMs int64) TakerFetchResult {
	res := TakerFetchResult{InstID: instID}

	// 5m 真数据回溯下限
	cut5m := nowMs - int64(taker5mMaxLookbackDays)*24*int64(time.Hour/time.Millisecond)
	from5m := fromMs
	if cut5m > from5m {
		from5m = cut5m
	}

	rows := make([]repo.TakerVol, 0, 2048)

	// ---- 第一段：5m 真数据（从新往老翻页）----
	after := nowMs + takerBarMS // 略大于当前，保证第一页就含最新
	deadline := 0
	for page := 0; page < 12; page++ {
		pg, err := takerFetchPage(instID, "5m", after)
		if err != nil {
			res.Err = err
			break
		}
		if len(pg) == 0 {
			break
		}
		oldest := int64(pg[0][0])
		for _, r := range pg {
			ts := int64(r[0])
			if ts < from5m || ts > nowMs {
				continue
			}
			rows = append(rows, repo.TakerVol{
				InstID: instID, Bar: takerBar, Ts: ts,
				BuyVol: r[1], SellVol: r[2], Src: takerSrc5m,
			})
		}
		// 翻到底或翻够了就停
		if oldest <= from5m || len(pg) < takerPageLimit {
			break
		}
		after = oldest
		deadline = page
		time.Sleep(250 * time.Millisecond) // 温柔对待接口，429 很难受
	}
	_ = deadline

	// ---- 第二段：1H 前向填充（补 5m 够不到的老区间）----
	if fromMs < cut5m {
		fillEnd := cut5m // 填充到 5m 数据的起点，无缝衔接
		afterH := nowMs
		for page := 0; page < 4; page++ {
			pg, err := takerFetchPage(instID, "1H", afterH)
			if err != nil {
				if res.Err == nil {
					res.Err = err
				}
				break
			}
			if len(pg) == 0 {
				break
			}
			oldest := int64(pg[0][0])
			for _, r := range pg {
				hTs := int64(r[0])
				// ★ 滞后 1h：1H 的第 T 根填 [T+1h, T+2h)
				base := hTs + takerLagMS
				for k := int64(0); k < 12; k++ {
					ts := base + k*takerBarMS
					if ts < fromMs || ts >= fillEnd {
						continue
					}
					rows = append(rows, repo.TakerVol{
						InstID: instID, Bar: takerBar, Ts: ts,
						BuyVol: r[1] / 12, SellVol: r[2] / 12, Src: takerSrc1H,
					})
				}
			}
			if oldest <= fromMs-int64(24*time.Hour/time.Millisecond) || len(pg) < takerPageLimit {
				break
			}
			afterH = oldest
			time.Sleep(250 * time.Millisecond)
		}
	}

	res.Rows = rows
	return res
}

// TakerPoolInput 挑池子需要的合约信息（避免 service 依赖 repo 的具体类型）
type TakerPoolInput struct {
	InstID       string
	InstCategory string
}

// TakerPoolByVolume 挑出 taker 面板与同步用的合约池：
// 非美股非ETF（instCategory=1 或空）里，24h 成交额前 topN。
//
// ★ 做成公共函数而不是在 cmd / handler 里各写一份：
//   两处各写一份就是「同一量两条路算」的老坑（见技能 config-change-effect-audit），
//   哪天口径要改（比如把 ETF 也算进来），漏改一处就会让面板和引擎看的不是同一批合约。
//   排序带 instID 兜底，保证同成交额时池子顺序稳定（否则前端同一格每次刷新换合约）。
func TakerPoolByVolume(insts []TakerPoolInput, vol map[string]float64, topN int) []string {
	type cand struct {
		id  string
		vol float64
	}
	list := make([]cand, 0, len(insts))
	for _, it := range insts {
		if it.InstCategory != "" && it.InstCategory != "1" {
			continue
		}
		v, ok := vol[it.InstID]
		if !ok || v <= 0 {
			continue
		}
		list = append(list, cand{id: it.InstID, vol: v})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].vol != list[j].vol {
			return list[i].vol > list[j].vol
		}
		return list[i].id < list[j].id
	})
	if topN > 0 && len(list) > topN {
		list = list[:topN]
	}
	out := make([]string, len(list))
	for i, c := range list {
		out[i] = c.id
	}
	return out
}

// TakerBackfillInsts 批量回补指定合约（并发）
//
// 返回成功合约数、写入行数。误差合约在日志里逐个报到，不阻塞其他合约。
//
// ★ 并发默认压到 3（2026-10-03 实测）：priapi 限频很紧，6 并发会整片 429。
//   80 个合约 × 3 并发 × 每合约 2~3 个请求 ≈ 1 分钟铺完，完全可以接受。
func TakerBackfillInsts(d *repo.DB, insts []string, days int, workers int) (int, int) {
	if len(insts) == 0 {
		return 0, 0
	}
	if workers < 1 {
		workers = 3
	}
	if workers > 4 {
		workers = 4 // 上限压死：再多就是给 429 送人头
	}
	nowMs := time.Now().UnixMilli()
	// 对齐到 5m 边界，避免出现 11:23 这种非整根时间戳
	nowMs = nowMs / takerBarMS * takerBarMS
	fromMs := nowMs - int64(days)*24*int64(time.Hour/time.Millisecond)

	type job struct{ inst string }
	jobs := make(chan job)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		okInst   int
		totalRow int
		failed   int
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				r := fetchTakerHistory(j.inst, fromMs, nowMs)
				// ★ 只要拿到了行，就先写库再判错（2026-10-03 修复）。
				//
				// 原实现是 `if r.Err != nil || len(r.Rows) == 0 { 丢掉 }` ——
				// 结果「5m 段翻页时最后一页吃 429」会让**已经翻好的 1440 行
				// 一起被丢**，日志只留一句「失败」，数据白拉。
				// 现在改成「有行就写」，错误只作为日志参考。
				if len(r.Rows) == 0 {
					mu.Lock()
					failed++
					if failed <= 5 {
						logx.Logf("WARN", "[TAKER] %s 回补失败（无数据）：%v", j.inst, r.Err)
					}
					mu.Unlock()
					continue
				}
				if _, err := d.UpsertTakerVols(r.Rows); err != nil {
					mu.Lock()
					failed++
					logx.Logf("WARN", "[TAKER] %s 写入失败：%v", j.inst, err)
					mu.Unlock()
					continue
				}
				if r.Err != nil {
					// 部分成功：数据已入库，只是没拉满 30 天
					logx.Logf("WARN", "[TAKER] %s 部分拉取失败（已写入 %d 行）：%v", j.inst, len(r.Rows), r.Err)
				}
				// 记水位线
				minTs, maxTs := r.Rows[0].Ts, r.Rows[0].Ts
				src := takerSrc5m
				for _, x := range r.Rows {
					if x.Ts < minTs {
						minTs = x.Ts
					}
					if x.Ts > maxTs {
						maxTs = x.Ts
					}
					if x.Src == takerSrc1H {
						src = takerSrc1H
					}
				}
				d.SaveTakerState(j.inst, takerBar, minTs, maxTs, int64(len(r.Rows)), src, time.Now().UnixMilli())

				mu.Lock()
				okInst++
				totalRow += len(r.Rows)
				mu.Unlock()
			}
		}()
	}
	for _, s := range insts {
		jobs <- job{inst: s}
	}
	close(jobs)
	wg.Wait()
	if failed > 0 {
		logx.Logf("INFO", "[TAKER] 回补完成：成功 %d 个，失败 %d 个，写入 %d 行", okInst, failed, totalRow)
	}
	return okInst, totalRow
}

// ---------------------------------------------------------------------------
// 实时增量
// ---------------------------------------------------------------------------

// TakerSyncLatest 只补最新几根 5m（每分钟调一次，代价极小）
//
// 与回补的区别：回补是「从最近往老铺 30 天」，实时是「追最新 1 小时」。
// 只拉 1 页（1030 根覆盖 3.5 天，绰绰有余），写入幂等。
func TakerSyncLatest(d *repo.DB, insts []string, workers int) (int, int) {
	if len(insts) == 0 {
		return 0, 0
	}
	if workers < 1 {
		workers = 4
	}
	if workers > 8 {
		workers = 8
	}
	nowMs := time.Now().UnixMilli() / takerBarMS * takerBarMS
	// 只补最近 2 小时，避免每轮重复写 3.5 天的数据
	fromMs := nowMs - int64(2*time.Hour/time.Millisecond)

	type job struct{ inst string }
	jobs := make(chan job)
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		ok   int
		rows int
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				pg, err := takerFetchPage(j.inst, "5m", nowMs+takerBarMS)
				if err != nil || len(pg) == 0 {
					continue
				}
				batch := make([]repo.TakerVol, 0, 32)
				for _, r := range pg {
					ts := int64(r[0])
					if ts < fromMs || ts > nowMs {
						continue
					}
					batch = append(batch, repo.TakerVol{
						InstID: j.inst, Bar: takerBar, Ts: ts,
						BuyVol: r[1], SellVol: r[2], Src: takerSrc5m,
					})
				}
				if len(batch) == 0 {
					continue
				}
				if n, err := d.UpsertTakerVols(batch); err == nil {
					mu.Lock()
					ok++
					rows += n
					mu.Unlock()
				}
			}
		}()
	}
	for _, s := range insts {
		jobs <- job{inst: s}
	}
	close(jobs)
	wg.Wait()
	return ok, rows
}
