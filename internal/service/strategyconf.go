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
	MaxHoldBars   int     `json:"max_hold_bars"`
	StopLossPct   float64 `json:"stop_loss_pct"`
}

// StrategyConfig 只取前端要展示的字段
type StrategyConfig struct {
	Enabled           bool     `json:"enabled"`
	DryRun            bool     `json:"dry_run"`
	Bar               string   `json:"bar"`
	BarsEnabled       []string `json:"bars_enabled"`
	ScoreThreshold    int      `json:"score_threshold"`
	MinQuoteVolume24h float64  `json:"min_quote_volume_24h"`
	TopNByVolume      int      `json:"top_n_by_volume"`

	// 合约准入（「哪些能买」）
	ExcludeStockETF       bool    `json:"exclude_stock_etf"`
	ExcludeNewListingDays int     `json:"exclude_new_listing_days"`
	ExcludeDelisting      bool    `json:"exclude_delisting"`
	MaxOrderMarginUSDT    float64 `json:"max_order_margin_usdt"`

	Entry StrategyEntry `json:"entry"`
	Exit  StrategyExit  `json:"exit"`

	Path string `json:"path"` // 配置文件路径（不在 JSON 里）
}

// LoadStrategy 读配置文件。带注释的 JSON 也能读（先把注释剥掉）。
// 文件不存在时返回内置默认值 + error，调用方自己决定怎么处理。
func LoadStrategy(path string) (*StrategyConfig, error) {
	def := &StrategyConfig{
		Enabled: true, DryRun: true, Bar: "15m",
		BarsEnabled: []string{"15m"}, ScoreThreshold: 6,
		MinQuoteVolume24h: 1000000, TopNByVolume: 80,
		ExcludeStockETF: true, ExcludeNewListingDays: 30, ExcludeDelisting: true,
		MaxOrderMarginUSDT: 0.5,
		Entry: StrategyEntry{TdMode: "isolated", PosSide: "net", OrdType: "market",
			MarginUSDT: 0.1, Leverage: 20, MaxConcurrentPositions: 8,
			CooldownBars: 6, DailyMaxEntries: 30, MarginPolicy: "min_one", MaxMarginUSDT: 0.5},
		Exit: StrategyExit{TakeProfitPct: 2.0, BollUpperExit: true},
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
	// 硬上限不得低于目标每笔保证金，否则「0.1U」永远买不起任何合约
	if cfg.MaxOrderMarginUSDT < cfg.Entry.MarginUSDT {
		cfg.MaxOrderMarginUSDT = cfg.Entry.MarginUSDT
	}
	if cfg.MaxOrderMarginUSDT <= 0 {
		cfg.MaxOrderMarginUSDT = cfg.Entry.MarginUSDT
	}
	if cfg.ExcludeNewListingDays < 0 {
		cfg.ExcludeNewListingDays = 0
	}
	if cfg.MinQuoteVolume24h < 0 {
		cfg.MinQuoteVolume24h = 0
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
