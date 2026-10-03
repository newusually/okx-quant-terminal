package service

// strategy_write.go —— 把管理台改的配置**原子写回** configs/okx_strategy.json
//
// 背景（2026-10-02 八期）：
//   管理台要能改「买入条件 / 加仓条件 / 金额」并立刻生效。改动落到唯一真源
//   configs/okx_strategy.json，靠已有的 strategyStore.Watch（2 秒轮询 + sha256 比对）
//   自动热加载 —— 这正是用户要的「保存后更新这些条件」，不需要重启进程。
//
// ★ 为什么不 json.Marshal 整份重写 ★
//   真源 JSON 里有大量注释，是给用户读懂口径用的（「为什么是 -0.7」「六期为什么反转」
//   这些都写在注释里）。Marshal 会把注释全丢，等于每次保存都把文档清一遍。
//   所以这里走**定点替换**：按 key 找到那一行的值，只替换数字，注释一字不动。
//
// ★ 原子性 ★
//   写临时文件 → fsync → os.Rename 覆盖。os.Rename 在同一分区上是原子的，
//   所以永远不会有「读到一个写了一半的 JSON」的窗口。
//   而 StrategyStore.Force() 本来就对读失败做了「沿用上一份」的兜底，
//   两层加起来，保存动作不会让引擎掉一拍。

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"

	"finally-main/internal/conf"
)

// 加仓模式常量别名（前端与写回逻辑共用一套字面量，避免两处各写一份字符串）
const (
	AddonModeResonance = conf.AddonModeResonance
	AddonModePrice     = conf.AddonModePrice
)

// StrategyWriter 串行化配置写入。
//
// 为什么要锁：两个管理员同时保存，或者保存与「另一个进程在改同一份文件」撞上，
// 会出现「后写的覆盖先写的」。这里序列化本进程内的写；
// 跨进程的冲突由「都读最新文件再改」+ 原子 rename 收敛（本机只有 OKXWeb 一个写者）。
type StrategyWriter struct {
	mu   sync.Mutex
	path string
	logf func(string, ...any)
}

// NewStrategyWriter 建写入器
func NewStrategyWriter(path string, logf func(string, ...any)) *StrategyWriter {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &StrategyWriter{path: path, logf: logf}
}

// Patch 一次保存请求要改的字段。
//
// 每个字段都是**指针**：nil = 「这次不动它」。
// 用指针而不是「零值表示不改」是刻意的 —— 用户完全可能想把某项改成 0
// （比如把 min_bar_rise_pct 改成 0 = 关闭该条件），
// 零值语义混淆的话那个 0 会被当成「没填」丢掉，正是本项目踩过多次的坑。
//
// ★ 九期：从「只管买入/加仓」扩到「整份可热插拔的口径」★
//
//	分组顺序与管理员页面上的分区一一对应，方便对照排查：
//	  总开关 → 买入 → 开仓闸门 → 合约准入 → 加仓 → 出场 → 风控 → 引擎节奏
//
//	⚠ 每加一个字段，必须同时在三个地方登记，少一处就是「改了没用」：
//	    1. 本结构体（带 json tag）
//	    2. ParsePatch 里解析它
//	    3. Patch.Empty() 里判断它
//	  —— 第 3 条最容易漏：漏了之后「只改这一个字段」会被判成「没有要改的字段」，
//	     接口直接报错，而用户以为自己填的东西没保存成功。
type Patch struct {
	// ---- 总开关 ----
	BuyEnabled *bool `json:"buy_enabled"` // 根级 enabled：策略总开关
	DryRun     *bool `json:"dry_run"`     // 根级 dry_run：true = 只出信号不下单

	// ---- 买入条件 ----
	ScoreThreshold *int     `json:"score_threshold"`  // 根级：8 因子里命中几项
	MinBarRisePct  *float64 `json:"min_bar_rise_pct"` // 带符号：<0 必须真跌，>0 必须真涨，0=关
	MaxBarDropPct  *float64 `json:"max_bar_drop_pct"` // 二十二期：跌幅上限（正数），<=0=关
	BuyMarginUSDT  *float64 `json:"buy_margin_usdt"`  // entry.margin_usdt 每笔买入保证金
	MaxMarginUSDT  *float64 `json:"max_margin_usdt"`  // entry.max_margin_usdt 单笔硬上限
	Leverage       *int     `json:"leverage"`         // entry.leverage 杠杆
	MarginPolicy   *string  `json:"margin_policy"`    // entry.margin_policy: min_one | fixed
	BuyUseScore    *bool    `json:"buy_use_score"`    // 是否启用「分数」这个条件
	BuyUseBarRise  *bool    `json:"buy_use_bar_rise"` // 是否启用「K线涨跌幅」这个条件

	// ---- 开仓闸门（entry）★ 九期新增：用户要能把「冷却中」这条限制自己调 ★
	CooldownBars           *int `json:"cooldown_bars"`            // 同一合约两次开仓最小间隔根数；0 = 不冷却
	MaxConcurrentPositions *int `json:"max_concurrent_positions"` // 0 = 不限
	DailyMaxEntries        *int `json:"daily_max_entries"`        // 0 = 不限

	// ---- 合约准入（根级）----
	MaxOrderMarginUSDT    *float64 `json:"max_order_margin_usdt"`    // 最小一手保证金 ≤ 它才做
	ExcludeNewListingDays *int     `json:"exclude_new_listing_days"` // 上市不足 N 天不买；0 = 不排除
	ExcludeDelisting      *bool    `json:"exclude_delisting"`        // 排除待下线
	ExcludeStockETF       *bool    `json:"exclude_stock_etf"`        // 排除美股/ETF/商品
	MinQuoteVolume24h     *float64 `json:"min_quote_volume_24h"`     // 24h 成交额下限（U）
	TopNByVolume          *int     `json:"top_n_by_volume"`          // 只算成交额前 N 名

	// ---- 加仓条件 ----
	AddonMode       *string  `json:"addon_mode"` // "resonance" | "price"
	AddonEnabled    *bool    `json:"addon_enabled"`
	AddonScore      *int     `json:"addon_score"`        // 共振模式：加仓分数阈值
	AddonBarRise    *float64 `json:"addon_bar_rise"`     // 共振模式：该根涨幅须 > N%
	AddonDropPct    *float64 `json:"addon_drop_pct"`     // 价格模式：收盘价低于买入价 N%
	AddonRisePct    *float64 `json:"addon_rise_pct"`     // 价格模式：该根涨幅须 > N%
	AddonMarginU    *float64 `json:"addon_margin_usdt"`  // 加仓金额（U）；0 = 用 ratio 比例
	AddonRatio      *float64 `json:"addon_ratio"`        // 加仓比例（margin_usdt=0 时生效）
	AddonMaxTimes   *int     `json:"addon_max_times"`    // 0 = 不限
	AddonMinGapBars *int     `json:"addon_min_gap_bars"` // 两次加仓至少隔 N 根；0 = 同一根也可
	AddonRiseBar    *string  `json:"addon_rise_bar"`     // auto = 用该仓位自己的周期

	// ---- 出场条件（exit）★ 九期新增：从「只读」改为「可写」★ ----
	TakeProfitPct  *float64 `json:"take_profit_pct"`  // 止盈%；0 = 关闭（不建议，超时平仓净值会变随机）
	StopLossPct    *float64 `json:"stop_loss_pct"`    // 止损%；300 ≈ 不设止损
	MaxHoldMinutes *int     `json:"max_hold_minutes"` // 超时平仓（分钟）
	MaxHoldBars    *int     `json:"max_hold_bars"`    // 超时平仓（根）；>0 时被分钟优先
	BollUpperExit  *bool    `json:"boll_upper_exit"`  // 布林上轨出场（与买入同根 K 线，易秒进秒出）

	// ---- 风控（risk）★ 九期新增 ★ ----
	AccountEquityStop    *float64 `json:"account_equity_stop"`    // 权益低于它全停；0 = 不启用
	DailyLossStopPct     *float64 `json:"daily_loss_stop_pct"`    // 当日亏损超权益 N% 停开仓
	MaxTotalMarginPct    *float64 `json:"max_total_margin_pct"`   // 总保证金不超权益 N%
	MinAvailableUSDT     *float64 `json:"min_available_usdt"`     // 可用余额低于 N U 停开仓
	ConsecutiveLossPause *int     `json:"consecutive_loss_pause"` // 连亏 N 笔暂停 2 小时
	PauseOnAPIError      *int     `json:"pause_on_api_error"`     // 连续 N 次 API 错误暂停

	// ---- 引擎节奏（live）★ 九期新增 ★ ----
	LiveExitSec  *int `json:"live_exit_sec"`  // 止盈巡检间隔（秒），下限 1
	LiveEntrySec *int `json:"live_entry_sec"` // 买入扫描间隔（秒），下限 5
}

// ParsePatch 从任意 map 解析出 Patch。前端传什么就解什么，缺的字段留 nil。
//
// 刻意的宽松：不认识的键直接忽略（前端多传参数不该 500），
// 认识但类型不对的键**返回错误**（静默忽略会让「改了没用」再次上演）。
func ParsePatch(body map[string]any) (*Patch, error) {
	p := &Patch{}
	var errs []string

	iptr := func(key string) *int {
		v, ok := body[key]
		if !ok || v == nil {
			return nil
		}
		f, ok := toFloat(v)
		if !ok {
			errs = append(errs, key+" 应为数字")
			return nil
		}
		n := int(f)
		return &n
	}
	fptr := func(key string) *float64 {
		v, ok := body[key]
		if !ok || v == nil {
			return nil
		}
		f, ok := toFloat(v)
		if !ok {
			errs = append(errs, key+" 应为数字")
			return nil
		}
		return &f
	}
	bptr := func(key string) *bool {
		v, ok := body[key]
		if !ok || v == nil {
			return nil
		}
		b, ok := v.(bool)
		if !ok {
			errs = append(errs, key+" 应为布尔值")
			return nil
		}
		return &b
	}
	sptr := func(key string) *string {
		v, ok := body[key]
		if !ok || v == nil {
			return nil
		}
		s, ok := v.(string)
		if !ok {
			errs = append(errs, key+" 应为字符串")
			return nil
		}
		return &s
	}

	p.ScoreThreshold = iptr("score_threshold")
	p.BuyMarginUSDT = fptr("buy_margin_usdt")
	p.MaxMarginUSDT = fptr("max_margin_usdt")
	p.BuyEnabled = bptr("buy_enabled")
	p.BuyUseScore = bptr("buy_use_score")
	p.BuyUseBarRise = bptr("buy_use_bar_rise")
	p.MinBarRisePct = fptr("min_bar_rise_pct")
	p.MaxBarDropPct = fptr("max_bar_drop_pct")

	// —— 九期新增：总开关 / 买入参数 / 开仓闸门 / 准入 ——
	p.DryRun = bptr("dry_run")
	p.Leverage = iptr("leverage")
	p.MarginPolicy = sptr("margin_policy")
	p.CooldownBars = iptr("cooldown_bars")
	p.MaxConcurrentPositions = iptr("max_concurrent_positions")
	p.DailyMaxEntries = iptr("daily_max_entries")
	p.MaxOrderMarginUSDT = fptr("max_order_margin_usdt")
	p.ExcludeNewListingDays = iptr("exclude_new_listing_days")
	p.ExcludeDelisting = bptr("exclude_delisting")
	p.ExcludeStockETF = bptr("exclude_stock_etf")
	p.MinQuoteVolume24h = fptr("min_quote_volume_24h")
	p.TopNByVolume = iptr("top_n_by_volume")

	p.AddonMode = sptr("addon_mode")
	p.AddonEnabled = bptr("addon_enabled")
	p.AddonScore = iptr("addon_score")
	p.AddonBarRise = fptr("addon_bar_rise")
	p.AddonDropPct = fptr("addon_drop_pct")
	p.AddonRisePct = fptr("addon_rise_pct")
	p.AddonMarginU = fptr("addon_margin_usdt")
	p.AddonRatio = fptr("addon_ratio")
	p.AddonMaxTimes = iptr("addon_max_times")
	p.AddonMinGapBars = iptr("addon_min_gap_bars")
	p.AddonRiseBar = sptr("addon_rise_bar")

	// —— 九期新增：出场 / 风控 / 引擎节奏 ——
	p.TakeProfitPct = fptr("take_profit_pct")
	p.StopLossPct = fptr("stop_loss_pct")
	p.MaxHoldMinutes = iptr("max_hold_minutes")
	p.MaxHoldBars = iptr("max_hold_bars")
	p.BollUpperExit = bptr("boll_upper_exit")
	p.AccountEquityStop = fptr("account_equity_stop")
	p.DailyLossStopPct = fptr("daily_loss_stop_pct")
	p.MaxTotalMarginPct = fptr("max_total_margin_pct")
	p.MinAvailableUSDT = fptr("min_available_usdt")
	p.ConsecutiveLossPause = iptr("consecutive_loss_pause")
	p.PauseOnAPIError = iptr("pause_on_api_error")
	p.LiveExitSec = iptr("live_exit_sec")
	p.LiveEntrySec = iptr("live_entry_sec")

	if len(errs) > 0 {
		return nil, fmt.Errorf("参数不合法：%s", strings.Join(errs, "；"))
	}
	if p.Empty() {
		return nil, fmt.Errorf("没有任何要修改的字段")
	}
	return p, nil
}

// Empty 是否什么都没要改。
//
// ★ 九期改用反射遍历 ★
//
//	原来是手写一串 `p.A == nil && p.B == nil && ...`。九期字段从 15 个涨到 40+，
//	手写清单有两个真实风险：
//	  ① 加字段时忘了补 —— 「只改这一个字段」会被判成「没有要改的字段」，
//	     接口直接报「没有任何要修改的字段」，而用户明明填了；
//	  ② 删字段时漏删 —— 编译能过，只是一个永远为真的条件，脏但难发现。
//
//	反射遍历一遍指针字段，只要有一个非 nil 就是「有改动」。
//	代价是每次保存多几微秒（这个接口一分钟都不会被调一次），
//	换来的是一条**不可能再漏**的规则。
//
//	约定：Patch 的字段**必须全是指针**（nil = 不改）。反射只看指针字段，
//	所以万一有人加了非指针字段，它会自动被视为「不参与判空」——
//	配合下面的单测把它钉住。
func (p *Patch) Empty() bool {
	v := reflect.ValueOf(p).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if f.Kind() == reflect.Ptr && !f.IsNil() {
			return false
		}
	}
	return true
}

// toFloat 把 JSON 数字（float64）或数字字符串都接受
func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case string:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(t), "%g", &f); err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// Apply 执行写回。返回实际改了哪些字段的描述（给日志/前端回显）。
//
// 流程：读原文 → 逐个定点替换 → 校验新文本能被 LoadStrategy 解析 → 原子落盘。
// 校验这一步不能省：写进去的 JSON 若解析不了，热加载会「沿用上一份」，
// 用户看到的是「保存成功但没有任何变化」——经典静默失效。
func (w *StrategyWriter) Apply(p *Patch) ([]string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	raw, err := os.ReadFile(w.path)
	if err != nil {
		return nil, fmt.Errorf("读配置失败：%w", err)
	}
	text := string(raw)
	orig := text
	var changed []string

	// 说明：这里刻意**不**提供「裸键替换」的便捷闭包。
	// 真源 JSON 里同名键跨段落出现（score_threshold / margin_usdt / mode / ...），
	// 任何「只认第一个匹配」的替换都会改错段 —— 而且改错了不报错。
	// 所有替换都必须显式声明「改的是根级还是哪一段」：
	//   · 根级 → setRootNum / setRootInt / setRootBool
	//   · 段内 → setNumInSect / setIntInSect / setBoolInSect
	// 少一个"方便"的入口，就少一类「改了没用/改错地方」的故障。

	// —— 买入条件 ——
	//
	// ★ 全部走「段落定位」而不是裸 replaceNumber ★
	//   原因见 setRootNum 的注释：`score_threshold` 在根级与 addon 段同名，
	//   `margin_usdt` / `max_margin_usdt` 在 entry 段里，
	//   裸替换只认第一个匹配 → 会改到别的段。
	if p.BuyEnabled != nil {
		if err := setRootBool(text, "enabled", *p.BuyEnabled, "策略开关", &text, &changed); err != nil {
			return nil, err
		}
	}
	// dry_run 也是根级布尔。用 setRootBool 而不是裸替换：
	// 「true/false」这种字面量在整份文件里到处都是，一旦退化成裸替换就会改错行。
	if p.DryRun != nil {
		if err := setRootBool(text, "dry_run", *p.DryRun, "试运行不下单", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.ScoreThreshold != nil {
		if err := setRootInt(text, "score_threshold", *p.ScoreThreshold, "买入分数阈值", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.MinBarRisePct != nil {
		if err := setNumInSect(text, "entry", "min_bar_rise_pct", *p.MinBarRisePct, "买入K线涨跌幅门槛", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.MaxBarDropPct != nil {
		if err := setNumInSect(text, "entry", "max_bar_drop_pct", *p.MaxBarDropPct, "买入K线跌幅上限", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.BuyMarginUSDT != nil {
		if err := setNumInSect(text, "entry", "margin_usdt", *p.BuyMarginUSDT, "每笔买入保证金U", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.MaxMarginUSDT != nil {
		if err := setNumInSect(text, "entry", "max_margin_usdt", *p.MaxMarginUSDT, "单笔硬上限U", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.Leverage != nil {
		if err := setIntInSect(text, "entry", "leverage", *p.Leverage, "杠杆", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.MarginPolicy != nil {
		// 白名单校验：写进去的值必须能被 LoadStrategy / fillDefaults 认出来，
		// 否则归一化会把它归到默认口径 —— 用户看到「保存成功」，
		// 实际跑的是另一套资金算法（买不起就跳过 vs 放大到 1 张，差别很大）。
		mp := strings.ToLower(strings.TrimSpace(*p.MarginPolicy))
		if mp != "min_one" && mp != "fixed" {
			return nil, fmt.Errorf("保证金口径只能填 min_one（放大到刚好 1 张）或 fixed（买不起就跳过），收到 %q", *p.MarginPolicy)
		}
		if err := setStrInSect(text, "entry", "margin_policy", mp, "保证金口径", &text, &changed); err != nil {
			return nil, err
		}
	}

	// —— 开仓闸门（★ 九期：用户要能自己调「冷却中」这条限制）——
	//
	//   这三个键目前只在 entry 段出现，但仍然走 setIntInSect 而不是裸替换 ——
	//   让「改的是哪一段」在代码里是可读的，不依赖对文件结构的记忆。
	//   ⚠ 语义：cooldown_bars / max_concurrent_positions / daily_max_entries
	//     三者都是 0 = 不限制，且归一化只反压负数（见 conf.fillDefaults）——
	//     所以管理台把 0 写进去是**真的能关掉**的，前端可以放心提供这个选项。
	if p.CooldownBars != nil {
		if err := setIntInSect(text, "entry", "cooldown_bars", *p.CooldownBars, "开仓冷却根数", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.MaxConcurrentPositions != nil {
		if err := setIntInSect(text, "entry", "max_concurrent_positions", *p.MaxConcurrentPositions, "最大同时持仓数", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.DailyMaxEntries != nil {
		if err := setIntInSect(text, "entry", "daily_max_entries", *p.DailyMaxEntries, "当日开仓上限", &text, &changed); err != nil {
			return nil, err
		}
	}

	// —— 合约准入（根级）——
	//
	//   这几个键都只在根级出现，但同理用 setRoot* —— 将来若有段落同名，
	//   这里的写法不需要改，裸替换则需要回头排查。
	if p.MaxOrderMarginUSDT != nil {
		if err := setRootNum(text, "max_order_margin_usdt", *p.MaxOrderMarginUSDT, "准入单笔上限U", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.ExcludeNewListingDays != nil {
		if err := setRootInt(text, "exclude_new_listing_days", *p.ExcludeNewListingDays, "新上线排除天数", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.ExcludeDelisting != nil {
		if err := setRootBool(text, "exclude_delisting", *p.ExcludeDelisting, "排除待下线", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.ExcludeStockETF != nil {
		if err := setRootBool(text, "exclude_stock_etf", *p.ExcludeStockETF, "排除美股ETF商品", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.MinQuoteVolume24h != nil {
		if err := setRootNum(text, "min_quote_volume_24h", *p.MinQuoteVolume24h, "成交额下限U", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.TopNByVolume != nil {
		if err := setRootInt(text, "top_n_by_volume", *p.TopNByVolume, "成交额前N名", &text, &changed); err != nil {
			return nil, err
		}
	}

	// —— 加仓条件 ——
	//   ★ 注意 score_threshold / margin_usdt / ratio / max_times / mode 这些键
	//     在根级或 entry 段里**同名**，所以下面一律用「段落内定位」的方式替换，
	//     不能直接用 replaceNumber（它只认第一个匹配，会改到别的段）。
	if p.AddonMode != nil {
		m := strings.ToLower(strings.TrimSpace(*p.AddonMode))
		if m != AddonModeResonance && m != AddonModePrice {
			return nil, fmt.Errorf("加仓模式只能是 %q 或 %q，收到 %q",
				AddonModeResonance, AddonModePrice, *p.AddonMode)
		}
		s, e, ok := section(text, "addon")
		if !ok {
			return nil, fmt.Errorf("配置里找不到段落 %q", "addon")
		}
		inner := text[s:e]
		ni, ok := replaceLiteral(inner, "mode", `"`+m+`"`)
		if !ok {
			return nil, fmt.Errorf("段落 %q 里找不到键 %q（无法写回）", "addon", "mode")
		}
		if ni != inner {
			changed = append(changed, "加仓模式 = "+m)
		}
		text = text[:s] + ni + text[e:]
	}
	if p.AddonEnabled != nil {
		if err := setBoolInSect(text, "addon", "enabled", *p.AddonEnabled, "加仓开关", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.AddonScore != nil {
		if err := setIntInSect(text, "addon", "score_threshold", *p.AddonScore, "加仓分数阈值", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.AddonBarRise != nil {
		if err := setNumInSect(text, "addon", "bar_rise_pct", *p.AddonBarRise, "加仓K线涨幅门槛", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.AddonDropPct != nil {
		if err := setNumInSect(text, "addon", "drop_pct", *p.AddonDropPct, "加仓跌幅门槛", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.AddonRisePct != nil {
		// 价格模式的「涨幅」与共振模式共用一个键 bar_rise_pct 会互相打架，
		// 所以价格模式用独立键 price_rise_pct（见 docs 说明）。
		if err := setNumInSect(text, "addon", "price_rise_pct", *p.AddonRisePct, "加仓价格模式涨幅门槛", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.AddonMarginU != nil {
		if err := setNumInSect(text, "addon", "margin_usdt", *p.AddonMarginU, "加仓金额U", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.AddonRatio != nil {
		if err := setNumInSect(text, "addon", "ratio", *p.AddonRatio, "加仓比例", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.AddonMaxTimes != nil {
		if err := setIntInSect(text, "addon", "max_times", *p.AddonMaxTimes, "加仓次数上限", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.AddonMinGapBars != nil {
		if err := setIntInSect(text, "addon", "min_gap_bars", *p.AddonMinGapBars, "加仓间隔根数", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.AddonRiseBar != nil {
		if err := setStrInSect(text, "addon", "rise_bar", strings.TrimSpace(*p.AddonRiseBar), "加仓判定周期", &text, &changed); err != nil {
			return nil, err
		}
	}

	// —— 出场条件（★ 九期：这一段以前是「只读展示」，现在开放为可写）——
	//
	//   ⚠ 把出场打开写权限时特意保留了「止盈」这一项不可为 0 的隐含约束说明：
	//     实测（2026-10-02 00:40~00:55）关掉止盈后，超时平仓那一刻的盈亏纯随机，
	//     9 笔里 6 笔未止盈合计 -0.0924U —— 净值反而是负的。
	//     这里不硬拦（用户明确要能改），但页面上必须写清这个代价。
	if p.TakeProfitPct != nil {
		if err := setNumInSect(text, "exit", "take_profit_pct", *p.TakeProfitPct, "止盈%", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.StopLossPct != nil {
		if err := setNumInSect(text, "exit", "stop_loss_pct", *p.StopLossPct, "止损%", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.MaxHoldMinutes != nil {
		if err := setIntInSect(text, "exit", "max_hold_minutes", *p.MaxHoldMinutes, "超时分钟", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.MaxHoldBars != nil {
		if err := setIntInSect(text, "exit", "max_hold_bars", *p.MaxHoldBars, "超时根数", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.BollUpperExit != nil {
		if err := setBoolInSect(text, "exit", "boll_upper_exit", *p.BollUpperExit, "布林上轨出场", &text, &changed); err != nil {
			return nil, err
		}
	}

	// —— 风控 ——
	//
	//   ⚠ 这几个字段上 0 是**有意义**的（= 关掉该保护），
	//     归一化里对它们只反压负数，所以管理台写 0 能真的关掉。
	if p.AccountEquityStop != nil {
		if err := setNumInSect(text, "risk", "account_equity_stop", *p.AccountEquityStop, "权益熔断U", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.DailyLossStopPct != nil {
		if err := setNumInSect(text, "risk", "daily_loss_stop_pct", *p.DailyLossStopPct, "日亏停开仓%", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.MaxTotalMarginPct != nil {
		if err := setNumInSect(text, "risk", "max_total_margin_pct", *p.MaxTotalMarginPct, "总保证金占比%", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.MinAvailableUSDT != nil {
		if err := setNumInSect(text, "risk", "min_available_usdt", *p.MinAvailableUSDT, "可用余额下限U", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.ConsecutiveLossPause != nil {
		if err := setIntInSect(text, "risk", "consecutive_loss_pause", *p.ConsecutiveLossPause, "连亏暂停笔数", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.PauseOnAPIError != nil {
		if err := setIntInSect(text, "risk", "pause_on_api_error", *p.PauseOnAPIError, "API错误暂停次数", &text, &changed); err != nil {
			return nil, err
		}
	}

	// —— 引擎节奏 ——
	//
	//   ⚠ 归一会夹下限（exit_sec ≥ 1、entry_sec ≥ 5），
	//     所以页面填 0 不会真的把巡检停掉，只会回到下限。
	//     页面上要照实说明，避免用户以为「填 0 = 关掉扫描」。
	if p.LiveExitSec != nil {
		if err := setIntInSect(text, "live", "exit_sec", *p.LiveExitSec, "止盈巡检秒", &text, &changed); err != nil {
			return nil, err
		}
	}
	if p.LiveEntrySec != nil {
		if err := setIntInSect(text, "live", "entry_sec", *p.LiveEntrySec, "买入扫描秒", &text, &changed); err != nil {
			return nil, err
		}
	}

	if text == orig {
		return nil, fmt.Errorf("没有字段发生变化（传入的值与当前配置相同）")
	}

	// ★ 落盘前先验证：临时写一份、用真实解析器读得出来，才允许覆盖真源。
	tmp := w.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return nil, fmt.Errorf("写临时文件失败：%w", err)
	}
	if _, err := LoadStrategy(tmp); err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("写回后的配置无法解析，已放弃保存（原文件未动）：%w", err)
	}
	if err := os.Rename(tmp, w.path); err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("原子替换失败：%w", err)
	}
	// 清掉目录项缓存，确保下一次 stat 能看到新 mtime（Windows 上尤其重要，
	// 否则 strategyStore 的 mtime 比对可能认为「没变」而不重读）
	if d, err := os.Open(filepath.Dir(w.path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}

	w.logf("★ 配置已写回 %s：%s", w.path, strings.Join(changed, "；"))
	return changed, nil
}

// ---------------------------------------------------------------------------
// 定点替换
// ---------------------------------------------------------------------------

// replaceNumber 把 "key": <数字> 的值替换成 val，注释与格式原样保留。
//
// 匹配规则：行内出现 "key" 后跟冒号，再把紧跟的数字字面量换掉。
// 只替换**第一个**匹配 —— 调用方要改重名键时用 setNumInSect。
//
// ★ 只替换数字本身，不碰数字前后的任何字符 ★
//
//	FindStringSubmatchIndex 交出的是一组区间：
//	  loc[0],loc[1] — 整个匹配      （"key" + 冒号 + 空白 + 数字）
//	  loc[2],loc[3] — 第 1 个捕获组 （"key" + 冒号 + 空白）
//	  loc[4],loc[5] — 第 2 个捕获组 （**只有数字**）
//	这里必须用「最后一个捕获组」的区间做替换。
//	此前误用了 loc[2]/loc[3]（= 键名+冒号+空白），结果是：
//	  原文   "score_threshold": 2,
//	  改后   （键名整段被吃掉）+ 4 + "2,"  →  残留 "42,"
//	—— 写出来的 JSON 直接解析不了。而且因为 shape 完全没变，
//	只有「值所在行与键名不同行 / 有缩进」时才暴露，属于典型的静默写坏。
func replaceNumber(text, key string, val float64) (string, bool) {
	return replaceLiteral(text, key, trimZero(val))
}

// replaceLiteral 把 "key": <字面量> 换掉（用于 true/false/"字符串"/数字）
//
// 同上：替换区间取**最后一个捕获组**（纯字面量），键名、冒号、空白、
// 后续逗号与注释一律原样保留 —— 这是「定点替换不丢注释」的关键。
func replaceLiteral(text, key, lit string) (string, bool) {
	re := regexp.MustCompile(`("` + regexp.QuoteMeta(key) + `"\s*:\s*)(true|false|"[^"]*"|-?\d+(?:\.\d+)?(?:[eE][-+]?\d+)?)`)
	loc := re.FindStringSubmatchIndex(text)
	if loc == nil || len(loc) < 6 {
		return text, false
	}
	vs, ve := loc[4], loc[5] // 第 2 个捕获组 = 字面量本体
	return text[:vs] + lit + text[ve:], true
}

// section 找出 "name" { ... } 的字节区间 [start, end)（end 指向闭合的 } 之后）
//
// 用括号配平而不是正则：JSON 嵌套一层就会让正则失效，
// 而这份配置里 addon 段就是嵌在根对象里的一层。
func section(text, name string) (int, int, bool) {
	re := regexp.MustCompile(`"` + regexp.QuoteMeta(name) + `"\s*:\s*\{`)
	loc := re.FindStringIndex(text)
	if loc == nil {
		return 0, 0, false
	}
	open := loc[1] - 1 // 指向 '{'
	depth := 0
	inStr := false
	esc := false
	for i := open; i < len(text); i++ {
		c := text[i]
		if inStr {
			if esc {
				esc = false
				continue
			}
			switch c {
			case '\\':
				esc = true
			case '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return open + 1, i, true // 不含外层花括号
			}
		}
	}
	return 0, 0, false
}

// setNumInSect 在指定段落内替换数字键
func setNumInSect(text, sect, key string, val float64, label string, out *string, changed *[]string) error {
	s, e, ok := section(text, sect)
	if !ok {
		return fmt.Errorf("配置里找不到段落 %q", sect)
	}
	inner := text[s:e]
	ni, ok := replaceNumber(inner, key, val)
	if !ok {
		return fmt.Errorf("段落 %q 里找不到键 %q（无法写回）", sect, key)
	}
	if ni != inner {
		*changed = append(*changed, fmt.Sprintf("%s = %s", label, trimZero(val)))
	}
	*out = text[:s] + ni + text[e:]
	return nil
}

// setRootNum 替换**根级**数字键（不在任何子段落里的那一个）。
//
// ★ 为什么不能直接用 replaceNumber ★
//
//	replaceNumber 只认「第一个匹配」。而这份配置里
//	`score_threshold` 同时出现在根级（买入门槛）和 addon 段（加仓门槛）里，
//	文件里的先后顺序是 addon 在前、根级在后。
//	直接替换会改到 **addon 的门槛**，而调用方以为改的是买入门槛 ——
//	用户点「保存买入条件」，结果加仓条件被悄悄改了。
//
// 做法：先算出每一层子段落的字节区间，再要求匹配位置**落在所有子段落之外**。
// 这样根级键与段内同名键彻底分开，不依赖文件里的书写顺序。
func setRootNum(text, key string, val float64, label string, out *string, changed *[]string) error {
	vs, ve, ok := rootValueSpan(text, key)
	if !ok {
		return fmt.Errorf("在根级找不到键 %q（无法写回）", key)
	}
	old := text[vs:ve]
	ni := trimZero(val)
	if old == ni {
		return nil // 值没变，不算「已改」
	}
	*changed = append(*changed, fmt.Sprintf("%s = %s", label, ni))
	*out = text[:vs] + ni + text[ve:]
	return nil
}

// setRootInt 根级整数键
func setRootInt(text, key string, val int, label string, out *string, changed *[]string) error {
	return setRootNum(text, key, float64(val), label, out, changed)
}

// setRootBool 根级布尔键
func setRootBool(text, key string, val bool, label string, out *string, changed *[]string) error {
	vs, ve, ok := rootValueSpan(text, key)
	if !ok {
		return fmt.Errorf("在根级找不到键 %q（无法写回）", key)
	}
	lit := fmt.Sprint(val)
	if text[vs:ve] == lit {
		return nil
	}
	*changed = append(*changed, fmt.Sprintf("%s = %v", label, val))
	*out = text[:vs] + lit + text[ve:]
	return nil
}

// rootValueSpan 找**根级**键 `"key"` 的值在原文里的字节区间 [start, end)。
//
// 判定「根级」的方式：不假设键名唯一，而是遍历所有匹配，跳过任何一个
// 落在已知子段落内部的匹配。子段落列表由 rootChildSections 从原文里现场扫出来
// （不写死 addon/entry，将来加新段也不会退化）。
func rootValueSpan(text, key string) (int, int, bool) {
	spans := rootChildSections(text)
	re := regexp.MustCompile(`"` + regexp.QuoteMeta(key) + `"\s*:\s*(true|false|"[^"]*"|-?\d+(?:\.\d+)?(?:[eE][-+]?\d+)?)`)
	for _, loc := range re.FindAllStringSubmatchIndex(text, -1) {
		// loc[2],loc[3] = 整段「键:值」的值的起点？不 —— 捕获组 1 就是值本体
		vs, ve := loc[2], loc[3]
		if insideAny(spans, loc[0]) {
			continue // 落在某个子段落里 → 不是根级
		}
		return vs, ve, true
	}
	return 0, 0, false
}

// span 一个字节区间
type span struct{ start, end int }

// insideAny 位置 p 是否落在任一区间内
func insideAny(spans []span, p int) bool {
	for _, s := range spans {
		if p >= s.start && p < s.end {
			return true
		}
	}
	return false
}

// rootChildSections 扫出根对象下所有「值是对象」的键所占的区间。
//
// 实现：对每个形如 `"name" {` 的位置调用 section() 拿配平区间。
// section() 本身就靠括号配平，嵌套多深都不会错。
func rootChildSections(text string) []span {
	re := regexp.MustCompile(`"([A-Za-z_][A-Za-z0-9_]*)"\s*:\s*\{`)
	out := []span{}
	for _, loc := range re.FindAllStringSubmatchIndex(text, -1) {
		name := text[loc[2]:loc[3]]
		s, e, ok := section(text, name)
		if !ok {
			continue
		}
		// section 返回的是**不含**外层花括号的内部区间，
		// 这里把外层花括号也算进去，免得边界上的键名/key 落在门外
		keyStart := loc[0]
		out = append(out, span{start: keyStart, end: e + 1})
		_ = s
	}
	return out
}

// setIntInSect 在指定段落内替换整数键
func setIntInSect(text, sect, key string, val int, label string, out *string, changed *[]string) error {
	return setNumInSect(text, sect, key, float64(val), label, out, changed)
}

// setStrInSect 在指定段落内替换**字符串**键。
//
// ★ 为什么字符串也要走「段落内定位」而不是裸替换 ★
//
//	`mode` / `rise_bar` / `margin_policy` 这类键名在别的段落里也有可能出现，
//	而 replaceLiteral 只认**第一个**匹配 —— 裸替换一旦命中别处，
//	改错了不报错，只是行为悄悄变味。这与 setRootNum 要解决的
//	`score_threshold` 同名问题完全同源。
func setStrInSect(text, sect, key, val string, label string, out *string, changed *[]string) error {
	s, e, ok := section(text, sect)
	if !ok {
		return fmt.Errorf("配置里找不到段落 %q", sect)
	}
	inner := text[s:e]
	ni, ok := replaceLiteral(inner, key, `"`+val+`"`)
	if !ok {
		return fmt.Errorf("段落 %q 里找不到键 %q（无法写回）", sect, key)
	}
	if ni != inner {
		*changed = append(*changed, fmt.Sprintf("%s = %s", label, val))
	}
	*out = text[:s] + ni + text[e:]
	return nil
}

// setBoolInSect 在指定段落内替换布尔键
func setBoolInSect(text, sect, key string, val bool, label string, out *string, changed *[]string) error {
	s, e, ok := section(text, sect)
	if !ok {
		return fmt.Errorf("配置里找不到段落 %q", sect)
	}
	inner := text[s:e]
	ni, ok := replaceLiteral(inner, key, fmt.Sprint(val))
	if !ok {
		return fmt.Errorf("段落 %q 里找不到键 %q（无法写回）", sect, key)
	}
	if ni != inner {
		*changed = append(*changed, fmt.Sprintf("%s = %v", label, val))
	}
	*out = text[:s] + ni + text[e:]
	return nil
}
