package service

// strategy_store.go —— 策略配置「热插拔」：改 configs/okx_strategy.json 即刻生效
//
// 背景（2026-10-01）：
//   引擎下单那一路走的是 conf.LoadConfig()，它本来就比对文件 mtime 热加载，
//   改完 JSON 下一个 tick 就生效。但「合约准入 / 网页展示」这一路走的是
//   service.LoadStrategy()，只在进程启动时读一次 —— 结果就是：
//     改 max_order_margin_usdt（可买入金额）→ symbolList 纹丝不动，
//     必须重启 OKXWeb 服务才变。
//
//   这个文件把那条路补成热读：
//     · Get()    先 stat 挡一层，mtime/size 变了才重读；内容 sha256 相同就算没变；
//     · Watch()  「文件一变就跑一次回调」的钩子（用来立刻重跑全市场准入过滤）；
//     · 读失败（正好保存到一半）沿用上一份，绝不让策略中断。
//
//   ★ 口径只有一处真源：configs/okx_strategy.json。
//     Go 里的默认值只是「文件缺失 / 解析失败」时的兜底，**不构成任何要求**。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"time"
)

// StrategyStore 线程安全的策略配置持有者（热读）
type StrategyStore struct {
	path string
	logf func(string, ...any)

	mu      sync.RWMutex
	cur     *StrategyConfig
	sum     string // 上次成功加载的文件内容 sha256
	modTime time.Time
	size    int64
	loaded  bool
}

// NewStrategyStore 建一个热读存储。path 不存在也行 —— 会用 LoadStrategy 的
// 默认值兜底，之后文件出现了会自动认到。
func NewStrategyStore(path string, logf func(string, ...any)) *StrategyStore {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := &StrategyStore{path: path, logf: logf}
	if _, _, err := s.Force(); err != nil { // 启动先读一次，保证 Get() 永不 nil
		s.logf("读取 %s 失败（先用默认值兜底，文件出现后会自动加载）：%v", path, err)
	}
	return s
}

// Path 配置文件路径
func (s *StrategyStore) Path() string { return s.path }

// Get 取当前生效的配置。文件被外部改过会自动重读，**永不返回 nil**。
//
// ★ 所有调用方都该走这里 —— 只要调它，天生就是热加载。
func (s *StrategyStore) Get() *StrategyConfig {
	if s.stale() {
		if _, _, err := s.Force(); err != nil {
			s.logf("重读 %s 失败，继续沿用上一份：%v", s.path, err)
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur
}

// Force 无条件重读文件。
//
// changed 表示「内容与上次成功加载的不同」，调用方可据此决定要不要
// 重跑下游（例如重算合约准入）。
func (s *StrategyStore) Force() (cfg *StrategyConfig, changed bool, err error) {
	raw, rerr := os.ReadFile(s.path)

	sum := ""
	var mt time.Time
	var size int64
	if fi, e := os.Stat(s.path); e == nil {
		mt, size = fi.ModTime(), fi.Size()
	}
	if raw != nil {
		h := sha256.Sum256(raw)
		sum = hex.EncodeToString(h[:])
	}

	s.mu.RLock()
	same := s.loaded && sum != "" && sum == s.sum
	prev := s.cur
	s.mu.RUnlock()
	if same {
		return prev, false, nil
	}

	loaded, lerr := LoadStrategy(s.path)
	if lerr != nil {
		// ★ 读失败（文件不存在 / 正好在保存到一半 / JSON 写坏了）：
		//   已经有配置就**沿用上一份**，绝不用默认值把它覆盖掉 ——
		//   否则一次手滑的语法错误就会把整个策略静默打回出厂设置。
		//   也不更新 sum，所以下次 Get() 还会继续重试。
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.cur == nil {
			s.cur = loaded // 首次就失败 → 只能先用默认值兜底
			s.logf("⚠ %s 读取失败，先用默认值兜底（%s）：%v",
				s.path, loaded.MarginText(), lerr)
		} else {
			s.logf("⚠ %s 读取失败，继续沿用上一份配置：%v", s.path, lerr)
		}
		return s.cur, false, lerr
	}

	s.mu.Lock()
	s.cur, s.sum, s.modTime, s.size, s.loaded = loaded, sum, mt, size, true
	s.mu.Unlock()

	if prev == nil {
		s.logf("策略配置已加载：%s（%s，准入上限 %sU）",
			s.path, loaded.MarginText(), trimZero(loaded.MaxOrderMarginUSDT))
	} else {
		s.logf("★ 策略配置已热更新：%s", s.path)
		s.logf("  %s", diffStrategy(prev, loaded))
	}
	return loaded, prev != nil, rerr
}

// stale 文件是否可能变了（先用 stat 挡一层，避免每个请求都读整个文件）
func (s *StrategyStore) stale() bool {
	s.mu.RLock()
	loaded, mt, size := s.loaded, s.modTime, s.size
	s.mu.RUnlock()
	if !loaded {
		return true
	}
	fi, err := os.Stat(s.path)
	if err != nil {
		return false // 文件没了：沿用现状，等它回来
	}
	return fi.ModTime() != mt || fi.Size() != size
}

// Watch 每 every 检查一次文件，**内容真的变了**才调 onChange。
//
// 用内容 sha256 判定而不是 mtime：编辑器「另存为」会让 mtime 变但内容没变，
// 那种情况不该重跑一遍全市场准入过滤。
func (s *StrategyStore) Watch(ctx context.Context, every time.Duration, onChange func(*StrategyConfig)) {
	if every <= 0 {
		every = 3 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !s.stale() {
				continue
			}
			cfg, changed, err := s.Force()
			if err != nil {
				s.logf("热加载失败（沿用上一份）：%v", err)
				continue
			}
			if changed && onChange != nil {
				onChange(cfg)
			}
		}
	}
}

// diffStrategy 把两次配置的关键差异压成一行，日志里能立刻确认「到底有没有改到」
func diffStrategy(a, b *StrategyConfig) string {
	if a == nil || b == nil {
		return "（无对比基准）"
	}
	parts := make([]string, 0, 12)
	// 前置 ★ 的是「改这个就能影响 symbolList」的字段，排在前面
	add := func(name string, x, y any) {
		if fmt.Sprint(x) != fmt.Sprint(y) {
			parts = append(parts, fmt.Sprintf("%s %v → %v", name, x, y))
		}
	}
	add("★准入上限U", a.MaxOrderMarginUSDT, b.MaxOrderMarginUSDT)
	add("★成交额下限", a.MinQuoteVolume24h, b.MinQuoteVolume24h)
	add("★排除美股ETF", a.ExcludeStockETF, b.ExcludeStockETF)
	add("★新上线天数", a.ExcludeNewListingDays, b.ExcludeNewListingDays)
	add("★排除下线", a.ExcludeDelisting, b.ExcludeDelisting)
	add("每笔保证金U", a.Entry.MarginUSDT, b.Entry.MarginUSDT)
	add("杠杆", a.Entry.Leverage, b.Entry.Leverage)
	add("单笔硬上限U", a.Entry.MaxMarginUSDT, b.Entry.MaxMarginUSDT)
	add("保证金口径", a.Entry.MarginPolicy, b.Entry.MarginPolicy)
	add("止盈%", a.Exit.TakeProfitPct, b.Exit.TakeProfitPct)
	add("共振阈值", a.ScoreThreshold, b.ScoreThreshold)
	add("dry_run", a.DryRun, b.DryRun)
	if len(parts) == 0 {
		return "关键字段无变化（只改了注释之类）"
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += "；" + p
	}
	return out
}
