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
//   GET  /api/takerflow     taker 买卖流向面板（时间/总买卖比/ETH下一根涨跌幅/最高合约）
//   GET  /api/takermacd     taker 买卖比的 MACD(12,26,60) 副图序列（仅 5m）
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

	"finally-main/internal/rbac"
	"finally-main/internal/repo"
	"finally-main/internal/service"
)

// Server 网页服务
type Server struct {
	db       *repo.DB
	feed     *service.DataFeed
	bf       *service.BackfillManager
	strategy *service.StrategyStore
	assets   fs.FS
	root     string
	startAt  time.Time
	logf     func(string, ...any)
	reqCount int64

	// ★ 2026-10-02 八期：管理员鉴权与配置写回
	//   auth   验证码 + 会话（internal/rbac）
	//   writer 配置原子写回（internal/service.StrategyWriter）
	//   onConfigSaved 保存成功后的回调（cmd 里挂「重算准入过滤」）
	auth          *rbac.Manager
	writer        *service.StrategyWriter
	onConfigSaved func()

	// freshAt 记录每个 (合约,周期) 上次「按需拉最新 K 线」的时间，
	// 用来给 /api/mark 的实时刷新做节流，防止前端高频轮询打爆 OKX 限频。
	freshMu sync.Mutex
	freshAt map[string]time.Time

	// tk taker 买卖流向面板的结果缓存（20 秒 TTL，见 api_takerflow.go）
	tk takerflowCache
}

// cfg 取当前生效的策略配置。
//
// ★ 热插拔的关键：这里**不缓存**，每次问 store 要最新的。
//   所以改 configs/okx_strategy.json 后，网页下次轮询就是新口径，不用重启服务。
func (s *Server) cfg() *service.StrategyConfig {
	if s.strategy == nil {
		return &service.StrategyConfig{}
	}
	return s.strategy.Get()
}

// NewServer 组装服务
func NewServer(db *repo.DB, feed *service.DataFeed, bf *service.BackfillManager, strategy *service.StrategyStore, assetsDir, root string, logf func(string, ...any)) *Server {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	// ★ 八期：鉴权与配置写回在这里就地建好（都不依赖外部资源），
	//   cmd 只需在需要时补一个 onConfigSaved 回调与 SMTP mailer。
	//   mailer 留空时「发验证码」接口会明确报「邮件服务未配置」，
	//   而不是静默失败 —— 用户能立刻看出少了什么。
	auth := rbac.NewManager(nil, logf)
	writer := service.NewStrategyWriter(service.StrategyConfigPath(root), logf)
	return &Server{
		db: db, feed: feed, bf: bf, strategy: strategy,
		assets:  os.DirFS(assetsDir),
		root:    root,
		startAt: time.Now(),
		logf:    logf,
		auth:    auth,
		writer:  writer,
		freshAt: map[string]time.Time{},
	}
}

// SetMailer 装上邮件发送器（cmd 里读 SMTP 配置后调）。
// 分开是为了让 NewServer 保持「无外部依赖」，测试里可以直接构造 Server。
func (s *Server) SetMailer(m rbac.Mailer) {
	s.auth = rbac.NewManager(m, s.logf)
}

// SetConfigSavedHook 挂「配置保存成功后」的回调（cmd 用来重算合约准入）。
func (s *Server) SetConfigSavedHook(fn func()) {
	s.onConfigSaved = fn
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
	mux.HandleFunc("/api/takerflow", s.wrap(s.handleTakerFlow))
	mux.HandleFunc("/api/takermacd", s.wrap(s.handleTakerMacd))
	mux.HandleFunc("/api/tables", s.wrap(s.handleTables))
	mux.HandleFunc("/api/health", s.wrap(s.handleHealth))
	mux.HandleFunc("/api/perf", s.wrap(s.handlePerf))

	// ★ 管理员接口（2026-10-02 八期）。
	//
	//   这套接口能改真实下单口径，所以鉴权拦在 s.wrap 里（见下面的统一判断），
	//   只放行两个「登录流程本身」的入口 —— 不登录就没法登录。
	//
	//   ⚠ 新增 /api/admin/xxx 时**不用**在这里写鉴权代码：
	//     一律由 wrap 的前缀判断统一兜住，漏不了的写法只有一种。
	mux.HandleFunc("/api/admin/send_code", s.wrap(s.handleAdminSendCode))
	mux.HandleFunc("/api/admin/login", s.wrap(s.handleAdminLogin))
	mux.HandleFunc("/api/admin/logout", s.wrap(s.handleAdminLogout))
	mux.HandleFunc("/api/admin/session", s.wrap(s.handleAdminSession))
	mux.HandleFunc("/api/admin/sessions", s.wrap(s.handleAdminSessionsAll))
	mux.HandleFunc("/api/admin/config", s.wrap(s.handleAdminSaveOrGetConfig))

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
		// ★★ 管理员接口统一鉴权（2026-10-02 八期）★★
		//
		//   ★ 为什么拦在 wrap 里而不是每个 handler 自己判 ★
		//     本项目最怕的就是「新加的路径忘了鉴权」—— 那不会报错，
		//     只会安静地把交易口径暴露出去。所以做成**前缀白名单**：
		//     凡是 /api/admin/ 下的路径都必须过会话校验，
		//     只有下面这两个「登录流程本身」的入口例外。
		//     以后新增 /api/admin/xxx 自动被兜住，不需要记得加代码。
		if strings.HasPrefix(r.URL.Path, "/api/admin/") && !adminOpenPath(r.URL.Path) {
			if s.auth == nil || s.auth.Verify(tokenFrom(r)) == nil {
				s.logf("✗ 未授权访问 %s（来源 %s）", r.URL.Path, clientIP(r))
				s.writeJSON(w, http.StatusUnauthorized, map[string]any{
					"ok":        false,
					"error":     "未登录或会话已过期，请重新验证邮箱",
					"needLogin": true,
				})
				return
			}
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

// adminOpenPath 无需登录即可访问的管理员路径。
//
// ★ 只有「登录流程本身」可以在这里：发验证码、登录、登出、探会话。
//   探会话（session）必须开放 —— 前端就是靠它问「我还需要登录吗」，
//   它本身只回一个 loggedIn 布尔，不泄露任何配置。
//   除此之外**不要往这里加东西**。
func adminOpenPath(p string) bool {
	switch p {
	case "/api/admin/send_code", "/api/admin/login",
		"/api/admin/logout", "/api/admin/session":
		return true
	}
	return false
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
	// ★ 只读板块（NQ）的数据来自外部源，不来自 OKX ★
	// 拿它去问 OKX 只能拿到错误，白白消耗一次请求还把错误日志刷满。
	// 它的新鲜度由 service.StartNQSync 自己负责（按缺口补 + 限流节流）。
	if service.IsReadonlyInst(inst) {
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
