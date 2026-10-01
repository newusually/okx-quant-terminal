package service

import "testing"

// TestExitText 四期口径：文案里只列**真正开着**的出场通道。
//
// 用户口径（2026-10-02）：「不准平仓」= 关掉布林上轨那种乱平仓；
// 随后两次调整止盈线：「止盈 1% 不平仓有问题」→ 保留止盈，再「改成赚 0.3% 也平仓」。
// 最终 = 止盈 **0.3%** + 超时 60 分钟，布林上轨关闭。
//
// 启动日志若把关闭项写成「0.00%」，会让人以为它开着、只是线设在 0 ——
// 与事实相反，所以关闭项必须**不出现**在文案里。
func TestExitText(t *testing.T) {
	cases := []struct {
		name string
		tp   float64
		boll bool
		hold int
		want string
	}{
		{"★ 四期最终：止盈 0.3 + 超时 1 小时", 0.3, false, 60, "止盈 +0.30% / 超时 1 小时"},
		{"二期口径：三条全开", 1.0, true, 360, "止盈 +1.00% / 布林上轨 / 超时 6 小时"},
		{"只关止盈（实测净值会变负，不要这样配）", 0, false, 60, "超时 1 小时"},
		{"只关上轨", 0.3, false, 60, "止盈 +0.30% / 超时 1 小时"},
		{"全部关闭 = 不会自动平仓", 0, false, 0, "无（不会自动平仓）"},
		{"非整小时用分钟", 0.3, false, 90, "止盈 +0.30% / 超时 90 分钟"},
	}
	for _, c := range cases {
		if got := ExitText(c.tp, c.boll, c.hold); got != c.want {
			t.Errorf("%s: ExitText(%v,%v,%d) = %q，want %q", c.name, c.tp, c.boll, c.hold, got, c.want)
		}
	}
}
