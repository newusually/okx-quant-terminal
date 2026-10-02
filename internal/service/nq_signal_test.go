package service

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"finally-main/internal/conf"
)

// nq_signal_test.go —— 只读板块（NQ）专属买入口径（2026-10-02 十三期）
//
// 用户口径：「买入信号共振给我 NQ 单独算，只算共振 4+ 下跌情况买入」。
//
// 这组测试守的核心只有一条：**NQ 的判定必须和全市场不同，而且「0」在这里
// 代表"只要收阴"，不是"关闭这个条件"**。
//
// 之所以要专门钉住 0 的语义：全局 entry.min_bar_rise_pct 用的是带符号三态，
// 那里 0 = 关闭；这里 0 = 只要收阴。同一份代码库里同一个数字两种意思，
// 是这类口径最容易走岔的地方 —— 而且走岔了不会报错，只是信号数量变了。

func sigNQ(score int, rise float64, ready bool) *Signal {
	return &Signal{
		InstID: NQInstID, Bar: "3m", Ts: 1_700_000_000_000,
		Open: 30000, Close: 30000, Score: score, RisePct: rise, Ready: ready,
	}
}

// TestReadonlySignalRule_FourPlusAndDown 主口径：共振 >= 4 且收阴。
func TestReadonlySignalRule_FourPlusAndDown(t *testing.T) {
	rule := ReadonlySignalRule{ScoreThreshold: 4, MaxRisePct: 0}
	cases := []struct {
		name  string
		score int
		rise  float64
		want  bool
	}{
		// 分数不够（全市场会放行，NQ 必须拦掉 —— 这就是"单独算"的意义）
		{"共振3且下跌", 3, -0.05, false},
		{"共振2且下跌", 2, -0.20, false},
		// 分数够但不是下跌
		{"共振4但上涨", 4, +0.10, false},
		{"共振6但上涨", 6, +0.30, false},
		// ★ 恰好 0（平盘）不算"下跌"：判定是**严格小于**
		{"共振4但平盘", 4, 0, false},
		// 合格
		{"共振4且小跌", 4, -0.01, true},
		{"共振4且大跌", 4, -0.30, true},
		{"共振8且下跌", 8, -0.12, true},
		// 暖机不够 → 一律不合格（Score 本身没有意义）
		{"共振4且下跌但未暖机", 4, -0.1, false},
	}
	for _, c := range cases {
		ready := c.name != "共振4且下跌但未暖机"
		got := rule.Qualify(sigNQ(c.score, c.rise, ready))
		if got != c.want {
			t.Errorf("%s：score=%d rise=%.2f ready=%v → %v，期望 %v",
				c.name, c.score, c.rise, ready, got, c.want)
		}
	}
}

// TestReadonlySignalRule_ZeroMeansDownNotOff 把「0 = 只要收阴」钉死。
//
// 如果有人把这里改成"沿用全局三态（0 = 关闭）"，那么 rise=+0.1（上涨）也会
// 通过 —— 本测试会立刻红。这正是要防的那种静默口径漂移。
func TestReadonlySignalRule_ZeroMeansDownNotOff(t *testing.T) {
	rule := ReadonlySignalRule{ScoreThreshold: 4, MaxRisePct: 0}
	if rule.Qualify(sigNQ(6, +0.1, true)) {
		t.Fatal("MaxRisePct=0 被当成了「关闭涨跌幅条件」：上涨的根也通过了 —— " +
			"0 在这里的语义是「只要收阴」（RisePct < 0），不是关闭")
	}
	if !rule.Qualify(sigNQ(6, -0.001, true)) {
		t.Fatal("MaxRisePct=0 时，微跌（-0.001%）的根应当合格")
	}
}

// TestReadonlySignalRule_NegativeMaxRise 收紧版：必须跌超指定幅度。
func TestReadonlySignalRule_NegativeMaxRise(t *testing.T) {
	rule := ReadonlySignalRule{ScoreThreshold: 4, MaxRisePct: -0.1}
	if rule.Qualify(sigNQ(4, -0.05, true)) {
		t.Error("跌幅 -0.05% 未达 -0.1% 门槛，不该合格")
	}
	if rule.Qualify(sigNQ(4, -0.1, true)) {
		t.Error("跌幅恰好 -0.1% 不算「跌超」：判定是严格小于，不该合格")
	}
	if !rule.Qualify(sigNQ(4, -0.2, true)) {
		t.Error("跌幅 -0.2% 超过门槛，应当合格")
	}
}

// TestReadonlySignalRule_DisabledNeverQualifies 未启用（或配置坏掉）时不参与判定，
// 调用方会自动退回全市场通用口径 —— 由 Enabled() 决定走哪条路。
func TestReadonlySignalRule_DisabledNeverQualifies(t *testing.T) {
	for _, r := range []ReadonlySignalRule{{}, {ScoreThreshold: 0}, {ScoreThreshold: -1}} {
		if r.Enabled() {
			t.Fatalf("零值/负门槛不该被判为启用：%+v", r)
		}
		if r.Qualify(sigNQ(8, -0.5, true)) {
			t.Fatalf("未启用的规则不该放行任何信号：%+v", r)
		}
	}
}

// TestReadonlySignalRule_NaNRiseRejected NaN 的涨跌幅（脏数据）必须判为不合格。
//
// RisePct 来自 (收-开)/开，开盘价为 0 时上游会记 0；真出现 NaN 时
// `NaN < x` 恒为 false —— 保守方向正确。反过来写成 !(NaN >= x) 会把 NaN 放行。
func TestReadonlySignalRule_NaNRiseRejected(t *testing.T) {
	rule := ReadonlySignalRule{ScoreThreshold: 4, MaxRisePct: 0}
	if rule.Qualify(sigNQ(8, math.NaN(), true)) {
		t.Fatal("NaN 涨跌幅被放行了")
	}
}

// TestNQMissingDays_NewestFirst 待补日期的顺序必须是「新 → 旧」。
//
// ★ 这是用户 2026-10-02 报的那个问题的本体 ★
//
// 现象：「NQ 数据滞后，1 天前的数据看不到」——库里只有一个月前那一天。
// 根因：待补列表是「旧→新」排的，而单轮只取前 8 个 → 每轮都在尝试最老的 8 天，
// 一撞限流就中止，最近的日子永远排在队尾，一轮都轮不到。
//
// 顺序错了不会报任何错，只是"最近的数据一直看不到"，从日志里几乎看不出来
// （日志只会说「缺口 30 天 · 本轮拉 8 天 · 留待下轮 22 天」，看着完全正常）。
// 所以必须由测试钉住。
func TestNQMissingDays_NewestFirst(t *testing.T) {
	today := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)

	// 场景一：库里什么都没有 → 30 天全缺，第一个必须是今天，最后一个是 29 天前
	miss := nqMissingDaysOf(map[int64]bool{}, today)
	if len(miss) != nqDays {
		t.Fatalf("全缺时应返回 %d 天，实际 %d", nqDays, len(miss))
	}
	if !miss[0].Equal(today) {
		t.Fatalf("第一个待补日期必须是今天 %s，实际 %s", today.Format("2006-01-02"), miss[0].Format("2006-01-02"))
	}
	for i := 1; i < len(miss); i++ {
		if !miss[i].Before(miss[i-1]) {
			t.Fatalf("待补日期必须严格递减（新→旧），第 %d 项 %s 不小于前一项 %s",
				i, miss[i].Format("2006-01-02"), miss[i-1].Format("2006-01-02"))
		}
	}

	// 场景二：**复现用户的现场** —— 只有一个月前那一天（09-03），其余全缺。
	// 单轮只取前 nqMaxPerRound 个，那必须全是最近的日子，而不是老日子。
	have := map[int64]bool{}
	d := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	have[d.UnixMilli()] = true

	miss = nqMissingDaysOf(have, today)
	batch := miss
	if len(batch) > nqMaxPerRound {
		batch = batch[:nqMaxPerRound]
	}
	oldest := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	for _, b := range batch {
		if !b.After(oldest) {
			t.Fatalf("本轮批次里出现了 %s（不晚于库里已有的 %s）——"+
				"又回到「每轮只拉最老的日子」的老毛病了",
				b.Format("2006-01-02"), oldest.Format("2006-01-02"))
		}
	}
	if !batch[0].Equal(today) || !batch[1].Equal(today.AddDate(0, 0, -1)) {
		t.Fatalf("本轮批次的前两天必须是今天/昨天，实际 %s / %s",
			batch[0].Format("2006-01-02"), batch[1].Format("2006-01-02"))
	}

	// 场景三：最近 2 天即使库里有，也要重拉（当天数据还在生成）
	have[time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).UnixMilli()] = true
	miss = nqMissingDaysOf(have, today)
	if !miss[0].Equal(today) || !miss[1].Equal(today.AddDate(0, 0, -1)) {
		t.Fatalf("最近 2 天必须始终重拉，实际前两项 %s / %s",
			miss[0].Format("2006-01-02"), miss[1].Format("2006-01-02"))
	}
}

// TestNQBadIPsFiltered 已知连不通的地址必须被丢掉。
//
// 它偶尔会通过 DoH 回来（实测日志：`解析到 [194.8.15.180]`），
// 一旦混进候选列表就会先耗掉一次 dial 超时，把整轮节奏拖垮。
func TestNQBadIPsFiltered(t *testing.T) {
	ctx := context.Background()
	ips := resolveDukaIPs(ctx)
	if len(ips) == 0 {
		t.Fatal("候选 IP 不该为空（至少有内置兜底）")
	}
	for _, ip := range ips {
		if dukaBadIPs[ip] {
			t.Fatalf("已知连不通的 IP %s 出现在候选列表里：%v", ip, ips)
		}
	}
	// 内置兜底必须始终在列表里（DoH 少给地址也不能少一条路）
	for _, want := range dukaFallbackIPs {
		found := false
		for _, ip := range ips {
			if ip == want {
				found = true
			}
		}
		if !found {
			t.Errorf("内置兜底 IP %s 不在候选列表里：%v", want, ips)
		}
	}
	// 不能有重复
	seen := map[string]bool{}
	for _, ip := range ips {
		if seen[ip] {
			t.Errorf("候选 IP 重复：%s（%v）", ip, ips)
		}
		seen[ip] = true
	}
}
// TestNQRuleFromConfig 配置 → 规则 的映射。
//
// 两种"没配到"要走不同的方向，这是本测试守的重点：
//
//	整块缺失 → **默认口径且启用**（最坏也不过"和默认一样"，
//	           绝不退化成全市场的 -0.7% → 一条信号都没有）
//	显式 false → 未启用，如实退回全市场口径（用户自己关的，要认）
func TestNQRuleFromConfig(t *testing.T) {
	off := false
	on := true

	// 整块缺失 / cfg 为 nil：都用默认口径（4 / 只要收阴），且是启用的
	for _, cfg := range []*conf.Config{nil, {}} {
		got := nqRuleFromConfig(cfg)
		if !got.Enabled() {
			t.Fatalf("配置缺失时应退回**默认口径并启用**，不能变成关闭：cfg=%+v", cfg)
		}
		if got.ScoreThreshold != conf.DefaultNQSignalScoreThreshold || got.MaxRisePct != conf.DefaultNQSignalMaxRisePct {
			t.Fatalf("配置缺失时应用的默认口径应为 %d/%.2f，实际 %+v",
				conf.DefaultNQSignalScoreThreshold, conf.DefaultNQSignalMaxRisePct, got)
		}
	}

	// 显式关闭：如实不启用
	if got := nqRuleFromConfig(&conf.Config{NQSignal: &conf.NQSignalCfg{Enabled: &off, ScoreThreshold: 4}}); got.Enabled() {
		t.Error("enabled=false 时应返回未启用规则")
	}

	// 正常配置：原样映射
	got := nqRuleFromConfig(&conf.Config{NQSignal: &conf.NQSignalCfg{
		Enabled: &on, ScoreThreshold: 4, MaxRisePct: 0,
	}})
	if !got.Enabled() || got.ScoreThreshold != 4 || got.MaxRisePct != 0 {
		t.Fatalf("配置 4/0 应原样映射，实际 %+v", got)
	}

	// score_threshold 写成 0（漏写）→ 补默认 4，而不是"共振 0+"把门槛放松
	got = nqRuleFromConfig(&conf.Config{NQSignal: &conf.NQSignalCfg{ScoreThreshold: 0}})
	if got.ScoreThreshold != conf.DefaultNQSignalScoreThreshold {
		t.Fatalf("score_threshold=0 应补成默认 %d，实际 %d",
			conf.DefaultNQSignalScoreThreshold, got.ScoreThreshold)
	}
}

// TestNQRuleFingerprint_DistinguishesRules 口径指纹：门槛或方向变了必须能看出来，
// 否则 ensureNQRuleFresh 会认为"口径没变"，直接跳过作废流程 → 新配置永不生效
// （水位线 + INSERT IGNORE 两重机制会把历史 K 线锁死在旧口径里）。
func TestNQRuleFingerprint_DistinguishesRules(t *testing.T) {
	a := ReadonlySignalRule{ScoreThreshold: 4, MaxRisePct: 0}
	b := ReadonlySignalRule{ScoreThreshold: 5, MaxRisePct: 0}
	c := ReadonlySignalRule{ScoreThreshold: 4, MaxRisePct: -0.1}
	d := ReadonlySignalRule{}

	seen := map[string]string{}
	for _, r := range []ReadonlySignalRule{a, b, c, d} {
		fp := r.String()
		if fp == "" {
			t.Fatalf("口径指纹不能为空：%+v", r)
		}
		if prev, dup := seen[fp]; dup {
			t.Fatalf("不同口径拿到同一个指纹 %q：%+v 与 %s", fp, r, prev)
		}
		seen[fp] = fp
	}
	// 指纹必须是「人能在日志里读懂」的描述
	if fp := a.String(); fp != "共振 >= 4 且该根只要收阴" {
		t.Fatalf("指纹文案变了（日志里会说错口径）：%q", fp)
	}
}

// TestRealConfig_NQSignalRule —— ★ 二十一期改写（2026-10-03）★
//
// 原断言是「真源里必须存在 nq_signal 块」。二十一期 NQ 板块整体下线
// （用户口径「取消NQ所有东西 包括并且删除NQ按钮 数据等页面还有信号」），
// 真源与示例里的 nq_signal 块已删、启动项已摘。
// 这条守门测试随之反转：**断言 nq_signal 不再出现在配置里** ——
// 防止有人把废弃块抄回来，让已删除的 NQ 数据路径悄悄复活。
func TestRealConfig_NQSignalRule(t *testing.T) {
	for _, rel := range []string{
		filepath.Join("..", "..", "configs", "okx_strategy.json"),
		filepath.Join("..", "..", "configs", "okx_strategy.example.json"),
	} {
		raw, err := os.ReadFile(rel)
		if err != nil {
			t.Skipf("配置不存在（%s），跳过", rel)
		}
		var cfg conf.Config
		if err := json.Unmarshal(conf.StripJSONComments(raw), &cfg); err != nil {
			t.Fatalf("%s 解析失败：%v", rel, err)
		}
		if cfg.NQSignal != nil {
			t.Fatalf("%s 不应再包含 nq_signal 块 —— NQ 板块二十一期已下线，"+
				"留着它会让下线的口径看起来还活着", rel)
		}
	}
}
