package handler

// server.go —— 网页客户端后端（AJAX + TradingView 数据源）
//
// 接口一览（全部返回 JSON）：
//   GET  /api/state        总览：统计 + 策略参数 + 已支持周期 + 数据库路径
//   GET  /api/instruments  合约列表（下拉框用，含名称/成交额）
//   GET  /api/tickers      实时行情（价格、涨跌幅、名称）
//   GET  /api/kline         K 线（?inst=&bar=&days=&limit=）
//   GET  /api/mark          K 线 + 均线/布林（一次性给图表用）
//   GET  /api/positions     当前持仓（含实时盈亏）
//   GET  /api/history       历史仓位
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
	}
}

// Handler 返回带日志 + 计数的 mux
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// 静态页面
	mux.Handle("/", http.FileServer(http.FS(s.assets)))
	mux.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(s.assets))))

	// JSON 接口
	mux.HandleFunc("/api/state", s.wrap(s.handleState))
	mux.HandleFunc("/api/instruments", s.wrap(s.handleInstruments))
	mux.HandleFunc("/api/tickers", s.wrap(s.handleTickers))
	mux.HandleFunc("/api/kline", s.wrap(s.handleKline))
	mux.HandleFunc("/api/mark", s.wrap(s.handleMark))
	mux.HandleFunc("/api/positions", s.wrap(s.handlePositions))
	mux.HandleFunc("/api/history", s.wrap(s.handleHistory))
	mux.HandleFunc("/api/signals", s.wrap(s.handleSignals))
	mux.HandleFunc("/api/pnl", s.wrap(s.handlePnl))
	mux.HandleFunc("/api/backfill", s.wrap(s.handleBackfill))
	mux.HandleFunc("/api/tables", s.wrap(s.handleTables))
	mux.HandleFunc("/api/health", s.wrap(s.handleHealth))

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
