package handler

// server.go —— 网页客户端后端（AJAX + TradingView 数据源）
//
// 接口一览（全部返回 JSON）：
//   GET  /api/state        总览：统计 + 策略参数 + 已支持周期 + 数据库路径
//   GET  /api/account      账户快照：权益/可用/本金/浮盈/总盈亏 + 实时引擎状态（轻量，2秒轮询）
//   GET  /api/instruments  合约列表（下拉框用，含名称/成交额）
//   GET  /api/tickers      实时行情（价格、涨跌幅、名称）
//   GET  /api/kline         K 线（?inst=&bar=&days=&limit=）
//   GET  /api/mark          K 线 + 均线/布林（一次性给图表用）
//   GET  /api/positions     当前持仓（含实时盈亏）
//   GET  /api/history       历史仓位（?days=30 默认最近 30 天；持仓中的不受天数限制）
//   GET  /api/events        交易记录详情：逐笔开仓/加仓/平仓流水（?days=30）
//   GET  /api/signals       信号流水
//   GET  /api/pnl           权益/浮盈曲线
//   GET  /api/backfill      回补进度 + 覆盖情况
//   POST /api/backfill      手动触发回补 {inst, bar}
//   GET  /api/tables        数据库表与行数（自检）
//   GET  /api/health        健康检查

import (
	"encoding/json"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"finally-main/internal/repo"
	"finally-main/internal/service"
)

// Server 网页服务
type Server struct {
	db       *repo.DB
	feed     *service.DataFeed
	bf       *service.BackfillManager
	strategy *service.StrategyConfig
	assets   fs.FS
	root     string
	startAt  time.Time
	logf     func(string, ...any)
	reqCount int64

	// freshAt 记录每个 (合约,周期) 上次「按需拉最新 K 线」的时间，
	// 用来给 /api/mark 的实时刷新做节流，防止前端高频轮询打爆 OKX 限频。
	freshMu sync.Mutex
	freshAt map[string]time.Time
}

// NewServer 组装服务
func NewServer(db *repo.DB, feed *service.DataFeed, bf *service.BackfillManager, strategy *service.StrategyConfig, assetsDir, root string, logf func(string, ...any)) *Server {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Server{
		db: db, feed: feed, bf: bf, strategy: strategy,
		assets:  os.DirFS(assetsDir),
		root:    root,
		startAt: time.Now(),
		logf:    logf,
		freshAt: map[string]time.Time{},
	}
}

// Handler 返回带日志 + 计数的 mux
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// 静态页面。
	// 必须 no-cache：浏览器对只有 Last-Modified 的资源会做「启发式缓存」，
	// index.html/app.js 改版后用户那边可能几小时还跑旧 JS（顶栏字段全显示 -- 就
	// 是旧 JS 写不到新 id 上）。本地内网服务，协商缓存的开销可以忽略。
	noCache := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-cache, must-revalidate")
			next.ServeHTTP(w, r)
		})
	}
	mux.Handle("/", noCache(http.FileServer(http.FS(s.assets))))
	mux.Handle("/assets/", noCache(http.StripPrefix("/assets/", http.FileServer(http.FS(s.assets)))))

	// JSON 接口
	mux.HandleFunc("/api/state", s.wrap(s.handleState))
	mux.HandleFunc("/api/account", s.wrap(s.handleAccount))
	mux.HandleFunc("/api/instruments", s.wrap(s.handleInstruments))
	mux.HandleFunc("/api/tickers", s.wrap(s.handleTickers))
	mux.HandleFunc("/api/kline", s.wrap(s.handleKline))
	mux.HandleFunc("/api/mark", s.wrap(s.handleMark))
	mux.HandleFunc("/api/positions", s.wrap(s.handlePositions))
	mux.HandleFunc("/api/history", s.wrap(s.handleHistory))
	mux.HandleFunc("/api/events", s.wrap(s.handleEvents))
	mux.HandleFunc("/api/signals", s.wrap(s.handleSignals))
	mux.HandleFunc("/api/pnl", s.wrap(s.handlePnl))
	mux.HandleFunc("/api/backfill", s.wrap(s.handleBackfill))
	mux.HandleFunc("/api/tables", s.wrap(s.handleTables))
	mux.HandleFunc("/api/health", s.wrap(s.handleHealth))
	mux.HandleFunc("/api/perf", s.wrap(s.handlePerf))

	return mux
}

type apiFunc func(w http.ResponseWriter, r *http.Request) (any, error)

func (s *Server) wrap(f apiFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/kline" {
			s.logf("→ %s %s", r.Method, r.URL.RequestURI())
		}
		out, err := f(w, r)
		if err != nil {
			s.logf("✗ %s %s：%v", r.Method, r.URL.Path, err)
			s.writeJSON(w, http.StatusInternalServerError, map[string]any{
				"ok": false, "error": err.Error(),
			})
			return
		}
		if out == nil {
			return // 处理器已经自己写过响应
		}
		s.writeJSON(w, http.StatusOK, out)
	}
}

func (s *Server) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func atoiDefault(s string, def int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}

func smaSeries(x []float64, period int) []float64 {
	n := len(x)
	out := make([]float64, n)
	for i := range out {
		out[i] = math.NaN()
	}
	if period <= 0 || n < period {
		return out
	}
	sum := 0.0
	for i := 0; i < n; i++ {
		sum += x[i]
		if i >= period {
			sum -= x[i-period]
		}
		if i >= period-1 {
			out[i] = sum / float64(period)
		}
	}
	return out
}

func bollSeries(x []float64, period int, mult float64) (up, mid, lo []float64) {
	n := len(x)
	up, mid, lo = make([]float64, n), make([]float64, n), make([]float64, n)
	for i := range up {
		up[i], mid[i], lo[i] = math.NaN(), math.NaN(), math.NaN()
	}
	if period <= 0 || n < period {
		return
	}
	for i := period - 1; i < n; i++ {
		s := 0.0
		for k := i - period + 1; k <= i; k++ {
			s += x[k]
		}
		m := s / float64(period)
		v := 0.0
		for k := i - period + 1; k <= i; k++ {
			d := x[k] - m
			v += d * d
		}
		v /= float64(period)
		sd := math.Sqrt(v)
		mid[i] = m
		up[i] = m + mult*sd
		lo[i] = m - mult*sd
	}
	return
}

// AssetsDir 静态资源目录（相对项目根）
func AssetsDir(root string) string { return filepath.Join(root, "web", "assets") }

// ---------------------------------------------------------------------------
// 实时性：按需刷新「当前这一根」
// ---------------------------------------------------------------------------

// ensureFresh 保证 (合约,周期) 的最新一根 K 线是新的。
//
// 图表的实时性就靠这里：前端几秒轮询一次 /api/mark，进来先看一眼库里
// 最新一根有多新，不够就从 OKX 拉最近 3 根补上（含正在走的那根）。
//
// 节流：同一个 (合约,周期) 最快每 throttle 才真的发一次请求，
// 周期越短允许越勤。没有这个节流，前端高频轮询会把 OKX 限频吃光，
// 反把回补任务饿死。
func (s *Server) ensureFresh(inst, bar string) {
	if s.feed == nil {
		return
	}
	d := service.BarDuration(bar)
	if d <= 0 {
		return
	}
	throttle := d / 8
	if throttle < 2*time.Second {
		throttle = 2 * time.Second
	}
	if throttle > 15*time.Second {
		throttle = 15 * time.Second
	}

	key := inst + "|" + bar
	s.freshMu.Lock()
	if t, ok := s.freshAt[key]; ok && time.Since(t) < throttle {
		s.freshMu.Unlock()
		return
	}
	s.freshAt[key] = time.Now()
	s.freshMu.Unlock()

	ks, err := s.feed.FetchCandles(inst, bar, 3)
	if err != nil || len(ks) == 0 {
		return
	}
	_, _ = s.db.UpsertKlines(ks)
}
