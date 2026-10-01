package service

// config.go —— 读 runtime/okx_strategy.json，把交易参数透给网页前端
//
// 策略引擎（runtime 包）自己也会读这个文件并热加载，这里只读不写，
// 目的是让客户端能显示「当前每笔保证金多少 U、几倍杠杆、止盈多少」。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"finally-main/internal/conf"
)

// StrategyEntry 开仓参数
type StrategyEntry struct {
	TdMode                 string  `json:"td_mode"`
	PosSide                string  `json:"pos_side"`
	OrdType                string  `json:"ord_type"`
	MarginUSDT             float64 `json:"margin_usdt"`
	Leverage               int     `json:"leverage"`
	MaxConcurrentPositions int     `json:"max_concurrent_positions"`
	CooldownBars           int     `json:"cooldown_bars"`
	DailyMaxEntries        int     `json:"daily_max_entries"`
	MarginPolicy           string  `json:"margin_policy"`
	MaxMarginUSDT          float64 `json:"max_margin_usdt"`
}

// StrategyExit 出场参数
type StrategyExit struct {
	TakeProfitPct float64 `json:"take_profit_pct"`
	BollUpperExit bool    `json:"boll_upper_exit"`
	// MaxHoldMinutes 超时平仓（分钟）。>0 时优先于 MaxHoldBars。
	// 用户口径：开仓满 360 分钟（6 小时）还没止盈就自动市价平掉。
	MaxHoldMinutes int     `json:"max_hold_minutes"`
	MaxHoldBars    int     `json:"max_hold_bars"`
	StopLossPct    float64 `json:"stop_loss_pct"`
}

// StrategyAddon 加仓参数（前端展示用）
//
//	用户口径：加仓最多 3 次（只限制继续补仓，与出场无关）。
type StrategyAddon struct {
	Enabled  bool    `json:"enabled"`
	Ratio    float64 `json:"ratio"`
	DropPct  float64 `json:"drop_pct"`
	RiseBar  string  `json:"rise_bar"`
	MaxTimes int     `json:"max_times"`
}

// StrategyConfig 只取前端要展示的字段
type StrategyConfig struct {
	Enabled           bool     `json:"enabled"`
	DryRun            bool     `json:"dry_run"`
	Bar               string   `json:"bar"`
	BarsEnabled       []string `json:"bars_enabled"`
	SignalBars        []string `json:"signal_bars"` // 信号回算/图上展示的周期（1m/3m 已下线）
	ScoreThreshold    int      `json:"score_threshold"`
	MinQuoteVolume24h float64  `json:"min_quote_volume_24h"`
	TopNByVolume      int      `json:"top_n_by_volume"`

	// 合约准入（「哪些能买」）
	ExcludeStockETF       bool    `json:"exclude_stock_etf"`
	ExcludeNewListingDays int     `json:"exclude_new_listing_days"`
	ExcludeDelisting      bool    `json:"exclude_delisting"`
	MaxOrderMarginUSDT    float64 `json:"max_order_margin_usdt"`

	// 本金（USDT）。用来算「累计收益率 = 总盈亏 ÷ 本金」。
	// 留 0 = 由「权益 − 浮盈 − 已实现盈亏」自动反推（没有出入金时是准的）。
	PrincipalUSDT float64 `json:"principal_usdt"`

	Entry StrategyEntry `json:"entry"`
	Exit  StrategyExit  `json:"exit"`
	Addon StrategyAddon `json:"addon"`
	Live  StrategyLive  `json:"live"`

	Path string `json:"path"` // 配置文件路径（不在 JSON 里）
}

// StrategyLive 实时引擎的两条心跳间隔（秒）
//
//	ExitSec  止盈巡检：只看在持仓，浮盈够线立刻平。要快，默认 3 秒。
//	EntrySec 买入信号扫描：全市场扫一遍，贵。默认 60 秒。
type StrategyLive struct {
	ExitSec  int `json:"exit_sec"`
	EntrySec int `json:"entry_sec"`
}

// LoadStrategy 读配置文件。带注释的 JSON 也能读（先把注释剥掉）。
// 文件不存在时返回内置默认值 + error，调用方自己决定怎么处理。
func LoadStrategy(path string) (*StrategyConfig, error) {
	def := &StrategyConfig{
		Enabled: true, DryRun: true, Bar: "15m",
		BarsEnabled: []string{"15m"}, SignalBars: []string{"15m"},
		ScoreThreshold: 6,
		MinQuoteVolume24h: 1000000, TopNByVolume: 80,
		ExcludeStockETF: true, ExcludeNewListingDays: 30, ExcludeDelisting: true,
		// ↑↓ 这些数字全是「配置文件缺失 / 解析失败」时的兜底，
		//    真正生效的口径永远来自 configs/okx_strategy.json（热插拔）。
		MaxOrderMarginUSDT: 1.0,
		Entry: StrategyEntry{TdMode: "isolated", PosSide: "net", OrdType: "market",
			// ★ 0 = 不限（用户口径「取消限制」）。这里只是「配置文件读不到」时的兜底，
			//   与 conf.DefaultConfig 保持一致，免得兜底值把限制偷偷放回来。
			MarginUSDT: 1.0, Leverage: 20, MaxConcurrentPositions: 0,
			CooldownBars: 6, DailyMaxEntries: 0, MarginPolicy: "min_one", MaxMarginUSDT: 1.5},
		Exit: StrategyExit{TakeProfitPct: 1.0, BollUpperExit: true, MaxHoldMinutes: 360},
		Addon: StrategyAddon{Enabled: true, Ratio: 1.0 / 3.0, DropPct: 0.5,
			RiseBar: "15m", MaxTimes: 3},
		Live: StrategyLive{ExitSec: 3, EntrySec: 60},
		Path: path,
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return def, err
	}
	clean := conf.StripJSONComments(raw)
	cfg := *def
	if err := json.Unmarshal(clean, &cfg); err != nil {
		return def, err
	}
	cfg.Path = path
	if cfg.Entry.MarginUSDT <= 0 {
		cfg.Entry.MarginUSDT = def.Entry.MarginUSDT
	}
	if cfg.Entry.Leverage <= 0 {
		cfg.Entry.Leverage = def.Entry.Leverage
	}
	if cfg.Bar == "" {
		cfg.Bar = "15m"
	}
	// 资金口径归一化：只认 fixed / min_one，其余一律回到严格的 fixed
	switch cfg.Entry.MarginPolicy {
	case "min_one", "fixed":
	default:
		cfg.Entry.MarginPolicy = "fixed"
	}
	// 准入上限（max_order_margin_usdt）：**以 JSON 里写的为准**，
	// 这里不做任何「要求」。
	//
	// 历史包袱：曾有一段「上限不得低于每笔保证金」的钳制，那是错的 ——
	// 把上限收紧到比每笔保证金更低（每笔 1U、但只买最小一手 ≤0.5U 的合约）
	// 是完全合理的收紧，不该被偷偷改回去；而放宽（1U → 1.5U）也只需改 JSON。
	// 现在只有「没填 / 写了个负数」才回落到兜底值。
	if cfg.MaxOrderMarginUSDT <= 0 {
		cfg.MaxOrderMarginUSDT = cfg.Entry.MarginUSDT
		if cfg.MaxOrderMarginUSDT <= 0 {
			cfg.MaxOrderMarginUSDT = def.MaxOrderMarginUSDT
		}
	}
	if cfg.ExcludeNewListingDays < 0 {
		cfg.ExcludeNewListingDays = 0
	}
	if cfg.MinQuoteVolume24h < 0 {
		cfg.MinQuoteVolume24h = 0
	}
	// 实时引擎节奏归一化：配置里没写 live 段时用默认值，
	// 并且夹住下限——止盈巡检最快 1 秒，再快就是白烧 OKX 接口。
	if cfg.Live.ExitSec <= 0 {
		cfg.Live.ExitSec = def.Live.ExitSec
	}
	if cfg.Live.ExitSec < 1 {
		cfg.Live.ExitSec = 1
	}
	if cfg.Live.EntrySec <= 0 {
		cfg.Live.EntrySec = def.Live.EntrySec
	}
	if cfg.Live.EntrySec < 5 {
		cfg.Live.EntrySec = 5
	}
	// 加仓口径归一化：「最多 3 次」是用户硬口径，配置里写 0/负数一律回默认。
	if cfg.Addon.MaxTimes <= 0 {
		cfg.Addon.MaxTimes = def.Addon.MaxTimes
	}
	if cfg.Addon.Ratio <= 0 {
		cfg.Addon.Ratio = def.Addon.Ratio
	}
	if cfg.Addon.RiseBar == "" {
		cfg.Addon.RiseBar = def.Addon.RiseBar
	}
	if cfg.Exit.MaxHoldMinutes < 0 {
		cfg.Exit.MaxHoldMinutes = 0
	}
	return &cfg, nil
}

// StrategyConfigPath 找配置文件（从项目根往下找）
func StrategyConfigPath(root string) string {
	cands := []string{
		filepath.Join(root, "configs", "okx_strategy.json"),
		filepath.Join("configs", "okx_strategy.json"),
		filepath.Join(root, "okx_strategy.json"),
		"okx_strategy.json",
	}
	for _, c := range cands {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	return cands[0]
}

// MarginText 给前端显示的一句话，例如 "0.1U/笔 · 20x"
func (s *StrategyConfig) MarginText() string {
	if s == nil {
		return ""
	}
	return trimZero(s.Entry.MarginUSDT) + "U/笔 · " + strconv.Itoa(s.Entry.Leverage) + "x"
}

func trimZero(v float64) string {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	if s == "" || s == "-" {
		return "0"
	}
	return s
}
