package service

// strategy_write_allfields_test.go —— ★ 九期：管理台「全字段可热插拔」的守门人 ★
//
// 用户九期口径：
//
//	「各种开仓 限制仓位的条件都给我搞到管理员页面 可以直接修改保存热插拔
//	 还有平仓条件等 各种可以热插拔的 买卖条件都给我上
//	 包括限制开仓 买入 加仓条件也给我搞定」
//
// 于是 Patch 从 15 个字段涨到 40+。这个文件就是保证这 40+ 个字段
// **每一个都真的能写进 configs/okx_strategy.json**，
// 而不是「页面上有个输入框、填了没反应」。
//
// ★ 为什么必须逐个字段断言 ★
//
//	本项目头号故障形态是「改了没用」。而写回这条链上有三个容易漏的环节，
//	每一个单独看都「看起来对」，合起来才表现为「保存成功但值没变」：
//
//	  ① ParsePatch 没解析这个键 → 字段永远是 nil → Empty() 判成「没要改的」
//	     → 接口直接报「没有任何要修改的字段」，用户以为自己没保存上
//	  ② Apply 里漏写这个键     → 别的字段都改了，就它没动，**而且不报错**
//	  ③ 段落定位错了           → 改到同名的另一个段
//	     （`score_threshold` 根级 vs addon 段、`margin_usdt` entry 段 vs addon 段）
//
//	所以这里一次性把全部字段塞进一个 Patch，再用**真实解析器**读回来逐条比对 ——
//	只有「读回来的值等于我要写的值」才算真的通。

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// 测试用的取值小工具（短名，避免与同包其它测试的 helper 撞名）
func ptF(v float64) *float64 { return &v }
func ptI(v int) *int         { return &v }
func ptB(v bool) *bool       { return &v }
func ptS(v string) *string   { return &v }

// realStrategyJSON 拿真源配置的副本。
//
// ★ 刻意用真源而不是手写一份精简 JSON ★
//   真源里 `score_threshold` / `margin_usdt` / `enabled` / `mode` 这些键
//   **跨段落同名**，而「段落定位」这一整套机制（setRootNum / setNumInSect）
//   就是为它们写的。拿一份字段齐全、结构一模一样的真源来测，
//   才能真的把「改错段」这个风险暴露出来。
//   写一份简化 JSON 测，恰恰会把最容易出错的那部分绕过去。
func realStrategyJSON(t *testing.T) []byte {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "configs", "okx_strategy.json"))
	if err != nil {
		t.Skipf("读不到真源配置（可能被 .gitignore 排除），跳过：%v", err)
	}
	return raw
}

// TestApply_AllEditableFieldsRoundTrip 全部可编辑字段一次性写回 → 读回 → 逐条比对。
func TestApply_AllEditableFieldsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "okx_strategy.json")
	if err := os.WriteFile(path, realStrategyJSON(t), 0o644); err != nil {
		t.Fatal(err)
	}

	w := NewStrategyWriter(path, func(string, ...any) {}) // 静音日志，测试输出才干净

	// —— 一次性改完所有可编辑字段 ——
	//   取值刻意挑「不会触发归一化反压」的：比如 cooldown_bars 用 42 而不是负数，
	//   live.exit_sec 用 5（下限是 1）——否则测出来的是归一化行为，不是写回行为。
	p := &Patch{
		BuyEnabled: ptB(false),
		DryRun:     ptB(true),

		ScoreThreshold: ptI(5),
		MinBarRisePct:  ptF(-1.5),
		BuyMarginUSDT:  ptF(0.25),
		MaxMarginUSDT:  ptF(2.5),
		Leverage:       ptI(10),
		MarginPolicy:   ptS("fixed"),

		CooldownBars:           ptI(42), // ★ 九期核心字段
		MaxConcurrentPositions: ptI(7),
		DailyMaxEntries:        ptI(13),

		MaxOrderMarginUSDT:    ptF(3.5),
		ExcludeNewListingDays: ptI(45),
		ExcludeDelisting:      ptB(false),
		ExcludeStockETF:       ptB(true),
		MinQuoteVolume24h:     ptF(2000000),
		TopNByVolume:          ptI(120),

		AddonMode:       ptS("price"),
		AddonEnabled:    ptB(false),
		AddonScore:      ptI(4),
		AddonBarRise:    ptF(0.9),
		AddonDropPct:    ptF(1.8),
		AddonRisePct:    ptF(1.4),
		AddonMarginU:    ptF(0.2),
		AddonRatio:      ptF(0.5),
		AddonMaxTimes:   ptI(3),
		AddonMinGapBars: ptI(5),
		AddonRiseBar:    ptS("15m"),

		TakeProfitPct:  ptF(0.8),
		StopLossPct:    ptF(250),
		MaxHoldMinutes: ptI(720),
		MaxHoldBars:    ptI(0),
		BollUpperExit:  ptB(true),

		AccountEquityStop:    ptF(0.5),
		DailyLossStopPct:     ptF(30),
		MaxTotalMarginPct:    ptF(80),
		MinAvailableUSDT:     ptF(0.2),
		ConsecutiveLossPause: ptI(3),
		PauseOnAPIError:      ptI(7),

		LiveExitSec:  ptI(5),
		LiveEntrySec: ptI(120),
	}

	// 判空这一步先过 —— 它自己就是一个易漏点（新字段没登记就永远判「空」）
	if p.Empty() {
		t.Fatal("Patch.Empty() 判成了「没有要改的字段」—— 说明有字段没在反射可及的指针里登记")
	}

	changed, err := w.Apply(p)
	if err != nil {
		t.Fatalf("Apply 失败：%v", err)
	}
	if len(changed) == 0 {
		t.Fatal("Apply 报告「没有任何变化」—— 40+ 个字段一个都没写进去")
	}

	// —— 用真实解析器读回 ——
	got, err := LoadStrategy(path)
	if err != nil {
		t.Fatalf("写回后的配置解析失败（写出来的 JSON 已经坏了）：%v", err)
	}

	// —— 逐条比对 ——
	//   注意 float 用精确相等：这些都是「字面量原样搬过去」的值，
	//   中间没有任何运算，出现误差就说明替换逻辑动了不该动的东西。
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"根级 enabled", got.Enabled, false},
		{"根级 dry_run", got.DryRun, true},

		{"根级 score_threshold", got.ScoreThreshold, 5},
		{"entry min_bar_rise_pct", got.MinBarRisePct(), -1.5},
		{"entry margin_usdt", got.Entry.MarginUSDT, 0.25},
		{"entry max_margin_usdt", got.Entry.MaxMarginUSDT, 2.5},
		{"entry leverage", got.Entry.Leverage, 10},
		{"entry margin_policy", got.Entry.MarginPolicy, "fixed"},

		{"★ entry cooldown_bars", got.Entry.CooldownBars, 42},
		{"entry max_concurrent_positions", got.Entry.MaxConcurrentPositions, 7},
		{"entry daily_max_entries", got.Entry.DailyMaxEntries, 13},

		{"根级 max_order_margin_usdt", got.MaxOrderMarginUSDT, 3.5},
		{"根级 exclude_new_listing_days", got.ExcludeNewListingDays, 45},
		{"根级 exclude_delisting", got.ExcludeDelisting, false},
		{"根级 exclude_stock_etf", got.ExcludeStockETF, true},
		{"根级 min_quote_volume_24h", got.MinQuoteVolume24h, 2000000.0},
		{"根级 top_n_by_volume", got.TopNByVolume, 120},

		{"addon mode", got.Addon.Mode, "price"},
		{"addon enabled", got.Addon.Enabled, false},
		{"★ addon score_threshold（不能改到根级那个）", got.Addon.ScoreThres, 4},
		{"addon bar_rise_pct", got.Addon.BarRisePct, 0.9},
		{"addon drop_pct", got.Addon.DropPct, 1.8},
		{"addon price_rise_pct", got.Addon.PriceRise, 1.4},
		{"★ addon margin_usdt（不能改到 entry 那个）", got.Addon.MarginUSDT, 0.2},
		{"addon ratio", got.Addon.Ratio, 0.5},
		{"addon max_times", got.Addon.MaxTimes, 3},
		{"★ addon min_gap_bars", got.Addon.MinGapBars, 5},
		{"addon rise_bar", got.Addon.RiseBar, "15m"},

		{"exit take_profit_pct", got.Exit.TakeProfitPct, 0.8},
		{"exit stop_loss_pct", got.Exit.StopLossPct, 250.0},
		{"exit max_hold_minutes", got.Exit.MaxHoldMinutes, 720},
		{"exit max_hold_bars", got.Exit.MaxHoldBars, 0},
		{"exit boll_upper_exit", got.Exit.BollUpperExit, true},

		{"risk account_equity_stop", got.Risk.AccountEquityStop, 0.5},
		{"risk daily_loss_stop_pct", got.Risk.DailyLossStopPct, 30.0},
		{"risk max_total_margin_pct", got.Risk.MaxTotalMarginPct, 80.0},
		{"risk min_available_usdt", got.Risk.MinAvailableUSDT, 0.2},
		{"risk consecutive_loss_pause", got.Risk.ConsecutiveLossPause, 3},
		{"risk pause_on_api_error", got.Risk.PauseOnAPIError, 7},

		{"live exit_sec", got.Live.ExitSec, 5},
		{"live entry_sec", got.Live.EntrySec, 120},
	}

	bad := 0
	for _, c := range checks {
		if c.got != c.want {
			bad++
			t.Errorf("✗ %s：写回后读回的是 %v（%T），期望 %v（%T）",
				c.name, c.got, c.got, c.want, c.want)
		}
	}
	if bad == 0 {
		t.Logf("✓ %d 个可编辑字段全部写回成功并被真实解析器读回一致（含 cooldown_bars=42）", len(checks))
	}

	// —— 注释必须一字不丢（定点替换存在的全部意义）——
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, keep := range []string{
		"//",                             // 至少还有注释
		"OKX 全合约 8 因子共振策略",         // 文件头
		"★★",                             // 九期 mark 的强调标记
	} {
		if !strings.Contains(text, keep) {
			t.Errorf("写回后丢了关键内容 %q —— 定点替换退化成了整份 Marshal 重写", keep)
		}
	}
	// 注释行数不该变少（允许增加，因为我们改的是值不是注释）
	origRaw := realStrategyJSON(t)
	if n, m := strings.Count(text, "//"), strings.Count(string(origRaw), "//"); n < m {
		t.Errorf("写回后注释行从 %d 掉到 %d —— 注释被吃掉了", m, n)
	}

	// —— 幂等：同样的 Patch 再写一次，必须明确报「没有字段发生变化」——
	//
	//   这条保护的是「值没变不算改」这个语义：它让前端能区分
	//   「保存成功」与「你填的就是当前值」，而不是每次都回一串假的 changed。
	if _, err := w.Apply(p); err == nil {
		t.Fatal("第二次写同一个 Patch 竟然成功了 —— 应当报「没有字段发生变化」")
	}
}

// TestPatch_AllFieldsArePointers Patch 的字段必须**全是指针**。
//
// Empty() 用反射只看指针字段。一旦有人加了非指针字段，那个字段会
// **永远不参与判空** —— 表现就是「只改它一个」时接口报「没有要修改的字段」，
// 而用户明明在页面上填了。这条断言把这个约定钉死。
func TestPatch_AllFieldsArePointers(t *testing.T) {
	rt := reflect.TypeOf(Patch{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if f.Type.Kind() != reflect.Ptr {
			t.Errorf("Patch.%s 的类型是 %s —— 必须是指针（nil = 本次不改），否则 Empty() 会漏掉它",
				f.Name, f.Type)
		}
	}
	t.Logf("✓ Patch 的 %d 个字段全是指针，Empty() 的反射遍历不会漏项", rt.NumField())
}

// TestPatch_JSONTagsUnique 每个字段都要有**非空且不重复**的 json tag。
//
// 重复的 tag 会让两个字段抢同一个入参键 —— 前端传一个值，
// 后端两个字段都非 nil，最后哪一个生效取决于 Apply 里的书写顺序。
// 这种「看代码才能知道哪个赢」的语义，正是要避免的。
func TestPatch_JSONTagsUnique(t *testing.T) {
	rt := reflect.TypeOf(Patch{})
	seen := map[string]string{}
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		if tag == "" || tag == "-" {
			t.Errorf("Patch.%s 缺 json tag（前端就没法给它传值）", f.Name)
			continue
		}
		if prev, dup := seen[tag]; dup {
			t.Errorf("json tag %q 被 Patch.%s 与 Patch.%s 同时占用", tag, prev, f.Name)
		}
		seen[tag] = f.Name
	}
	t.Logf("✓ %d 个 json tag 互不重复", len(seen))
}
