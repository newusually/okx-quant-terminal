package service

// strategy_store_test.go —— 策略配置热插拔的回归测试
//
// 覆盖四件事（都是「改 JSON 就能改可买入金额 / symbolList」的前提）：
//   1. 启动时能读到 JSON 里的值
//   2. 改了文件后**不重建 store** 也能读到新值（这就是热插拔）
//   3. 只动 mtime、内容没变 → 判为「没变」（不触发下游重算）
//   4. 文件被写坏 → 沿用上一份好配置，绝不打回默认值

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeStrategyFile(t *testing.T, path string, capUSDT string) {
	t.Helper()
	body := `{
  // 测试用配置，带注释（走 StripJSONComments）
  "enabled": true,
  "dry_run": true,
  "bar": "15m",
  "max_order_margin_usdt": ` + capUSDT + `,
  "min_quote_volume_24h": 1000000,
  "entry": {
    "margin_usdt": 1.2,
    "leverage": 20,
    "margin_policy": "min_one",
    "max_margin_usdt": 1.5
  }
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("写配置失败：%v", err)
	}
	// 强制 mtime 前进 1 秒，避免「同一秒内两次写入」在低精度文件系统上判不出变化
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(path, future, future)
}

func TestStrategyStore_HotReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "okx_strategy.json")
	writeStrategyFile(t, path, "1.0")

	st := NewStrategyStore(path, nil)
	if got := st.Get().MaxOrderMarginUSDT; got != 1.0 {
		t.Fatalf("初始准入上限 = %v，想要 1.0", got)
	}

	// ---- 核心：只改文件，不重建 store ----
	writeStrategyFile(t, path, "1.5")
	if got := st.Get().MaxOrderMarginUSDT; got != 1.5 {
		t.Fatalf("改完 JSON 后 Get() = %v，想要 1.5（热插拔没生效）", got)
	}
	// 其它字段也跟着热更新了
	if got := st.Get().Entry.MarginUSDT; got != 1.2 {
		t.Fatalf("每笔保证金 = %v，想要 1.2", got)
	}

	// ---- 再改回去，确认是双向的 ----
	writeStrategyFile(t, path, "0.8")
	if got := st.Get().MaxOrderMarginUSDT; got != 0.8 {
		t.Fatalf("收紧到 0.8U 后 Get() = %v，想要 0.8（被 Go 里的钳制吃掉了？）", got)
	}
}

func TestStrategyStore_ContentUnchangedNotTriggered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "okx_strategy.json")
	writeStrategyFile(t, path, "1.5")

	st := NewStrategyStore(path, nil)
	if got := st.Get().MaxOrderMarginUSDT; got != 1.5 {
		t.Fatalf("初始 = %v，想要 1.5", got)
	}

	// 只动 mtime（编辑器「另存为」/ touch），内容一模一样 → 不该算变化
	writeStrategyFile(t, path, "1.5")
	_, changed, err := st.Force()
	if err != nil {
		t.Fatalf("Force 报错：%v", err)
	}
	if changed {
		t.Fatal("内容没变却报了 changed —— 会导致每次另存为都白跑一遍全市场准入过滤")
	}
}

func TestStrategyStore_BrokenFileKeepsLastGood(t *testing.T) {
	path := filepath.Join(t.TempDir(), "okx_strategy.json")
	writeStrategyFile(t, path, "1.5")

	st := NewStrategyStore(path, nil)
	if got := st.Get().MaxOrderMarginUSDT; got != 1.5 {
		t.Fatalf("初始 = %v，想要 1.5", got)
	}

	// 手滑写坏（少个逗号 / 括号没闭合）
	if err := os.WriteFile(path, []byte(`{"max_order_margin_usdt": 9.9,`), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(path, future, future)

	if got := st.Get().MaxOrderMarginUSDT; got != 1.5 {
		t.Fatalf("配置写坏后 = %v，想要沿用上一份的 1.5（别被打回默认值）", got)
	}

	// 修好之后要能自动恢复
	writeStrategyFile(t, path, "1.3")
	if got := st.Get().MaxOrderMarginUSDT; got != 1.3 {
		t.Fatalf("修好后 = %v，想要 1.3", got)
	}
}

func TestStrategyStore_MissingFileDoesNotPanic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-there.json")
	st := NewStrategyStore(path, nil)
	if st.Get() == nil {
		t.Fatal("文件不存在时 Get() 返回了 nil，调用方会炸")
	}
	// 文件后来出现了 → 自动认到
	writeStrategyFile(t, path, "2.5")
	if got := st.Get().MaxOrderMarginUSDT; got != 2.5 {
		t.Fatalf("文件出现后 = %v，想要 2.5", got)
	}
}
