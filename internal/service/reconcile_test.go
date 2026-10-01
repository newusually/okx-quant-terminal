package service

// reconcile_test.go —— 持仓对账（幽灵仓剔除）的判定逻辑单测。
//
// 为什么专门给这一段写单测：
//   reconcilePositions 决定「什么样的仓位会被引擎放弃管理」。误判的代价极不对称 ——
//   把真仓位误判成幽灵，本地就不再管它，而在「不设止损」的策略下这等于敞口裸奔。
//   而且这条路径在正常情况下（OKX 持仓与本地一致）永远跑不到，
//   只有出事了才会执行 —— 那种代码不改一次就上线是不会有人发现写错的。
//
// 单测只覆盖**纯判定**部分（ghostDecision / livePositionIDs），
// 不做任何网络与数据库访问。

import (
	"testing"

	"finally-main/internal/repo"
)

func mkPos(id int64, inst string) repo.OpenPos {
	return repo.OpenPos{
		ID: id, InstID: inst, Sz: 1.9, EntryPx: 0.2223,
		Margin: 2.37, Leverage: 20, OpenTs: 1790841600000, Bar: "15m",
	}
}

// A. 本地与 OKX 完全一致 → 一个都不能动，计数必须清零
func TestReconcile_AllAliveKeepsEverything(t *testing.T) {
	open := []repo.OpenPos{mkPos(1197, "XLM-USDT-SWAP"), mkPos(1202, "PROS-USDT-SWAP")}
	live := map[string]bool{"XLM-USDT-SWAP": true, "PROS-USDT-SWAP": true}
	seen := map[string]int{"XLM-USDT-SWAP": 2} // 上一轮留下的陈旧计数

	keep, ghosts, next := ghostDecision(seen, live, open)

	if len(keep) != 2 {
		t.Fatalf("在持仓应全部保留，实际保留 %d 个", len(keep))
	}
	if len(ghosts) != 0 {
		t.Fatalf("不该有幽灵仓，实际 %d 个", len(ghosts))
	}
	if len(next) != 0 {
		t.Fatalf("计数应被清零，实际 %v", next)
	}
}

// B. 连续缺席：第 1、2 轮必须保留，第 3 轮才判为幽灵
//
// 这是整个函数最要紧的一条 —— 少等一轮就可能把真仓位误杀。
func TestReconcile_NeedsThreeConsecutiveMisses(t *testing.T) {
	open := []repo.OpenPos{mkPos(1197, "XLM-USDT-SWAP")}
	live := map[string]bool{} // OKX 上一个仓都没有

	seen := map[string]int{}
	for round := 1; round <= ghostConfirmRounds; round++ {
		var keep, ghosts []repo.OpenPos
		keep, ghosts, seen = ghostDecision(seen, live, open)

		if round < ghostConfirmRounds {
			if len(keep) != 1 || len(ghosts) != 0 {
				t.Fatalf("第 %d 轮就动手了：keep=%d ghosts=%d（必须等满 %d 轮）",
					round, len(keep), len(ghosts), ghostConfirmRounds)
			}
			continue
		}
		if len(ghosts) != 1 || ghosts[0].InstID != "XLM-USDT-SWAP" {
			t.Fatalf("第 %d 轮应判为幽灵仓，实际 keep=%d ghosts=%d", round, len(keep), len(ghosts))
		}
		if len(keep) != 0 {
			t.Fatalf("判定为幽灵后不该再留在在持仓里，实际 %d 个", len(keep))
		}
	}
}

// C. 数到一半回来了 → 计数必须清零，下一轮重新数
func TestReconcile_ReappearingResetsCounter(t *testing.T) {
	open := []repo.OpenPos{mkPos(1197, "XLM-USDT-SWAP")}

	keep, ghosts, seen := ghostDecision(map[string]int{}, map[string]bool{}, open)
	if len(keep) != 1 || len(ghosts) != 0 || seen["XLM-USDT-SWAP"] != 1 {
		t.Fatalf("第 1 轮缺席应计数 1，实际 seen=%v", seen)
	}

	// 第 2 轮又出现了
	live := map[string]bool{"XLM-USDT-SWAP": true}
	keep, ghosts, seen = ghostDecision(seen, live, open)
	if len(keep) != 1 || len(ghosts) != 0 {
		t.Fatalf("仓位回来了，不该判定为幽灵：keep=%d ghosts=%d", len(keep), len(ghosts))
	}
	if len(seen) != 0 {
		t.Fatalf("仓位回来后计数必须清零，实际 %v", seen)
	}

	// 第 3 轮再次缺席 → 从 1 重新数，不能因为「历史累计 2」就直接判死
	keep, ghosts, seen = ghostDecision(seen, map[string]bool{}, open)
	if len(ghosts) != 0 {
		t.Fatalf("计数应已重置，这一次只该记为第 1 轮缺席，不能直接判幽灵")
	}
	if seen["XLM-USDT-SWAP"] != 1 {
		t.Fatalf("计数应重新从 1 开始，实际 %v", seen)
	}
}

// D. 已经不在 openPos 里的陈旧计数要被清掉，不能让 map 无限长
func TestReconcile_StaleCountersAreDropped(t *testing.T) {
	open := []repo.OpenPos{mkPos(1197, "XLM-USDT-SWAP")}
	live := map[string]bool{"XLM-USDT-SWAP": true}
	seen := map[string]int{
		"XLM-USDT-SWAP": 1,
		"GONE-USDT-SWAP": 2, // 这行本地已经平掉了
	}

	_, _, next := ghostDecision(seen, live, open)
	if _, ok := next["GONE-USDT-SWAP"]; ok {
		t.Fatalf("陈旧计数没被清掉：%v", next)
	}
}

// E. 没有在持仓 → 空进空出，计数清空
func TestReconcile_EmptyOpenPos(t *testing.T) {
	keep, ghosts, next := ghostDecision(map[string]int{"X": 3}, map[string]bool{}, nil)
	if len(keep) != 0 || len(ghosts) != 0 || len(next) != 0 {
		t.Fatalf("空在持仓应得到空结果与空计数，实际 keep=%d ghosts=%d next=%v",
			len(keep), len(ghosts), next)
	}
}

// F. 混合：一个还活着、一个已经缺席满轮次
func TestReconcile_MixedPositions(t *testing.T) {
	open := []repo.OpenPos{
		mkPos(1, "LTC-USDT-SWAP"),
		mkPos(2, "PROS-USDT-SWAP"),
		mkPos(3, "XLM-USDT-SWAP"),
	}
	live := map[string]bool{"LTC-USDT-SWAP": true}
	// PROS 缺席第 1 轮、XLM 已累计 2 轮
	seen := map[string]int{"XLM-USDT-SWAP": 2}

	keep, ghosts, next := ghostDecision(seen, live, open)

	got := map[string]bool{}
	for _, p := range keep {
		got[p.InstID] = true
	}
	if !got["LTC-USDT-SWAP"] || !got["PROS-USDT-SWAP"] {
		t.Fatalf("活着/刚缺席一轮的都该保留，实际 keep=%v", got)
	}
	if got["XLM-USDT-SWAP"] {
		t.Fatalf("XLM 应被判为幽灵并剔除")
	}
	if len(ghosts) != 1 || ghosts[0].InstID != "XLM-USDT-SWAP" {
		t.Fatalf("应恰好剔掉 XLM，实际 %+v", ghosts)
	}
	// LTC 活着 → 计数清零；PROS 记 1；XLM 判死后移除
	if _, ok := next["LTC-USDT-SWAP"]; ok {
		t.Fatalf("活着的仓位不该留计数：%v", next)
	}
	if next["PROS-USDT-SWAP"] != 1 {
		t.Fatalf("PROS 应记为 1 轮缺席，实际 %v", next)
	}
	if _, ok := next["XLM-USDT-SWAP"]; ok {
		t.Fatalf("判为幽灵后应从计数里移除（写库失败会重新从 0 数）：%v", next)
	}
}

// G. openPos 为空切片（不是 nil）也要走空分支
func TestReconcile_EmptyOpenPosSlice(t *testing.T) {
	keep, ghosts, next := ghostDecision(nil, nil, []repo.OpenPos{})
	if len(keep) != 0 || len(ghosts) != 0 || len(next) != 0 {
		t.Fatalf("空切片应得到空结果，实际 keep=%d ghosts=%d next=%v", len(keep), len(ghosts), next)
	}
}

// H. livePositionIDs：只有 pos=0 才算「没仓」
//
// OKX 在双向持仓模式下会对已经平掉的那条腿回一条 pos=0 的记录。
// 要是把它当成「有仓」，幽灵仓就永远剔不掉 —— 对账形同虚设。
//
// ★ 但反过来必须极保守：只要张数**非 0 就算有仓**，哪怕是负数。
//   本策略只做多，理论上不会出现负张数；真出现了说明账户侧有事，
//   这时「保留本地仓位、继续管它」比「当幽灵剔掉、放任裸奔」安全得多。
//   误杀的代价是不可逆的（不设止损），多留一行的代价只是几次接口调用。
func TestLivePositionIDs_OnlyZeroMeansNoPosition(t *testing.T) {
	ps := []Position{
		{InstID: "XLM-USDT-SWAP", PosSide: "long", Pos: "1.9"},
		{InstID: "CLOSED-USDT-SWAP", PosSide: "short", Pos: "0"},   // 平掉的腿
		{InstID: "EMPTY-USDT-SWAP", PosSide: "long", Pos: ""},      // 空字段
		{InstID: "BTC-USDT-SWAP", PosSide: "long", Pos: "0.01000"}, // 小数张
		{InstID: "NEG-USDT-SWAP", PosSide: "short", Pos: "-3"},     // 异常反向腿
	}
	ids := livePositionIDs(ps)

	for _, ok := range []string{"XLM-USDT-SWAP", "BTC-USDT-SWAP", "NEG-USDT-SWAP"} {
		if !ids[ok] {
			t.Fatalf("%s 张数非 0，必须算有仓（保守优先，误杀不可逆）", ok)
		}
	}
	for _, bad := range []string{"CLOSED-USDT-SWAP", "EMPTY-USDT-SWAP"} {
		if ids[bad] {
			t.Fatalf("%s 张数为 0/空，不该算有仓（实际 map=%v）", bad, ids)
		}
	}
}

// I. 常量钉死：确认阈值没有被随手改小
//
// 这个数字直接决定「几轮之后放弃管理一个仓位」，改小就会放大误杀风险，
// 所以用测试钉住，改动必须是有意识的（改测试 + 改代码）。
func TestGhostConfirmRoundsIsThree(t *testing.T) {
	if ghostConfirmRounds != 3 {
		t.Fatalf("ghostConfirmRounds 期望 3（3 秒巡检 ≈ 9 秒确认），实际 %d", ghostConfirmRounds)
	}
}
