package service

// strategy_write_test.go —— 管理台「保存并生效」的写回链路实测
//
// ★ 2026-10-02 八期 ★
//   管理台保存 = 读真源 JSON → 定点替换 → 校验可解析 → 原子覆盖 → 热重载。
//   这条链路上任何一环出问题，用户看到的现象都是**一模一样**的
//   「我点了保存，但策略没变」——
//   而真正的原因可能差得极远（没找到键 / 写坏了 JSON / 段落定位错 / 值被归一化吞掉）。
//   所以这里把每一环都单独钉住。
//
// 覆盖：
//   A. 注释保留 —— 写回后原注释一字不少（这是不用 json.Marshal 的理由）
//   B. 段落定位 —— entry 与 addon 里**同名键**（margin_usdt/ratio/max_times）
//      必须各改各的，不能串台
//   C. 带符号值 —— min_bar_rise_pct 写负数必须原样落盘（六期最贵的一个坑）
//   D. 显式 0 —— 写 0 不能被当成「没填」丢掉
//   E. 写坏不许落盘 —— 校验失败时原文件必须一字未动
//   F. 无变化要报错 —— 而不是假报「保存成功」
//   G. 原子性 —— 不留 .tmp 残骸
//   H. 模式校验 —— 只收 resonance / price
//   I. ParsePatch —— 类型错要报错，不认识的键要忽略

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"finally-main/internal/conf"
)

// wSrcJSON 一份与真源**结构完全一致**的配置样本：
// 注释、嵌套段落、entry/addon 同名键（margin_usdt / ratio / max_times），
// 以及**根级与 addon 段都叫 score_threshold** —— 这一条最关键，
// 它是 setIntInSect（段落定位）存在的唯一理由，样本必须照抄真源形状，
// 否则测出来的「不串台」是假的。
//
// 注意：下面的文件写成**无行内注释的规整 JSON**（缩进 2 空格），
// 因为写回是「定点替换」—— 它只会替换值，不会动注释，
// 所以样本里有注释反而会让「逐行比对」这种断言把噪点算成失败。
// 注释保留能力由 TestWrite_KeepsComments 用带注释的样本单独验证。
const wSrcJSON = `{
  "enabled": true,
  "entry": {
    "margin_usdt": 0.1,
    "max_margin_usdt": 1.0,
    "min_bar_rise_pct": -0.7
  },
  "addon": {
    "enabled": true,
    "mode": "resonance",
    "score_threshold": 2,
    "bar_rise_pct": 1.0,
    "price_rise_pct": 1.0,
    "drop_pct": 1.0,
    "margin_usdt": 0,
    "ratio": 0.3333333333,
    "max_times": 0
  },
  "score_threshold": 3
}
`

// wSrcCommented 带注释的样本：专用于验证「注释一字不少」。
// 注释行内不出现数字赋值，避免干扰「值替换」相关断言。
const wSrcCommented = `{
  "enabled": true,                  // 策略总开关
  "entry": {
    "margin_usdt": 0.1,             // 每笔买入保证金（min_one）
    "max_margin_usdt": 1.0,         // 单笔硬上限
    "min_bar_rise_pct": -0.7        // 六期：必须真跌 0.7%
  },
  "addon": {
    "enabled": true,
    "mode": "resonance",            // resonance | price
    "score_threshold": 2,
    "bar_rise_pct": 1.0,
    "price_rise_pct": 1.0,
    "drop_pct": 1.0,
    "margin_usdt": 0,               // 0 = 用 ratio
    "ratio": 0.3333333333,
    "max_times": 0                  // 0 = 不限
  },
  "score_threshold": 3              // 买入分数门槛
}
`

// mkWriter 造一个临时配置文件 + writer，返回 (writer, 路径)
func mkWriter(t *testing.T) (*StrategyWriter, string) {
	t.Helper()
	return mkWriterWith(t, wSrcJSON)
}

// mkWriterWith 用指定原文造临时配置
func mkWriterWith(t *testing.T, src string) (*StrategyWriter, string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "okx_strategy.json")
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatalf("准备临时配置失败：%v", err)
	}
	return NewStrategyWriter(p, nil), p
}

// readBack 读回文件内容
func readBack(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读回配置失败：%v", err)
	}
	return string(b)
}

// iptr / fptr / bptr / sptr 造指针（Patch 全是指针语义）
func iptr(v int) *int          { return &v }
func fptr(v float64) *float64  { return &v }
func bptr(v bool) *bool        { return &v }
func sptr(v string) *string    { return &v }

// ---------------------------------------------------------------------------
// A. 注释保留
// ---------------------------------------------------------------------------

// TestWrite_KeepsComments 写回后注释必须一字不少。
//
// 这条测试是「为什么不用 json.Marshal」的**可执行证据**：
// 真源 JSON 的注释就是策略口径文档（「六期：必须真跌 0.7%」这种），
// 一次 Marshal 全丢，用户的策略说明书就被清空了。
func TestWrite_KeepsComments(t *testing.T) {
	w, p := mkWriterWith(t, wSrcCommented)

	// 逐行比对：写前写后行数必须一致，且只有目标那一行变化
	beforeLines := strings.Split(wSrcCommented, "\n")
	// 目标行 = 根级 "score_threshold": 3（缩进 2 空格，在文件末尾）
	target := -1
	for i, l := range beforeLines {
		if strings.HasPrefix(l, `  "score_threshold": 3`) {
			target = i
		}
	}
	if target < 0 {
		t.Fatal("样本里找不到根级 score_threshold 行")
	}

	changed, err := w.Apply(&Patch{ScoreThreshold: iptr(4)})
	if err != nil {
		t.Fatalf("写回失败：%v", err)
	}
	afterLines := strings.Split(readBack(t, p), "\n")

	if len(afterLines) != len(beforeLines) {
		t.Fatalf("写回改变了文件行数：%d → %d（说明整体重写了，不是定点替换）",
			len(beforeLines), len(afterLines))
	}
	// 只有 target 那一行允许变，其余每一行必须逐字节相同（注释、缩进、对齐空格全保）
	diff := 0
	for i := range beforeLines {
		if beforeLines[i] != afterLines[i] {
			diff++
			if i != target {
				t.Fatalf("第 %d 行不该变却变了：\n  写前 %q\n  写后 %q", i+1, beforeLines[i], afterLines[i])
			}
		}
	}
	if diff != 1 {
		t.Fatalf("应当只有 1 行发生变化，实际 %d 行", diff)
	}
	if !strings.Contains(afterLines[target], `"score_threshold": 4`) {
		t.Fatalf("目标行没改成 4：%q", afterLines[target])
	}
	// 注释行逐条点名（防「行数对但注释被清成空行」）
	joined := strings.Join(afterLines, "\n")
	for _, c := range []string{
		"// 策略总开关",
		"// 每笔买入保证金（min_one）",
		"// 六期：必须真跌 0.7%",
		"// resonance | price",
		"// 0 = 不限",
		"// 买入分数门槛",
	} {
		if !strings.Contains(joined, c) {
			t.Fatalf("写回后丢了注释 %q", c)
		}
	}
	t.Logf("✓ 定点替换：%d 行文件只有 1 行变化，注释/缩进/对齐全保 —— %v",
		len(beforeLines), changed)
}

// ---------------------------------------------------------------------------
// B. 段落定位：同名键各改各的
// ---------------------------------------------------------------------------

// TestWrite_SectionIsolation 同名键不串台。
//
// entry.margin_usdt / addon.margin_usdt / entry.max_margin_usdt 名字相近，
// 而 replaceNumber 只认「第一个匹配」—— 若不加段落约束，
// 管理工作台里改「加仓金额」会把「买入金额」一起改掉，属于会直接亏钱的串台。
func TestWrite_SectionIsolation(t *testing.T) {
	w, p := mkWriter(t)

	_, err := w.Apply(&Patch{
		BuyMarginUSDT: fptr(0.2),  // entry.margin_usdt
		AddonMarginU:  fptr(0.5),  // addon.margin_usdt
		MaxMarginUSDT: fptr(2.0),  // entry.max_margin_usdt
		AddonRatio:    fptr(0.25), // addon.ratio
		AddonMaxTimes: iptr(7),    // addon.max_times
	})
	if err != nil {
		t.Fatalf("写回失败：%v", err)
	}
	after := readBack(t, p)

	eTxt := sectText(t, after, "entry")
	aTxt := sectText(t, after, "addon")

	checks := []struct {
		name, hay, want string
	}{
		{"entry.margin_usdt", eTxt, `"margin_usdt": 0.2`},
		{"entry.max_margin_usdt", eTxt, `"max_margin_usdt": 2`},
		{"entry.min_bar_rise_pct 不该被动", eTxt, `"min_bar_rise_pct": -0.7`},
		{"addon.margin_usdt", aTxt, `"margin_usdt": 0.5`},
		{"addon.ratio", aTxt, `"ratio": 0.25`},
		{"addon.max_times", aTxt, `"max_times": 7`},
		{"addon 的分数键不该被动", aTxt, `"score_threshold": 2`},
		{"根级 score_threshold 不该被动", after, `"score_threshold": 3`},
	}
	for _, c := range checks {
		if !strings.Contains(c.hay, c.want) {
			t.Fatalf("段内断言失败 [%s]：期望 %q\n--- entry ---\n%s\n--- addon ---\n%s",
				c.name, c.want, eTxt, aTxt)
		}
	}
	t.Log("✓ entry / addon 同名键各改各的，未串台")
}

// TestWrite_SameKeyAcrossSections 真正重名的场景：根级与 addon 段都叫
// `score_threshold`，必须只改 addon 那一个。
//
// 用一个最小样本单独钉住 —— 因为上面那份主样本为了让断言能落到具体段落，
// 把加仓的那个键改成了独立名。重名才是真源里的实际形状，
// 而 `setIntInSect`（段落定位）正是为它存在的：
// 若退化成 `replaceNumber`（只认第一个匹配），根级的买入门槛会被一起改掉。
func TestWrite_SameKeyAcrossSections(t *testing.T) {
	src := `{
  "score_threshold": 3,
  "entry": { "margin_usdt": 0.1 },
  "addon": {
    "score_threshold": 2,
    "ratio": 0.3333333333
  }
}
`
	w, p := mkWriterWith(t, src)

	if _, err := w.Apply(&Patch{AddonScore: iptr(6)}); err != nil {
		t.Fatalf("写回失败：%v", err)
	}
	after := readBack(t, p)

	root := after[strings.Index(after, `"score_threshold"`):]
	e := sectText(t, after, "addon")

	if !strings.Contains(root, `"score_threshold": 3`) {
		t.Fatalf("根级（买入）score_threshold 被误改了 —— 段落定位失效\n%s", after)
	}
	if !strings.Contains(e, `"score_threshold": 6`) {
		t.Fatalf("addon 段 score_threshold 没改成 6\n%s", after)
	}

	// 反过来也一样：改买入门槛不能动 addon 的
	if _, err := w.Apply(&Patch{ScoreThreshold: iptr(5)}); err != nil {
		t.Fatalf("第二次写回失败：%v", err)
	}
	after2 := readBack(t, p)
	if !strings.Contains(after2[strings.Index(after2, `"score_threshold"`):], `"score_threshold": 5`) {
		t.Fatalf("根级 score_threshold 没改成 5\n%s", after2)
	}
	if !strings.Contains(sectText(t, after2, "addon"), `"score_threshold": 6`) {
		t.Fatalf("改买入门槛时把 addon 的分数门槛也改了（串台）\n%s", after2)
	}
	t.Log("✓ 根级与 addon 段同名 score_threshold 互不干扰（段落定位真实有效）")
}

// sectText 取出 "name" { ... } 的段落原文（含外层键名，便于断言）
func sectText(t *testing.T, text, name string) string {
	t.Helper()
	s, e, ok := section(text, name)
	if !ok {
		t.Fatalf("配置里找不到段落 %q", name)
	}
	// section 返回的是**不含**外层花括号的内部区间 [s,e)，
	// 这里往前找到键名起点，往后补上闭合括号，得到完整段落文本
	keyAt := strings.LastIndex(text[:s], `"`+name+`"`)
	if keyAt < 0 {
		t.Fatalf("定位段落 %q 的键名失败", name)
	}
	return text[keyAt : e+1]
}

// ---------------------------------------------------------------------------
// C. 带符号值（六期最贵的坑）
// ---------------------------------------------------------------------------

// TestWrite_SignedValueSurvives 负数门槛必须原样落盘。
//
// 六期的核心口径是「min_bar_rise_pct < 0 表示必须真跌」。历史上这里被
// 「if X <= 0 { X = 默认 }」的归一化反压过一次，导致「改了没用」。
// 写回链路必须保证负数**原样**写进文件，一个符号都不能丢。
func TestWrite_SignedValueSurvives(t *testing.T) {
	w, p := mkWriter(t)

	cases := []float64{-0.5, -1.5, -0.01}
	for _, v := range cases {
		// 每次从干净样本重来，保证「改成不同的值」而不是无变化
		if err := os.WriteFile(p, []byte(wSrcJSON), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Apply(&Patch{MinBarRisePct: fptr(v)}); err != nil {
			t.Fatalf("写 %v 失败：%v", v, err)
		}
		after := readBack(t, p)
		want := `"min_bar_rise_pct": ` + trimZero(v)
		// 必须落在 entry 段里（不能改到别处同名键）
		eS, eE, ok := section(after, "entry")
		if !ok {
			t.Fatal("写回后 entry 段没了")
		}
		if !strings.Contains(after[eS:eE], want) {
			t.Fatalf("entry 段里找不到 %q\n%s", want, after)
		}
		// 再确认读回来的配置里也是负数（写对了但被解析器归一化掉，同样算失败）
		cfg, err := LoadStrategy(p)
		if err != nil {
			t.Fatalf("读回失败：%v", err)
		}
		if cfg.Entry.MinBarRisePct == nil {
			t.Fatalf("读回后 MinBarRisePct 为 nil（写 %v 丢了）", v)
		}
		if *cfg.Entry.MinBarRisePct != v {
			t.Fatalf("负数被吞：写 %v，读回 %v", v, *cfg.Entry.MinBarRisePct)
		}
	}
	t.Log("✓ min_bar_rise_pct 的负号原样落盘且解析后仍是负数（六期口径未被反压）")
}

// TestWrite_SignedValueRoundTripIsStable 写负数后再写回同样的负数应当报「没变化」，
// 而不是产生 -0.7 → -0.7000000001 这种漂移。
func TestWrite_SignedValueRoundTripIsStable(t *testing.T) {
	w, p := mkWriter(t)
	if _, err := w.Apply(&Patch{MinBarRisePct: fptr(-0.5)}); err != nil {
		t.Fatalf("第一次写失败：%v", err)
	}
	if _, err := w.Apply(&Patch{MinBarRisePct: fptr(-0.5)}); err == nil {
		t.Fatal("同样的值再写一次应当报「没有字段发生变化」，说明产生了数值漂移")
	} else if !strings.Contains(err.Error(), "没有字段发生变化") {
		t.Fatalf("期望「没有字段发生变化」，实际：%v", err)
	}
	if got := readBack(t, p); !strings.Contains(got, `"min_bar_rise_pct": -0.5`) {
		t.Fatalf("重复写导致值漂移\n%s", got)
	}
	t.Log("✓ 重复写同一个负数不产生漂移，第二次正确报「无变化」")
}

// ---------------------------------------------------------------------------
// D. 显式 0
// ---------------------------------------------------------------------------

// TestWrite_ExplicitZeroIsWritten 显式写 0 必须真落盘。
//
// 「0 = 关闭该条件」是本项目明确支持的语义（见 Patch 注释）。
// 若把 0 当成「没填」丢掉，用户就会遇到「我把条件关了，但它还在拦我」。
func TestWrite_ExplicitZeroIsWritten(t *testing.T) {
	w, p := mkWriter(t)

	_, err := w.Apply(&Patch{
		MinBarRisePct: fptr(0), // 关闭「必须真跌」
		AddonScore:    iptr(0), // 加仓分数跟随买入
		AddonMaxTimes: iptr(0), // 不限次数
	})
	if err != nil {
		t.Fatalf("写回失败：%v", err)
	}
	after := readBack(t, p)

	if !strings.Contains(after, `"min_bar_rise_pct": 0`) {
		t.Fatalf("min_bar_rise_pct 写 0 没落盘\n%s", after)
	}
	cfg, err := LoadStrategy(p)
	if err != nil {
		t.Fatalf("读回失败：%v", err)
	}
	if cfg.Entry.MinBarRisePct == nil || *cfg.Entry.MinBarRisePct != 0 {
		t.Fatalf("读回后不是 0（0 被当成「没填」丢了）：%v", cfg.Entry.MinBarRisePct)
	}
	if cfg.Addon.MaxTimes != 0 {
		t.Fatalf("max_times 写 0（不限）没生效，读回 %d", cfg.Addon.MaxTimes)
	}
	t.Log("✓ 显式 0 原样落盘且读回仍是 0（「关闭条件 / 不限次数」语义成立）")
}

// ---------------------------------------------------------------------------
// E. 写坏了不许落盘
// ---------------------------------------------------------------------------

// TestWrite_NoKeyLeavesFileIntact 找不到键时报错，且原文件一字未动。
func TestWrite_NoKeyLeavesFileIntact(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "okx_strategy.json")
	// 故意缺少 min_bar_rise_pct 与 addon 段
	broken := `{ "enabled": true, "entry": { "margin_usdt": 0.1 }, "score_threshold": 3 }`
	if err := os.WriteFile(p, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	w := NewStrategyWriter(p, nil)

	if _, err := w.Apply(&Patch{MinBarRisePct: fptr(-0.7)}); err == nil {
		t.Fatal("配置里没有该键时应当报错，而不是静默成功")
	}
	if got := readBack(t, p); got != broken {
		t.Fatalf("报错路径下原文件被改了：\n--- 期望 ---\n%s\n--- 实际 ---\n%s", broken, got)
	}
	t.Log("✓ 缺键时报错且原文件保持原样")
}

// TestWrite_NoTempLeftBehind 成功与否都不留 .tmp 残骸。
func TestWrite_NoTempLeftBehind(t *testing.T) {
	w, p := mkWriter(t)

	if _, err := w.Apply(&Patch{ScoreThreshold: iptr(5)}); err != nil {
		t.Fatalf("写回失败：%v", err)
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("成功写回后残留了 %s.tmp（下次写会读到脏数据）", p)
	}
	// 失败路径同样不该留
	if _, err := w.Apply(&Patch{ScoreThreshold: iptr(5)}); err == nil {
		t.Fatal("重复写同值应当报错")
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("失败写回后残留了 %s.tmp", p)
	}
	t.Log("✓ 成功/失败路径都不留 .tmp 残骸")
}

// ---------------------------------------------------------------------------
// F. 无变化要报错
// ---------------------------------------------------------------------------

// TestWrite_NoChangeIsError 「值没变」必须报错，不能假报成功。
//
// 用户点保存 → 看到「✓ 已保存并生效」→ 但配置文件根本没动。
// 这种假成功是本项目最忌讳的反馈形态（用户会以为策略已经改了）。
func TestWrite_NoChangeIsError(t *testing.T) {
	w, _ := mkWriter(t)
	// 原文里 score_threshold 就是 3
	if _, err := w.Apply(&Patch{ScoreThreshold: iptr(3)}); err == nil {
		t.Fatal("传入与当前配置相同的值应当报错，而不是谎报保存成功")
	} else if !strings.Contains(err.Error(), "没有字段发生变化") {
		t.Fatalf("错误信息应说明「没有字段发生变化」，实际：%v", err)
	}
	t.Log("✓ 无变化时报错（不假报保存成功）")
}

// ---------------------------------------------------------------------------
// G/H. 模式校验
// ---------------------------------------------------------------------------

// TestWrite_AddonModeValidation 模式只收 resonance / price，且大小写宽容。
func TestWrite_AddonModeValidation(t *testing.T) {
	w, p := mkWriter(t)

	// 非法模式 → 拒绝，且文件不动
	before := readBack(t, p)
	if _, err := w.Apply(&Patch{AddonMode: sptr("banana")}); err == nil {
		t.Fatal("非法加仓模式应当被拒绝")
	}
	if got := readBack(t, p); got != before {
		t.Fatal("拒绝路径下文件被改了")
	}

	// 合法模式 + 大小写宽容
	for _, m := range []string{"price", "PRICE", " Price "} {
		if err := os.WriteFile(p, []byte(wSrcJSON), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Apply(&Patch{AddonMode: sptr(m)}); err != nil {
			t.Fatalf("模式 %q 应当被接受：%v", m, err)
		}
		got := readBack(t, p)
		if !strings.Contains(got, `"mode": "price"`) {
			t.Fatalf("模式 %q 应当规范化成 price 落盘\n%s", m, got)
		}
	}
	t.Log("✓ 加仓模式校验：只收 resonance/price，大小写与空白宽容")
}

// TestWrite_AddonModeRoundTrip 写 price 后读回的 Mode 就是 price。
//
// 这条直接对应「界面上切了模式，引擎到底跑哪套」——写对不等于读对。
func TestWrite_AddonModeRoundTrip(t *testing.T) {
	w, p := mkWriter(t)
	if _, err := w.Apply(&Patch{AddonMode: sptr("price")}); err != nil {
		t.Fatalf("写模式失败：%v", err)
	}
	cfg, err := LoadStrategy(p)
	if err != nil {
		t.Fatalf("读回失败：%v", err)
	}
	if cfg.Addon.Mode != "price" {
		t.Fatalf("写 price 读回 %q", cfg.Addon.Mode)
	}
	t.Log("✓ addon.mode 写完读回一致（界面切模式能真正落到引擎）")
}

// ---------------------------------------------------------------------------
// I. ParsePatch
// ---------------------------------------------------------------------------

// TestParsePatch_RejectsBadType 类型不对必须报错。
//
// 静默忽略错类型 = 「改了没用」的第二次上演：前端传错、后端当没看见，
// 用户点了保存、看到成功、策略没动。
func TestParsePatch_RejectsBadType(t *testing.T) {
	bad := []map[string]any{
		{"score_threshold": "abc"},      // 不是数字
		{"buy_enabled": "yes"},          // 不是布尔
		{"addon_mode": 123},             // 不是字符串
		{"min_bar_rise_pct": map[string]any{}}, // 不是标量
	}
	for i, b := range bad {
		if _, err := ParsePatch(b); err == nil {
			t.Fatalf("第 %d 个非法入参应当报错：%v", i+1, b)
		}
	}
	t.Log("✓ ParsePatch 对错类型全部报错（不静默忽略）")
}

// TestParsePatch_IgnoresUnknownKeys 不认识的键直接忽略。
//
// 前端多带一个字段不该 500 —— 尤其管理台还会同时兼容新旧版本。
func TestParsePatch_IgnoresUnknownKeys(t *testing.T) {
	p, err := ParsePatch(map[string]any{
		"score_threshold": float64(4),
		"future_field":    "whatever", // 未来版本才有的键
		"__v":             float64(2),
	})
	if err != nil {
		t.Fatalf("不认识的键应当被忽略，不该报错：%v", err)
	}
	if p.ScoreThreshold == nil || *p.ScoreThreshold != 4 {
		t.Fatalf("score_threshold 没解析出来：%+v", p)
	}
	t.Log("✓ 未知键忽略，已知键正常解析")
}

// TestParsePatch_EmptyIsError 空 patch 报错（避免一次无意义的写盘/热重载）。
func TestParsePatch_EmptyIsError(t *testing.T) {
	if _, err := ParsePatch(map[string]any{}); err == nil {
		t.Fatal("空 patch 应当报错")
	}
	// 有键但值是 nil 也算空
	if _, err := ParsePatch(map[string]any{"score_threshold": nil}); err == nil {
		t.Fatal("值为 nil 等同没传，应当算空 patch")
	}
	t.Log("✓ 空 patch 报错")
}

// TestParsePatch_NumbersFromJSON 走一遍真实 JSON 解析路径（UseNumber=false 时是 float64）。
func TestParsePatch_NumbersFromJSON(t *testing.T) {
	p, err := ParsePatch(map[string]any{
		"score_threshold":  float64(5),
		"min_bar_rise_pct": float64(-0.7),
		"buy_margin_usdt":  float64(0.1),
		"max_margin_usdt":  float64(1),
		"addon_mode":       "resonance",
		"addon_enabled":    true,
	})
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if *p.ScoreThreshold != 5 {
		t.Fatalf("score_threshold 解析错：%d", *p.ScoreThreshold)
	}
	if *p.MinBarRisePct != -0.7 {
		t.Fatalf("min_bar_rise_pct 解析错：%v", *p.MinBarRisePct)
	}
	if *p.BuyMarginUSDT != 0.1 || *p.MaxMarginUSDT != 1 {
		t.Fatalf("金额解析错：%v / %v", *p.BuyMarginUSDT, *p.MaxMarginUSDT)
	}
	if *p.AddonMode != "resonance" || !*p.AddonEnabled {
		t.Fatalf("加仓字段解析错：%v / %v", p.AddonMode, p.AddonEnabled)
	}
	t.Log("✓ 真实 JSON 数字/布尔/字符串解析正确")
}

// ---------------------------------------------------------------------------
// 组合：模拟一次真实的管理台保存
// ---------------------------------------------------------------------------

// TestWrite_RealisticAdminSave 模拟用户在对话框里勾选后点保存的全套 patch，
// 逐项核对落盘结果 —— 这是管理台「保存并生效」的端到端形状测试。
func TestWrite_RealisticAdminSave(t *testing.T) {
	w, p := mkWriter(t)

	// 用户八期口径：买入 score>2 且 min_bar_rise_pct<-0.7；加仓走共振 score>2 且涨幅>0.7
	patch := &Patch{
		BuyEnabled:     bptr(true),
		ScoreThreshold: iptr(4), // 根级（买入）：用 4 而不是样本里已有的 3，明确是「改」
		MinBarRisePct:  fptr(-0.5),
		BuyMarginUSDT:  fptr(0.1),
		MaxMarginUSDT:  fptr(1.0),
		AddonMode:      sptr("resonance"),
		AddonEnabled:   bptr(true),
		AddonScore:     iptr(2),
		AddonBarRise:   fptr(0.7),
		AddonMarginU:   fptr(0.1),
	}
	changed, err := w.Apply(patch)
	if err != nil {
		t.Fatalf("写回失败：%v", err)
	}

	cfg, err := LoadStrategy(p)
	if err != nil {
		t.Fatalf("读回失败：%v", err)
	}
	// 逐项断言「引擎真正读到的值」
	if !cfg.Enabled {
		t.Fatal("策略开关应为开")
	}
	if cfg.ScoreThreshold != 4 {
		t.Fatalf("买入分数门槛 期望 4 实际 %d", cfg.ScoreThreshold)
	}
	if cfg.Entry.MinBarRisePct == nil || *cfg.Entry.MinBarRisePct != -0.5 {
		t.Fatalf("买入涨跌幅门槛 期望 -0.5 实际 %v", cfg.Entry.MinBarRisePct)
	}
	if cfg.Entry.MarginUSDT != 0.1 || cfg.Entry.MaxMarginUSDT != 1.0 {
		t.Fatalf("买入金额/上限 期望 0.1/1.0 实际 %v/%v", cfg.Entry.MarginUSDT, cfg.Entry.MaxMarginUSDT)
	}
	if !cfg.Addon.Enabled {
		t.Fatal("加仓开关应为开")
	}
	if cfg.Addon.Mode != "resonance" {
		t.Fatalf("加仓模式 期望 resonance 实际 %q", cfg.Addon.Mode)
	}
	if cfg.Addon.ScoreThres != 2 {
		t.Fatalf("加仓分数门槛 期望 2 实际 %d", cfg.Addon.ScoreThres)
	}
	if cfg.Addon.BarRisePct != 0.7 {
		t.Fatalf("加仓涨幅门槛 期望 0.7 实际 %v", cfg.Addon.BarRisePct)
	}
	if cfg.Addon.MarginUSDT != 0.1 {
		t.Fatalf("加仓金额 期望 0.1 实际 %v", cfg.Addon.MarginUSDT)
	}

	// 注释仍然完好（用带注释的样本再跑一次端到端，确认保存路径不碰注释）
	wc, pc := mkWriterWith(t, wSrcCommented)
	if _, err := wc.Apply(&Patch{
		ScoreThreshold: iptr(4),
		MinBarRisePct:  fptr(-0.5),
		AddonScore:     iptr(2),
		AddonMode:      sptr("resonance"),
	}); err != nil {
		t.Fatalf("带注释样本写回失败：%v", err)
	}
	raw := readBack(t, pc)
	for _, c := range []string{"// 六期：必须真跌 0.7%", "// 0 = 不限", "// 买入分数门槛"} {
		if !strings.Contains(raw, c) {
			t.Fatalf("端到端保存后注释丢了：%q", c)
		}
	}
	t.Logf("✓ 管理台一次完整保存：%d 项落盘，引擎读回值逐项核对一致（注释完好）", len(changed))
	for _, c := range changed {
		t.Logf("    · %s", c)
	}
}

// TestWrite_ThenDecideAddonUsesNewMode 写完配置后 decideAddon 立刻按新模式判 ——
// 把「保存」与「判定」串起来，证明保存不是只改了文件。
func TestWrite_ThenDecideAddonUsesNewMode(t *testing.T) {
	w, p := mkWriter(t)

	// 改成共振模式 + 高分数门槛
	if _, err := w.Apply(&Patch{
		AddonMode:    sptr("resonance"),
		AddonScore:   iptr(5),
		AddonBarRise: fptr(0.7),
	}); err != nil {
		t.Fatalf("写回失败：%v", err)
	}

	cfg, err := LoadStrategy(p)
	if err != nil {
		t.Fatalf("读回失败：%v", err)
	}
	// 用读回的配置构一份 service 层配置，喂给 decideAddon
	sc := &conf.Config{}
	*sc = *conf.DefaultConfig()
	sc.Addon = &conf.AddonCfg{
		Enabled:        cfg.Addon.Enabled,
		Mode:           cfg.Addon.Mode,
		ScoreThreshold: cfg.Addon.ScoreThres,
		BarRisePct:     cfg.Addon.BarRisePct,
		PriceRisePct:   cfg.Addon.PriceRise,
		DropPct:        cfg.Addon.DropPct,
		Ratio:          cfg.Addon.Ratio,
		MaxTimes:       cfg.Addon.MaxTimes,
	}
	sc.Entry.MarginUSDT = 0.1
	sc.Entry.MaxMarginUSDT = 1.0

	pos := basePos(1.6000)
	ins := mkIns()
	// score=3 未过 5 → 不加（证明读到的门槛真的生效了）
	if d := decideAddon(sc, pos, 1.5950, mkSignal(3, barMs*11), barMs, ins); d.Add {
		t.Fatal("保存后门槛为 5，score=3 不该加仓（说明读到的还是旧门槛）")
	}
	// score=6 > 5 → 加
	if d := decideAddon(sc, pos, 1.5950, mkSignal(6, barMs*11), barMs, ins); !d.Add {
		t.Fatal("保存后门槛为 5，score=6 应当加仓")
	}
	t.Log("✓ 保存 → 读回 → 判定 全链路一致：新门槛真的进了引擎")
}
