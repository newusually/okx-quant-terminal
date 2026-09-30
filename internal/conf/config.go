package conf

// config.go —— 策略配置加载（热加载，改文件即生效，不用重启）
//
// 只读 configs/okx_strategy.json，不碰项目里任何其它文件。
// 支持 // 与 /* */ 注释（读之前先剥掉），也支持 "_xxx" 形式的注释键（JSON 解析时自动忽略）。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"finally-main/internal/logx"
)

// ---------------------------------------------------------------------------
// 配置结构
// ---------------------------------------------------------------------------

type EntryCfg struct {
	TdMode                 string  `json:"td_mode"`
	PosSide                string  `json:"pos_side"`
	OrdType                string  `json:"ord_type"`
	MarginUSDT             float64 `json:"margin_usdt"`
	Leverage               int     `json:"leverage"`
	MaxConcurrentPositions int     `json:"max_concurrent_positions"`
	CooldownBars           int     `json:"cooldown_bars"`
	DailyMaxEntries        int     `json:"daily_max_entries"`

	// MarginPolicy 决定「算出来买不起 1 张」怎么办：
	//   "fixed"   —— 严格用 MarginUSDT，买不起就跳过（默认，忠于原口径）
	//   "min_one" —— 自动把保证金放大到刚好能买 1 张，但不超过 MaxMarginUSDT
	MarginPolicy  string  `json:"margin_policy"`
	MaxMarginUSDT float64 `json:"max_margin_usdt"`
}

type ExitCfg struct {
	TakeProfitPct float64 `json:"take_profit_pct"`
	BollUpperExit bool    `json:"boll_upper_exit"`
	MaxHoldBars   int     `json:"max_hold_bars"`
	StopLossPct   float64 `json:"stop_loss_pct"`
}

// AddonCfg 加仓（浮亏补仓 / 摊薄均价）。
//
// 现行口径（用户指定）：
//
//	加仓额 = 原持仓保证金 × Ratio（Ratio 默认 1/3）
//	触发  = 15m 周期上「先跌 DropPct%（默认 0.5%）」然后「K 线重新转涨」
//
// 也就是：开仓后价格先跌够 0.5%，等 15m 收出一根阳线且高于前一根收盘价，
// 才补 1/3 的仓位进去。不抄底、不追跌，只做反转确认后的一次加仓。
type AddonCfg struct {
	Enabled bool `json:"enabled"`

	// Ratio 加仓额 = 原持仓保证金 × Ratio。默认 1/3。
	Ratio float64 `json:"ratio"`

	// DropPct 先要跌这么多（%）才算「跌过」。默认 0.5。
	DropPct float64 `json:"drop_pct"`

	// RiseBar 用哪个周期判断「转涨」。默认 15m。
	RiseBar string `json:"rise_bar"`

	// LookbackBars 回看多少根 RiseBar 找「先跌」的低点。默认 24（15m × 24 = 6 小时）。
	LookbackBars int `json:"lookback_bars"`

	// MaxTimes 每个仓位最多加几次。默认 2。
	MaxTimes int `json:"max_times"`

	// MinGapBars 两次加仓之间至少隔多少根 RiseBar。默认 1。
	MinGapBars int `json:"min_gap_bars"`

	// MarginUSDT 固定加仓额（>0 时优先于 Ratio，一般留 0）
	MarginUSDT float64 `json:"margin_usdt"`

	// OnlyWhenPriceUp 只在上行时加（保持 true）
	OnlyWhenPriceUp bool `json:"only_when_price_up"`

	// FibRatio 旧的斐波那契口径，已废弃，仅保留读兼容
	FibRatio float64 `json:"fib_ratio,omitempty"`
}

type RiskCfg struct {
	AccountEquityStop    float64 `json:"account_equity_stop"`
	DailyLossStopPct     float64 `json:"daily_loss_stop_pct"`
	MaxTotalMarginPct    float64 `json:"max_total_margin_pct"`
	MinAvailableUSDT     float64 `json:"min_available_usdt"`
	ConsecutiveLossPause int     `json:"consecutive_loss_pause"`
	PauseOnAPIError      int     `json:"pause_on_api_error"`
}

type AICfg struct {
	Enabled        bool   `json:"enabled"`
	Provider       string `json:"provider"`
	BaseURL        string `json:"base_url"`
	APIKey         string `json:"api_key"`
	Model          string `json:"model"`
	TimeoutSec     int    `json:"timeout_sec"`
	MaxCallsPerDay int    `json:"max_calls_per_day"`
	OnlyOnOrder    bool   `json:"only_on_order"`
}

type OKXCfg struct {
	BaseURL      string   `json:"base_url"`
	FallbackURLs []string `json:"fallback_urls"`
	APIKey       string   `json:"api_key"`
	SecretKey    string   `json:"secret_key"`
	Passphrase   string   `json:"passphrase"`
	Simulated    bool     `json:"simulated"`
	Proxy        string   `json:"proxy"`
}

// StoreCfg 持久化配置。
//
// 数据库已从 SQLite 切换为 MySQL（400+ 合约并发写入，SQLite 单写者扛不住）。
// 旧字段 DBPath / Python 仅作兼容保留，新代码不再使用。
type StoreCfg struct {
	Enabled bool   `json:"enabled"`
	Driver  string `json:"driver"` // 固定 mysql

	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	Database string `json:"database"`
	DSN      string `json:"dsn"` // 若填写则优先于上面 5 个字段

	MaxOpenConns int `json:"max_open_conns"`
	MaxIdleConns int `json:"max_idle_conns"`
	BatchSize    int `json:"batch_size"` // 批量写入分片大小

	KeepKlineBars int    `json:"keep_kline_bars"`
	LogDir        string `json:"log_dir"`
	LogMaxMB      int    `json:"log_max_mb"`
	LogKeep       int    `json:"log_keep"`

	// 兼容旧配置（不再使用）
	DBPath string `json:"db_path,omitempty"`
	Python string `json:"python,omitempty"`
}

type Config struct {
	Enabled           bool           `json:"enabled"`
	DryRun            bool           `json:"dry_run"`
	OrderVia          string         `json:"order_via"`
	Bar               string         `json:"bar"`
	BarsEnabled       []string       `json:"bars_enabled"`
	MinCandles        int            `json:"min_candles"`
	TopNByVolume      int            `json:"top_n_by_volume"`
	MinQuoteVolume24h float64        `json:"min_quote_volume_24h"`
	ExcludeInst       []string       `json:"exclude_inst"`
	Workers           int            `json:"workers"`
	CandleLimit       int            `json:"candle_limit"`
	HistoryPages      int            `json:"history_pages"`
	ScoreThreshold    int            `json:"score_threshold"`
	ScoreThresholdMap map[string]int `json:"score_threshold_map"`
	SignalTimeoutSec  int            `json:"signal_timeout_sec"`
	RequestTimeoutSec int            `json:"request_timeout_sec"`

	// ---- 合约准入（「哪些能买」）----
	// ExcludeStockETF 不买美股 / ETF / 商品，只做加密（OKX instCategory=1）
	ExcludeStockETF bool `json:"exclude_stock_etf"`
	// ExcludeNewListingDays 上市不足这么多天的不买（0 = 不排除）
	ExcludeNewListingDays int `json:"exclude_new_listing_days"`
	// ExcludeDelisting 排除 OKX 公告里说要下线的
	ExcludeDelisting bool `json:"exclude_delisting"`
	// MaxOrderMarginUSDT 单笔保证金硬上限（防止算错了买多）
	MaxOrderMarginUSDT float64 `json:"max_order_margin_usdt"`

	Entry *EntryCfg `json:"entry"`
	Exit  *ExitCfg  `json:"exit"`
	Addon *AddonCfg `json:"addon"`
	Risk  *RiskCfg  `json:"risk"`
	AI    *AICfg    `json:"ai"`
	OKX   *OKXCfg   `json:"okx"`
	Store *StoreCfg `json:"store"`

	// 运行时字段（不来自 JSON）
	path string // 配置文件绝对/相对路径
	dir  string // 配置文件所在目录（db.py 就从这里找）
	root string // 项目根目录（有 go.mod 的那层），相对路径都按它解析
}

// Resolve 把配置里的相对路径解析成绝对路径。
// 这样无论从哪个目录启动（项目根 / internal/service / 计划任务的任意工作目录），
// 数据库和日志都落在同一个地方。
func (c *Config) Resolve(p string) string {
	if p == "" {
		return p
	}
	if filepath.IsAbs(p) {
		return p
	}
	root := c.root
	if root == "" {
		root = "."
	}
	return filepath.Join(root, p)
}

// Dir 配置文件所在目录（外部包要用时走这个方法，别去碰私有字段）
func (c *Config) Dir() string { return c.dir }

// Root 项目根目录（有 go.mod 的那层）
func (c *Config) Root() string { return c.root }

// ---------------------------------------------------------------------------
// logx.Sink 实现 —— 让 logx 拿到「日志往哪写」而不用反过来依赖 conf
// ---------------------------------------------------------------------------

// LogEnabled 对应 store.enabled
func (c *Config) LogEnabled() bool { return c.Store != nil && c.Store.Enabled }

// LogDirResolved 日志目录（已解析成绝对路径）
func (c *Config) LogDirResolved() string {
	if c.Store == nil {
		return ""
	}
	return c.Resolve(c.Store.LogDir)
}

// LogMaxMBytes 单文件体积上限（MB）
func (c *Config) LogMaxMBytes() int {
	if c.Store == nil {
		return 0
	}
	return c.Store.LogMaxMB
}

// LogKeepFiles 滚动保留份数
func (c *Config) LogKeepFiles() int {
	if c.Store == nil {
		return 0
	}
	return c.Store.LogKeep
}

// projectRoot 从当前目录往上找 go.mod；找不到就退回当前目录
func projectRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	start := dir
	for i := 0; i < 5; i++ {
		if _, e := os.Stat(filepath.Join(dir, "go.mod")); e == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return start
}

// ---------------------------------------------------------------------------
// 默认值（文件缺失或字段缺失时兜底）
// ---------------------------------------------------------------------------

// DefaultConfig 返回一份纯内置默认配置（不读磁盘、不受策略文件影响）。
// 单测和「配置缺失时手工构造」都走这个入口。
func DefaultConfig() *Config { return defaultConfig() }

func defaultConfig() *Config {
	return &Config{
		Enabled:           true,
		DryRun:            true,
		OrderVia:          "go",
		Bar:               "15m",
		BarsEnabled:       []string{"15m"},
		MinCandles:        400,
		TopNByVolume:      80,
		MinQuoteVolume24h: 1000000,
		ExcludeInst:       []string{},
		// 合约准入：不买美股/ETF/商品；不买 30 天内新上线；不买要下线的
		ExcludeStockETF:       true,
		ExcludeNewListingDays: 30,
		ExcludeDelisting:      true,
		MaxOrderMarginUSDT:    0.5,
		Workers:               4,
		CandleLimit:           300,
		HistoryPages:          2,
		ScoreThreshold:        6,
		ScoreThresholdMap:     map[string]int{"BTC-USDT-SWAP": 7, "ETH-USDT-SWAP": 7},
		SignalTimeoutSec:      900,
		RequestTimeoutSec:     20,
		Entry: &EntryCfg{
			TdMode: "isolated", PosSide: "net", OrdType: "market",
			MarginUSDT: 0.1, Leverage: 20,
			MaxConcurrentPositions: 8, CooldownBars: 6, DailyMaxEntries: 30,
			// 目标每笔 0.1 U；合约准入要求「最小一手保证金 ≤ 0.5 U」。
			// 0.1U 买不起 1 张的合约会放大到刚好买 1 张来下单，绝不超过 MaxMarginUSDT。
			MarginPolicy: "min_one", MaxMarginUSDT: 0.5,
		},
		Exit:  &ExitCfg{TakeProfitPct: 2.0, BollUpperExit: true, MaxHoldBars: 0, StopLossPct: 0},
		// 加仓：15m 先跌 0.5% 再转涨 → 补原仓位的 1/3（不超过 max_margin_usdt）
		Addon: &AddonCfg{
			Enabled: true, Ratio: 1.0 / 3.0, DropPct: 0.5, RiseBar: "15m",
			LookbackBars: 24, MaxTimes: 2, MinGapBars: 1,
			MarginUSDT: 0, OnlyWhenPriceUp: true,
		},
		Risk: &RiskCfg{
			// 小资金口径（账户就几毛到几 U）：
			//   百分比类的保护要按笔算，不能用「5U 可用余额」「30% 总保证金」这种大账户默认值，
			//   否则 0.1U 的账户一笔都开不出来。
			AccountEquityStop: 0, DailyLossStopPct: 50, MaxTotalMarginPct: 100,
			MinAvailableUSDT: 0.1, ConsecutiveLossPause: 5, PauseOnAPIError: 10,
		},
		AI: &AICfg{
			Enabled: true, Provider: "openai_compatible",
			BaseURL: "https://api.deepseek.com/v1", APIKey: "", Model: "deepseek-chat",
			TimeoutSec: 30, MaxCallsPerDay: 50, OnlyOnOrder: true,
		},
		OKX: &OKXCfg{
			BaseURL:      "https://www.okx.com",
			FallbackURLs: []string{"https://aws.okx.com", "https://okx.com"},
			Simulated:    true,
		},
		Store: &StoreCfg{
			Enabled: true, Driver: "mysql",
			Host: "127.0.0.1", Port: 3306,
			User: "okx", Password: "OkxQuant2026", Database: "okx",
			MaxOpenConns: 64, MaxIdleConns: 32, BatchSize: 500,
			KeepKlineBars: 60000, LogDir: "logs", LogMaxMB: 20, LogKeep: 5,
		},
		path: "configs/okx_strategy.json",
		dir:  "configs",
	}
}

// ---------------------------------------------------------------------------
// 加载
// ---------------------------------------------------------------------------

var (
	cfgMu      sync.Mutex
	cfgCache   *Config
	cfgModTime time.Time
	cfgLoaded  bool
)

// LoadConfig 返回当前配置。文件被改动过会自动重新读。
// 永远不会返回 nil。
func LoadConfig() *Config {
	cfgMu.Lock()
	defer cfgMu.Unlock()

	p := resolveConfigPath()

	if cfgLoaded && cfgCache != nil && cfgCache.path == p {
		if fi, err := os.Stat(p); err != nil || !fi.ModTime().After(cfgModTime) {
			return cfgCache
		}
		// 有更新 → 往下走重新加载
	}

	cfg, mt, err := readConfig(p)
	if err != nil {
		if cfgCache != nil {
			// 读失败（比如改到一半保存了）→ 继续用上一份，别让策略中断
			logx.Logf("ERROR", "配置读取失败，继续沿用上一份配置：%v", err)
			cfgCache.path = p
			return cfgCache
		}
		logx.Logf("ERROR", "配置读取失败，改用内置默认值：%v", err)
		def := defaultConfig()
		def.path = p
		def.dir = filepath.Dir(p)
		def.root = projectRoot()
		logx.SetSink(def)
		cfgCache, cfgModTime, cfgLoaded = def, time.Time{}, true
		return def
	}

	cfg.path = p
	cfg.dir = filepath.Dir(p)
	cfg.root = projectRoot()
	logx.SetSink(cfg) // 必须先装好，下面 logf 才不会重入
	if cfgCache == nil && !cfgLoaded {
		logx.Logf("INFO", "策略配置已加载：%s（dry_run=%v simulated=%v bar=%s）",
			p, cfg.DryRun, cfg.OKX.Simulated, cfg.Bar)
	} else {
		logx.Logf("INFO", "策略配置已热加载：%s", p)
	}
	cfgCache, cfgModTime, cfgLoaded = cfg, mt, true
	return cfg
}

// ConfigPath 暴露当前配置文件路径（给外部日志用）
func ConfigPath() string {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return resolveConfigPath()
}

func resolveConfigPath() string {
	if p := strings.TrimSpace(os.Getenv("OKX_STRATEGY_CONFIG")); p != "" {
		return p
	}
	cands := []string{
		filepath.Join("configs", "okx_strategy.json"),
		"okx_strategy.json",
	}
	if r := projectRoot(); r != "." {
		cands = append(cands,
			filepath.Join(r, "configs", "okx_strategy.json"),
			filepath.Join(r, "okx_strategy.json"))
	}
	for _, c := range cands {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	// 相对可执行文件目录再找一遍（服务方式启动时 cwd 可能不同）
	if exe, err := os.Executable(); err == nil {
		base := filepath.Dir(exe)
		for _, c := range []string{
			filepath.Join(base, "configs", "okx_strategy.json"),
			filepath.Join(base, "okx_strategy.json"),
		} {
			if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
				return c
			}
		}
	}
	return cands[0]
}

func readConfig(path string) (*Config, time.Time, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	clean := StripJSONComments(raw)
	cfg := defaultConfig()
	if err := json.Unmarshal(clean, cfg); err != nil {
		return nil, time.Time{}, fmt.Errorf("JSON 解析失败：%v", err)
	}
	fillDefaults(cfg)
	mt := time.Time{}
	if fi, err := os.Stat(path); err == nil {
		mt = fi.ModTime()
	}
	return cfg, mt, nil
}

// fillDefaults 兼容「文件里只写了几个键」的情况
func fillDefaults(c *Config) {
	d := defaultConfig()
	if c.Bar == "" {
		c.Bar = d.Bar
	}
	if len(c.BarsEnabled) == 0 {
		c.BarsEnabled = d.BarsEnabled
	}
	if c.MinCandles <= 0 {
		c.MinCandles = d.MinCandles
	}
	if c.TopNByVolume <= 0 {
		c.TopNByVolume = d.TopNByVolume
	}
	if c.MinQuoteVolume24h <= 0 {
		c.MinQuoteVolume24h = d.MinQuoteVolume24h
	}
	if c.Workers <= 0 {
		c.Workers = d.Workers
	}
	if c.Workers > 8 {
		c.Workers = 8
	}
	if c.CandleLimit <= 0 || c.CandleLimit > 300 {
		c.CandleLimit = d.CandleLimit
	}
	if c.HistoryPages < 0 {
		c.HistoryPages = 0
	}
	if c.ScoreThreshold <= 0 {
		c.ScoreThreshold = d.ScoreThreshold
	}
	if c.ScoreThreshold > 8 {
		c.ScoreThreshold = 8
	}
	if c.ScoreThresholdMap == nil {
		c.ScoreThresholdMap = map[string]int{}
	}
	if c.OrderVia == "" {
		c.OrderVia = d.OrderVia
	}
	if c.SignalTimeoutSec <= 0 {
		c.SignalTimeoutSec = d.SignalTimeoutSec
	}
	if c.RequestTimeoutSec <= 0 {
		c.RequestTimeoutSec = d.RequestTimeoutSec
	}
	if c.Entry == nil {
		c.Entry = d.Entry
	} else {
		e, de := c.Entry, d.Entry
		if e.TdMode == "" {
			e.TdMode = de.TdMode
		}
		if e.PosSide == "" {
			e.PosSide = de.PosSide
		}
		if e.OrdType == "" {
			e.OrdType = de.OrdType
		}
		if e.MarginUSDT <= 0 {
			e.MarginUSDT = de.MarginUSDT
		}
		if e.Leverage <= 0 {
			e.Leverage = de.Leverage
		}
		if e.MaxConcurrentPositions <= 0 {
			e.MaxConcurrentPositions = de.MaxConcurrentPositions
		}
		if e.CooldownBars < 0 {
			e.CooldownBars = de.CooldownBars
		}
		if e.DailyMaxEntries <= 0 {
			e.DailyMaxEntries = de.DailyMaxEntries
		}
		if e.MarginPolicy == "" {
			e.MarginPolicy = de.MarginPolicy
		}
		if e.MaxMarginUSDT <= 0 {
			e.MaxMarginUSDT = de.MaxMarginUSDT
		}
	}
	if c.Exit == nil {
		c.Exit = d.Exit
	}
	if c.Addon == nil {
		c.Addon = d.Addon
	} else {
		a, da := c.Addon, d.Addon
		if a.Ratio <= 0 {
			a.Ratio = da.Ratio
		}
		if a.DropPct <= 0 {
			a.DropPct = da.DropPct
		}
		if a.RiseBar == "" {
			a.RiseBar = da.RiseBar
		}
		if a.LookbackBars <= 0 {
			a.LookbackBars = da.LookbackBars
		}
		if a.MaxTimes <= 0 {
			a.MaxTimes = da.MaxTimes
		}
		if a.MinGapBars < 0 {
			a.MinGapBars = da.MinGapBars
		}
		if a.MarginUSDT < 0 {
			a.MarginUSDT = 0
		}
	}
	if c.Risk == nil {
		c.Risk = d.Risk
	}
	if c.AI == nil {
		c.AI = d.AI
	} else {
		a, da := c.AI, d.AI
		if a.BaseURL == "" {
			a.BaseURL = da.BaseURL
		}
		if a.Model == "" {
			a.Model = da.Model
		}
		if a.TimeoutSec <= 0 {
			a.TimeoutSec = da.TimeoutSec
		}
		if a.MaxCallsPerDay <= 0 {
			a.MaxCallsPerDay = da.MaxCallsPerDay
		}
	}
	if c.OKX == nil {
		c.OKX = d.OKX
	} else {
		o, do := c.OKX, d.OKX
		if o.BaseURL == "" {
			o.BaseURL = do.BaseURL
		}
		if len(o.FallbackURLs) == 0 {
			o.FallbackURLs = do.FallbackURLs
		}
	}
	if c.Store == nil {
		c.Store = d.Store
	} else {
		s, ds := c.Store, d.Store
		if s.Driver == "" {
			s.Driver = ds.Driver
		}
		if s.Host == "" {
			s.Host = ds.Host
		}
		if s.Port == 0 {
			s.Port = ds.Port
		}
		if s.User == "" {
			s.User = ds.User
		}
		if s.Password == "" {
			s.Password = ds.Password
		}
		if s.Database == "" {
			s.Database = ds.Database
		}
		if s.MaxOpenConns <= 0 {
			s.MaxOpenConns = ds.MaxOpenConns
		}
		if s.MaxIdleConns <= 0 {
			s.MaxIdleConns = ds.MaxIdleConns
		}
		if s.BatchSize <= 0 {
			s.BatchSize = ds.BatchSize
		}
		if s.KeepKlineBars <= 0 {
			s.KeepKlineBars = ds.KeepKlineBars
		}
		if s.LogDir == "" {
			s.LogDir = ds.LogDir
		}
		if s.LogMaxMB <= 0 {
			s.LogMaxMB = ds.LogMaxMB
		}
		if s.LogKeep <= 0 {
			s.LogKeep = ds.LogKeep
		}
	}
}

// ThresholdFor 取某合约的达标阈值
func (c *Config) ThresholdFor(instID string) int {
	if c.ScoreThresholdMap != nil {
		if v, ok := c.ScoreThresholdMap[instID]; ok && v > 0 {
			if v > 8 {
				v = 8
			}
			return v
		}
	}
	return c.ScoreThreshold
}

// BarEnabled 该周期是否允许扫描开仓
func (c *Config) BarEnabled(bar string) bool {
	for _, b := range c.BarsEnabled {
		if strings.EqualFold(strings.TrimSpace(b), bar) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

// StripJSONComments 去掉 JSON 里的 // 和 /* */ 注释（字符串内部不动）
//
// 注意：不能用正则 `//.*$` 去剥 —— 那会把 "https://api.deepseek.com" 这类
// 字符串里的 // 也当成注释起点，直接把 JSON 切坏。所以老老实实按字符扫，
// 记录「当前是否在字符串里」和「是否刚吃过转义符」。
func StripJSONComments(b []byte) []byte {
	out := make([]byte, 0, len(b))
	inStr := false
	esc := false
	for i := 0; i < len(b); i++ {
		ch := b[i]
		if inStr {
			out = append(out, ch)
			if esc {
				esc = false
				continue
			}
			if ch == '\\' {
				esc = true
				continue
			}
			if ch == '"' {
				inStr = false
			}
			continue
		}
		if ch == '"' {
			inStr = true
			out = append(out, ch)
			continue
		}
		if ch == '/' && i+1 < len(b) {
			if b[i+1] == '/' {
				for i < len(b) && b[i] != '\n' {
					i++
				}
				if i < len(b) {
					out = append(out, '\n')
				}
				continue
			}
			if b[i+1] == '*' {
				i += 2
				for i+1 < len(b) && !(b[i] == '*' && b[i+1] == '/') {
					i++
				}
				i++
				continue
			}
		}
		out = append(out, ch)
	}
	return out
}

// ---------------------------------------------------------------------------
// 派生口径
// ---------------------------------------------------------------------------

// OrderMarginCap 单笔保证金硬上限（USDT）。
// 「不要买多，超过太多不好」—— 任何下单路径都不得突破这个数。
func (c *Config) OrderMarginCap() float64 {
	if c.MaxOrderMarginUSDT > 0 {
		return c.MaxOrderMarginUSDT
	}
	if c.Entry != nil && c.Entry.MaxMarginUSDT > 0 {
		return c.Entry.MaxMarginUSDT
	}
	return 0.5
}

// PerTradeMargin 单笔计划保证金（USDT），默认 0.1
func (c *Config) PerTradeMargin() float64 {
	if c.Entry != nil && c.Entry.MarginUSDT > 0 {
		return c.Entry.MarginUSDT
	}
	return 0.1
}

// PlanLeverage 计划杠杆
func (c *Config) PlanLeverage() int {
	if c.Entry != nil && c.Entry.Leverage > 0 {
		return c.Entry.Leverage
	}
	return 20
}
