package service

// 六期（2026-10-02）新增：开仓均价校正的守门测试。
//
// 事故背景：trader.go 开仓时 EntryPx 写的是信号 K 线收盘价（乐观初值），
// 市价单真成交的均价能比它高 0.5%~0.8%（小币）。止盈判据 (标记价 ÷ entry_px − 1)
// 因此在开仓瞬间就"假浮盈"到止盈线 → 秒判止盈 → 实际倒亏手续费。
// 实测：GRASS 标记「止盈 +0.79%」，OKX 真实账单 −3.68%。
//
// 修复：syncEntryPx 用 OKX 持仓的真实 avgPx 覆盖本地 entry_px。
// 这里钉死筛选与判定的边界，防止有人把它改回"无条件覆盖"或"从不覆盖"。

import (
	"testing"

	"finally-main/internal/repo"
)

func mkPxPos(instID, pos, avgPx string) Position {
	return Position{InstID: instID, Pos: pos, AvgPx: avgPx, PosSide: "long", MgnMode: "cross"}
}

// A. 筛选：只认「张数非 0 且均价 > 0」的持仓。
//
// 双向持仓下 OKX 会给已平的腿回 pos=0 的记录 —— 那条 avgPx 是上一轮的成本，
// 拿来覆盖会把开仓价改歪。这是本项目反复踩过的「同一个量两条路」的变体。
func TestRealEntryPxByInstFilters(t *testing.T) {
	ps := []Position{
		mkPxPos("A-USDT-SWAP", "2", "0.7145"),   // 正常
		mkPxPos("B-USDT-SWAP", "0", "0.1234"),   // 已平的腿 → 必须排除
		mkPxPos("C-USDT-SWAP", "5", ""),         // 均价空 → 排除
		mkPxPos("D-USDT-SWAP", "1", "0"),        // 均价 0 → 排除
		mkPxPos("E-USDT-SWAP", "", "0.5"),       // 张数空（=0）→ 排除
	}
	got := realEntryPxByInst(ps)
	if len(got) != 1 {
		t.Fatalf("应只保留 1 条合法持仓，实际 %d 条：%v", len(got), got)
	}
	if v, ok := got["A-USDT-SWAP"]; !ok || v != 0.7145 {
		t.Fatalf("A 应保留 0.7145，实际 v=%v ok=%v", v, ok)
	}
	if _, ok := got["B-USDT-SWAP"]; ok {
		t.Fatalf("pos=0 的已平腿不能进均价表（会把开仓价改回上一轮成本）")
	}
}

// B. 判定：容差与非法值。
//
// 容差 0.02% 的意义：exitPass 3 秒一轮，没有容差就是每 3 秒一次无意义写库。
// 而 GRASS 事故的偏差是 0.79%，是容差的 40 倍 —— 必须能触发。
func TestNeedSyncEntryPx(t *testing.T) {
	cases := []struct {
		name       string
		oldPx, new float64
		want       bool
	}{
		{"偏差 0.79%（GRASS 实测）→ 校正", 0.7089, 0.7145, true},
		{"偏差 0.53%（USELESS 实测）→ 校正", 0.23756, 0.23882, true},
		{"完全一致 → 不动", 0.7145, 0.7145, false},
		{"偏差 0.01%（容差内）→ 不动", 100, 100.01, false},
		{"偏差 0.03%（超容差）→ 校正", 100, 100.03, true},
		{"真实价更低也要校正（不是只修高的）", 100, 99.0, true},
		{"本地价非法(0) → 不动", 0, 100, false},
		{"真实价非法(0) → 不动", 100, 0, false},
		{"两个都是 0 → 不动", 0, 0, false},
	}
	for _, c := range cases {
		if got := needSyncEntryPx(c.oldPx, c.new); got != c.want {
			t.Errorf("%s：得到 %v，期望 %v", c.name, got, want(c.want))
		}
	}
}

func want(b bool) bool { return b }

// C. 端到端的最小验证：内存里的 EntryPx 必须被同步改掉。
//
// 这是「同一轮 runExits 立刻用真实均价判止盈」的前提 —— 只改库不改内存的话，
// 校正要等下一轮（3 秒），对秒级误平来说 3 秒足够致命。
// store 传 nil 走「写库会失败」路径：内存必须仍然被改吗？—— 不：
// 设计上**写库成功才改内存**，两者必须原子。所以 nil store 时函数整体早退。
func TestSyncEntryPxNilStoreKeepsMemoryUntouched(t *testing.T) {
	ops := []repo.OpenPos{{ID: 1, InstID: "A-USDT-SWAP", EntryPx: 0.7089}}
	got := syncEntryPx(nil, []Position{mkPxPos("A-USDT-SWAP", "2", "0.7145")}, ops)
	if got[0].EntryPx != 0.7089 {
		t.Fatalf("store 为 nil 时不应改动内存：实际 %v", got[0].EntryPx)
	}
}

// D. 空输入早退，不得 panic（exitPass 3 秒一轮，任何 panic 都是全局事故）。
func TestSyncEntryPxEmptyInputs(t *testing.T) {
	ops := []repo.OpenPos{{ID: 1, InstID: "A-USDT-SWAP", EntryPx: 0.7089}}
	_ = syncEntryPx(nil, nil, ops)              // 持仓空
	_ = syncEntryPx(nil, []Position{mkPxPos("A-USDT-SWAP", "2", "0.7145")}, nil) // 在持空
}
