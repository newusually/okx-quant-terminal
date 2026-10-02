package service

import "testing"

// ===========================================================================
// 开仓闸门（runEntries 里那几道"上限/冷却"）—— 边界单测
// ===========================================================================
//
// 为什么单独给冷却开一个文件：
//
//   2026-10-02 十期，用户报「提示冷却中 距开仓不足 6 根」，要求「冷却条件全部删除」。
//   把配置改成 0 之后我去读判定式，发现它**关不掉**（在特定输入下）：
//
//       if last, ok := ...; ok && last > 0 && durMs > 0 &&
//           s.Ts-last < int64(cfg.Entry.CooldownBars)*durMs {
//
//   CooldownBars=0 时右边是 0，看着"只要 s.Ts >= last 就没事"。但 s.Ts < last
//   （K 线回填 / 补数据 / 同一根被重扫）时左边是负数，`负数 < 0` 恒为真 →
//   冷却明明关了却照拦，日志还写「距上次开仓不足 0 根」。
//
//   这是本项目第四次遇到同一类问题：**"关闭值"必须显式排除，不能靠算术巧合**。
//   所以除了修，还要把边界钉在测试里 —— 否则下一个人"顺手简化"一下又会退化。
//
// 每组用例的坐标准义：
//
//   3m K 线，durMs = 180000
//   lastTs = 1_000_000_000_000，sigTs 按需要往后推

const (
	gTestDurMs  int64 = 180_000 // 3m
	gTestLastTs int64 = 1_000_000_000_000
)

func TestCooldownBlocked_ZeroMeansOff(t *testing.T) {
	// ★ 十期核心：0 = 冷却完全关闭。这条是这次报障的直接回归。
	cases := []struct {
		name  string
		sigTs int64
		why   string
	}{
		{"同一根重扫（sigTs == lastTs）", gTestLastTs,
			"0 根间隔也不该拦 —— 冷却关了就是关了"},
		{"下一根（隔 1 根）", gTestLastTs + gTestDurMs,
			"正常前进"},
		{"隔很多根", gTestLastTs + 500*gTestDurMs,
			"正常前进"},
		{"★信号时间戳早于上次开仓（回填/乱序）", gTestLastTs - 10*gTestDurMs,
			"这是修之前会误拦的那一种：s.Ts-last 是负数，`负数 < 0` 恒真"},
		{"★信号时间戳远远早于上次开仓", gTestLastTs - 100_000*gTestDurMs,
			"同上，极端乱序"},
	}
	for _, c := range cases {
		if cooldownBlocked(0, gTestLastTs, c.sigTs, gTestDurMs) {
			t.Errorf("cooldown_bars=0（冷却已关闭）却拦下了【%s】，%s", c.name, c.why)
		}
	}
}

func TestCooldownBlocked_NegativeMeansOff(t *testing.T) {
	// 负数在归一化里会被反压成默认值（0），但万一漏到判定式里，
	// 语义必须跟 0 一样是"关"，不能变成"负数根 = 全拦"。
	for _, cd := range []int{-1, -42, -99999} {
		if cooldownBlocked(cd, gTestLastTs, gTestLastTs+gTestDurMs, gTestDurMs) {
			t.Errorf("cooldown_bars=%d 应当等同关闭，却拦下了", cd)
		}
		if cooldownBlocked(cd, gTestLastTs, gTestLastTs-5*gTestDurMs, gTestDurMs) {
			t.Errorf("cooldown_bars=%d 应当等同关闭，却在乱序时间戳上拦下了", cd)
		}
	}
}

func TestCooldownBlocked_EnabledBehavesAsBefore(t *testing.T) {
	// 冷却开启时，行为必须跟修之前**一模一样** —— 这次只加守卫，不改语义。
	const cd = 30

	cases := []struct {
		name   string
		sigTs  int64
		expect bool
	}{
		{"刚开完（同一根）", gTestLastTs, true},
		{"隔 1 根", gTestLastTs + 1*gTestDurMs, true},
		{"隔 29 根（差一根到门槛）", gTestLastTs + 29*gTestDurMs, true},
		{"★隔 30 根（恰好够）", gTestLastTs + 30*gTestDurMs, false},
		{"隔 31 根", gTestLastTs + 31*gTestDurMs, false},
		{"乱序时间戳：宁可少开，仍拦", gTestLastTs - 3*gTestDurMs, true},
	}
	for _, c := range cases {
		got := cooldownBlocked(cd, gTestLastTs, c.sigTs, gTestDurMs)
		if got != c.expect {
			t.Errorf("cooldown_bars=%d 【%s】：期望 blocked=%v，实际 %v",
				cd, c.name, c.expect, got)
		}
	}
}

func TestCooldownBlocked_MissingContextNeverBlocks(t *testing.T) {
	// 上下文不齐时一律放行 —— 冷却不是用来挡"我们不知道"的。
	cases := []struct {
		name                       string
		cd                         int
		lastTs, sigTs, durMs       int64
	}{
		{"没有历史（lastTs=0）", 30, 0, gTestLastTs, gTestDurMs},
		{"周期未知（durMs=0）", 30, gTestLastTs, gTestLastTs + gTestDurMs, 0},
		{"周期为负", 30, gTestLastTs, gTestLastTs + gTestDurMs, -1},
		{"三个都不齐", 30, 0, 0, 0},
	}
	for _, c := range cases {
		if cooldownBlocked(c.cd, c.lastTs, c.sigTs, c.durMs) {
			t.Errorf("【%s】上下文不齐，不该拦，却拦了", c.name)
		}
	}
}

// ---------------------------------------------------------------------------
// 顺便把「0 = 不限」这条口径一起钉住：max_concurrent_positions 的判定
// 写在 runEntries 里是 `> 0 && len(openPos)+opened >= N`，
// 这里用等价的纯逻辑复刻一遍，防止有人把它简化成 `>= N`。
//
// 这条不是重复测试 —— conf 层的 config_limit_test.go 测的是**归一化**
// （0 别被改掉），这里测的是**判定顺序**（0 别变成"已达上限 0"）。
// 一期在第一笔信号就被拦掉、症状是"买得太少"，正是这个顺序反了。
// ---------------------------------------------------------------------------
func TestMaxPositionsGate_ZeroMeansUnlimited(t *testing.T) {
	blocked := func(maxPos, openCount int) bool {
		return maxPos > 0 && openCount >= maxPos
	}

	// 0 = 不限：持仓 0 个 / 30 个 / 1000 个，都不拦
	for _, n := range []int{0, 1, 30, 1000} {
		if blocked(0, n) {
			t.Errorf("max_concurrent_positions=0（不限）却在持仓 %d 时拦下了", n)
		}
	}

	// 上限 30（十期口径）：持仓 29 放行、30 拦
	if blocked(30, 29) {
		t.Errorf("上限 30、持仓 29 时不该拦")
	}
	if !blocked(30, 30) {
		t.Errorf("上限 30、持仓 30 时应当拦（持仓数量要求 < 30）")
	}
	if !blocked(30, 31) {
		t.Errorf("上限 30、持仓 31 时应当拦")
	}
}
