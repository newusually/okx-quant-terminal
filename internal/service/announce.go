package service

// announce.go —— OKX 公告抓取与「不能买」名单
//
// 需求：不要那些「要下线」或者「刚上线」的合约。
//
// 两个来源：
//   1. 下线名单 —— 抓 OKX 公告中心 announcements-delistings，
//      把标题里出现过的币种解析出来，进黑名单。
//      例："OKX to delist perpetual futures for ONEUSDT"   → ONE
//          "OKX to delist X-Perp for SKDDUSD"             → SKDD
//          "OKX to delist DORA, ICX, STORJ, ZEUS and ELF spot trading pairs"
//                                                          → DORA/ICX/STORJ/ZEUS/ELF
//      注意 "postpone the delisting"（延后下线）不算，要跳过。
//   2. 新上线名单 —— 用合约自身的 listTime 判断：上市不足 N 天的一律不碰。
//      （公告的 announcements-new-listings 作为补充，用于拿不到 listTime 的场景）
//
// 结果缓存在 MySQL 的 meta 表里，避免每次都打公告接口。

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"finally-main/internal/repo/okx"
)

func itoa(i int) string { return strconv.Itoa(i) }

// ---------------------------------------------------------------------------
// 公告数据结构
// ---------------------------------------------------------------------------

// Announcement 一条公告
type Announcement struct {
	AnnType string `json:"annType"`
	Title   string `json:"title"`
	PTime   int64  `json:"pTime"` // 毫秒
	URL     string `json:"url"`
}

// DelistEntry 某个币种被列入下线名单
type DelistEntry struct {
	Symbol string `json:"symbol"` // 币种，如 ONE
	Title  string `json:"title"`  // 触发它的公告标题
	PTime  int64  `json:"pTime"`
}

// FilterStats 过滤统计（打印 / 展示用）
type FilterStats struct {
	Total            int            `json:"total"`           // 抓到的全部合约
	Kept             int            `json:"kept"`            // 通过全部过滤后剩下
	DroppedCategory  int            `json:"droppedCategory"` // 美股 / ETF / 商品
	DroppedState     int            `json:"droppedState"`    // 非 live
	DroppedNew       int            `json:"droppedNew"`      // 刚上线
	DroppedDelist    int            `json:"droppedDelist"`   // 即将下线
	DroppedVolume    int            `json:"droppedVolume"`   // 24h 成交额不足
	DroppedNotional  int            `json:"droppedNotional"` // 0.1U 买不起最小一手
	ScaledUp         int            `json:"scaledUp"`        // 0.1U 买不起 1 张但没超硬上限，下单会放大
	DelistSymbols    []string       `json:"delistSymbols"`   // 命中的下线币种
	NewListingSymbol []string       `json:"newListingSymbols"`
	Delist           []DelistEntry  `json:"delist"`
	ByCategory       map[string]int `json:"byCategory"`
}

// ---------------------------------------------------------------------------
// 公告抓取
// ---------------------------------------------------------------------------

// FetchAnnouncements 抓公告。annType 留空 = 全部类型。pages 为翻页数（每页 20 条）
func (f *DataFeed) FetchAnnouncements(annType string, pages int) ([]Announcement, error) {
	if pages <= 0 {
		pages = 3
	}
	out := make([]Announcement, 0, pages*20)
	for p := 1; p <= pages; p++ {
		resp, err := f.pub.GetAnnouncements(annType, itoa(p))
		if err != nil {
			if p == 1 {
				return nil, err
			}
			break // 翻页失败就用已拿到的
		}
		for _, blk := range okx.Data(resp) {
			details, _ := blk["details"].([]any)
			for _, d := range details {
				dm, _ := d.(map[string]any)
				if dm == nil {
					continue
				}
				out = append(out, Announcement{
					AnnType: okx.Str(dm, "annType"),
					Title:   okx.Str(dm, "title"),
					PTime:   okx.ToInt64(okx.Str(dm, "pTime")),
					URL:     okx.Str(dm, "url"),
				})
			}
		}
		time.Sleep(120 * time.Millisecond) // 公告接口有频率限制，礼貌一点
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 下线名单解析
// ---------------------------------------------------------------------------

// skipWords 标题里出现这些词说明「不是真的要下线」
var skipWords = []string{"postpone", "delay", "resume", "cancel"}

// BuildDelistList 从公告里解析出下线币种
//
// universe 是「当前在交易的合约基础币种集合」，只在这个集合里匹配，
// 这样既准又不会把 "OKX"、"USDT" 这种词误判成币种。
//
// 两遍扫描：
//
//	第一遍  记下「推迟 / 取消下线」类公告的币种 + 时间
//	第二遍  收集下线公告；如果该币种存在一条更晚的「推迟 / 取消」公告，
//	        说明这次下线已经作废，不加进黑名单。
//
// 这个顺序很重要：OKX 经常出现「9/17 说 delist ONEUSDT」→「9/18 又说 postpone」，
// 只跳过 postpone 那条是不够的，必须让它把更早的下线公告一起撤销。
func BuildDelistList(anns []Announcement, universe map[string]bool, withinDays int) []DelistEntry {
	if withinDays <= 0 {
		withinDays = 30
	}
	cut := time.Now().AddDate(0, 0, -withinDays).UnixMilli()

	// ---- 第一遍：推迟 / 取消 ----
	cancelled := map[string]int64{}
	for _, a := range anns {
		if a.PTime > 0 && a.PTime < cut {
			continue
		}
		low := strings.ToLower(a.Title)
		hasSkip := false
		for _, w := range skipWords {
			if strings.Contains(low, w) {
				hasSkip = true
				break
			}
		}
		if !hasSkip {
			continue
		}
		for _, sym := range matchSymbols(a.Title, universe) {
			if a.PTime > cancelled[sym] {
				cancelled[sym] = a.PTime
			}
		}
	}

	// ---- 第二遍：下线 ----
	seen := map[string]DelistEntry{}
	for _, a := range anns {
		if a.PTime > 0 && a.PTime < cut {
			continue
		}
		low := strings.ToLower(a.Title)
		skip := false
		for _, w := range skipWords {
			if strings.Contains(low, w) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		// 只看下线类公告；annType 为空时退化成「标题里含 delist」
		isDelist := a.AnnType == okx.AnnTypeDelistings ||
			(a.AnnType == "" && strings.Contains(low, "delist"))
		if !isDelist {
			continue
		}
		for _, sym := range matchSymbols(a.Title, universe) {
			// 更晚的「推迟 / 取消」公告 → 这条下线作废
			if cAt, ok := cancelled[sym]; ok && cAt > a.PTime {
				continue
			}
			if old, ok := seen[sym]; !ok || a.PTime > old.PTime {
				seen[sym] = DelistEntry{Symbol: sym, Title: a.Title, PTime: a.PTime}
			}
		}
	}
	out := make([]DelistEntry, 0, len(seen))
	for _, v := range seen {
		out = append(out, v)
	}
	return out
}

// matchSymbols 在标题里找出现过的币种（词边界匹配，避免 ONE 命中 ONEOK 之类）
func matchSymbols(title string, universe map[string]bool) []string {
	up := strings.ToUpper(title)
	var out []string
	for sym := range universe {
		if len(sym) < 2 {
			continue
		}
		if containsSymbol(up, sym) {
			out = append(out, sym)
		}
	}
	return out
}

func containsSymbol(up, sym string) bool {
	idx := 0
	for {
		i := strings.Index(up[idx:], sym)
		if i < 0 {
			return false
		}
		abs := idx + i
		var before byte = ' '
		if abs > 0 {
			before = up[abs-1]
		}
		var after byte = ' '
		if abs+len(sym) < len(up) {
			after = up[abs+len(sym)]
		}
		// 前后都不是字母数字 → 纯词
		if !isAlnum(before) && !isAlnum(after) {
			return true
		}
		// "ONEUSDT" / "ONEUSD" 这种也行
		rest := up[abs+len(sym):]
		if !isAlnum(before) && (strings.HasPrefix(rest, "USDT") || strings.HasPrefix(rest, "USD")) {
			return true
		}
		idx = abs + 1
		if idx >= len(up) {
			return false
		}
	}
}

func isAlnum(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

// ---------------------------------------------------------------------------
// 带缓存的名单获取
// ---------------------------------------------------------------------------

// AnnouncementFetcher 抓公告的函数签名（DataFeed / OKXClient 都能满足）
type AnnouncementFetcher func(annType string, pages int) ([]Announcement, error)

// DBMeta meta 键值接口（repo.DB 天然满足；传 nil 表示不落库）
type DBMeta interface {
	GetMeta(k string) (string, bool, error)
	SetMeta(k, v string) error
}

var (
	delistMu     sync.Mutex
	delistCache  []DelistEntry
	delistLoaded bool

	// announceCacheDB 全局缓存库（由 cmd 启动时注入一次）
	announceCacheDB DBMeta
)

// DelistCacheKey meta 表里的缓存键
const DelistCacheKey = "delist_blacklist"

// SetAnnounceCacheDB 注入落库缓存（可传 nil）
func SetAnnounceCacheDB(db DBMeta) { announceCacheDB = db }

// LoadDelistList 取下线名单。优先走内存缓存 → MySQL 缓存（24 小时）→ 打公告接口。
func LoadDelistList(fetch AnnouncementFetcher, cache DBMeta, universe map[string]bool, withinDays int) []DelistEntry {
	delistMu.Lock()
	defer delistMu.Unlock()

	if delistLoaded {
		return delistCache
	}
	if cache == nil {
		cache = announceCacheDB
	}

	// MySQL meta 缓存
	if cache != nil {
		if raw, ok, err := cache.GetMeta(DelistCacheKey); err == nil && ok {
			var wrap struct {
				FetchedAt int64         `json:"fetchedAt"`
				Items     []DelistEntry `json:"items"`
			}
			if json.Unmarshal([]byte(raw), &wrap) == nil &&
				time.Since(time.UnixMilli(wrap.FetchedAt)) < 24*time.Hour {
				delistCache, delistLoaded = wrap.Items, true
				return delistCache
			}
		}
	}

	// 打公告接口
	if fetch == nil {
		delistCache, delistLoaded = []DelistEntry{}, true
		return delistCache
	}
	anns, err := fetch(okx.AnnTypeDelistings, 4)
	if err != nil {
		// 拿不到公告不能因噎废食：返回空名单（其余过滤照常生效）
		delistCache, delistLoaded = []DelistEntry{}, true
		return delistCache
	}
	items := BuildDelistList(anns, universe, withinDays)
	delistCache, delistLoaded = items, true

	if cache != nil {
		if b, err := json.Marshal(map[string]any{
			"fetchedAt": time.Now().UnixMilli(), "items": items,
		}); err == nil {
			_ = cache.SetMeta(DelistCacheKey, string(b))
		}
	}
	return items
}

// ResetDelistCache 手动清缓存（配置改动 / 需要立刻刷新时用）
func ResetDelistCache() {
	delistMu.Lock()
	delistCache, delistLoaded = nil, false
	delistMu.Unlock()
}

// DelistSymbolSet 把名单转成 set，方便查
func DelistSymbolSet(items []DelistEntry) map[string]DelistEntry {
	m := make(map[string]DelistEntry, len(items))
	for _, it := range items {
		m[it.Symbol] = it
	}
	return m
}

// ---------------------------------------------------------------------------
// 引擎侧（OKXClient）的公告入口
// ---------------------------------------------------------------------------

// FetchAnnouncements 用引擎自带的 HTTP 客户端抓公告（走 /api/v5/support/announcements）
func (c *OKXClient) FetchAnnouncements(annType string, pages int) ([]Announcement, error) {
	if pages <= 0 {
		pages = 3
	}
	out := make([]Announcement, 0, pages*20)
	for p := 1; p <= pages; p++ {
		path := "/api/v5/support/announcements?page=" + itoa(p)
		if annType != "" {
			path += "&annType=" + annType
		}
		raw, err := c.Get(path, false)
		if err != nil {
			if p == 1 {
				return nil, err
			}
			break
		}
		var resp struct {
			Code string `json:"code"`
			Msg  string `json:"msg"`
			Data []struct {
				Details []struct {
					AnnType string `json:"annType"`
					Title   string `json:"title"`
					PTime   string `json:"pTime"`
					URL     string `json:"url"`
				} `json:"details"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, err
		}
		for _, blk := range resp.Data {
			for _, d := range blk.Details {
				out = append(out, Announcement{
					AnnType: d.AnnType,
					Title:   d.Title,
					PTime:   toI64(d.PTime),
					URL:     d.URL,
				})
			}
		}
		time.Sleep(120 * time.Millisecond)
	}
	return out, nil
}

// AnnouncementFetcher 把方法转成函数（给 LoadDelistList 用）
func (c *OKXClient) AnnouncementFetcher() AnnouncementFetcher {
	return func(annType string, pages int) ([]Announcement, error) {
		return c.FetchAnnouncements(annType, pages)
	}
}

// AnnouncementFetcher DataFeed 侧同样支持
func (f *DataFeed) AnnouncementFetcher() AnnouncementFetcher {
	return func(annType string, pages int) ([]Announcement, error) {
		return f.FetchAnnouncements(annType, pages)
	}
}

// SymbolOf 取合约基础币种（BTC-USDT-SWAP → BTC）
func SymbolOf(instID string) string {
	if i := indexByte(instID, '-'); i > 0 {
		return instID[:i]
	}
	return instID
}
