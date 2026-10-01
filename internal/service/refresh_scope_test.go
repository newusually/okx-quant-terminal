package service

// refresh_scope_test.go —— 「续最新一根」的合约范围判定
//
// 这个判据决定「给哪些合约续最新 K 线」。挑错有两个方向，都必须钉住：
//   挑少了 → 扫描要算的合约没续 → 每轮回退网络，闸门被抢空（本次要修的 bug）
//   挑多了 → 四周期 × 可交易合约数顶穿 OKX 20 次/2 秒闸门 → 1m 永远不新鲜
//
// 还有一个更隐蔽的方向：**查库失败时不能把所有合约都截掉**，
// 否则全站 K 线图一起停更，而且日志里看不出是「截断」还是「真的没数据」。

import (
	"fmt"
	"testing"
)

func TestPickRefreshInsts(t *testing.T) {
	list := []string{"BTC-USDT-SWAP", "ETH-USDT-SWAP", "SOL-USDT-SWAP", "DOGE-USDT-SWAP", "XRP-USDT-SWAP"}

	// A. n<=0 = 不限（退回老行为）
	for _, n := range []int{0, -1, -100} {
		if got := pickRefreshInsts(list, n); got != nil {
			t.Errorf("A n=%d 应当返回 nil（不限），得到 %v", n, got)
		}
	}

	// B. 列表为空 → nil。这条是「查库失败不误杀」的守门人：
	//    ListTradeableInstruments 出错时调用方传进来的是空切片，
	//    这里若返回空 map，所有合约都会被 continue 掉，全站图停更。
	for _, n := range []int{0, 1, 80, 1000} {
		if got := pickRefreshInsts(nil, n); got != nil {
			t.Errorf("B n=%d 空列表应返回 nil，得到 %v", n, got)
		}
		if got := pickRefreshInsts([]string{}, n); got != nil {
			t.Errorf("B n=%d 空切片应返回 nil，得到 %v", n, got)
		}
	}

	// C. n >= len(list) → nil（截了等于没截，不做无谓分配）
	if got := pickRefreshInsts(list, len(list)); got != nil {
		t.Errorf("C n==len 应返回 nil，得到 %v", got)
	}
	if got := pickRefreshInsts(list, len(list)+1); got != nil {
		t.Errorf("C n>len 应返回 nil，得到 %v", got)
	}

	// D. 正常截取：只含前 n 个，且一个不多一个不少
	got := pickRefreshInsts(list, 2)
	if len(got) != 2 {
		t.Fatalf("D 期望 2 个，得到 %d 个：%v", len(got), got)
	}
	if !got["BTC-USDT-SWAP"] || !got["ETH-USDT-SWAP"] {
		t.Errorf("D 前 2 个应当入选，得到 %v", got)
	}
	// 顺序敏感：第 3 个及以后必须落选
	for _, id := range list[2:] {
		if got[id] {
			t.Errorf("D %s 是第 3 个以后，不该入选", id)
		}
	}

	// E. n=1 的边界
	one := pickRefreshInsts(list, 1)
	if len(one) != 1 || !one["BTC-USDT-SWAP"] {
		t.Errorf("E n=1 应只含列表首个，得到 %v", one)
	}

	// F. 真实规模下不越界：80 / 169
	//
	// ⚠ 夹具必须自证唯一性：第一版写成 string(rune('A'+i%26)) 只造出 26 个不同 ID，
	//   进 map 一去重就剩 26，测试报「应得 80 个，得到 26」——
	//   **错的是夹具不是实现**。所以这里先生成、再断言 len(set)==169。
	big := make([]string, 0, 169)
	for i := 0; i < 169; i++ {
		big = append(big, fmt.Sprintf("INST-%03d-USDT-SWAP", i))
	}
	if uniq := len(pickRefreshInsts(big, 169)); uniq != 0 {
		// pickRefreshInsts(big,169) 应得 nil（不限），这里只借它确认「不截时返回 nil」
		t.Fatalf("F 夹具前提不成立：169 取 169 应得 nil，得到 %d 个", uniq)
	}
	uniqSet := map[string]bool{}
	for _, id := range big {
		uniqSet[id] = true
	}
	if len(uniqSet) != 169 {
		t.Fatalf("F 夹具自证失败：%d 个 ID 里有 %d 个唯一值，夹具不是 169 个不同合约", len(big), len(uniqSet))
	}
	if s := pickRefreshInsts(big, 80); len(s) != 80 {
		t.Errorf("F 169 取 80 应得 80 个，得到 %d", len(s))
	}
	if s := pickRefreshInsts(big, 200); s != nil {
		t.Errorf("F 169 取 200 应得 nil（不限），得到 %d 个", len(s))
	}
}

// TestRefreshScope_NilFn 没配回调时必须退回「不限」，
// 否则老部署（不设 RefreshTopNFn）会突然只剩零个合约续 K 线。
func TestRefreshScope_NilFn(t *testing.T) {
	m := &BackfillManager{}
	if got := m.refreshScope(); got != nil {
		t.Errorf("RefreshTopNFn=nil 应返回 nil（不限），得到 %v", got)
	}
}
