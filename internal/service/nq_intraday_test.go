package service

// nq_intraday_test.go —— Yahoo ^NDX 盘中解析的单测。
//
// 用一份手工构造的最小响应（覆盖真实响应里出现过的所有坑）：
//   · 同一分钟两条（前 null 后有值）→ last-wins
//   · h < l 的脏数据 → 丢弃
//   · 跨两天 → 按 UTC 天分组，Off 从当天 00:00 起算
import (
	"testing"
)

const yahooFixture = `{"chart":{"result":[{"meta":{"regularMarketTime":1790970024},
  "timestamp":[1790895000, 1790895000, 1790895060, 1790981400],
  "indicators":{"quote":[{
    "open":  [30535.84, 30504.45, 30504.45, 30804.89],
    "high":  [30546.42, 30511.91, 30511.91, 1.0],
    "low":   [30519.33, 30475.86, 30475.86, 2.0],
    "close": [30520.99, 30487.20, 30487.20, 30800.00],
    "volume":[0, 9764682, 9764682, null]
  }]}}],"error":null}}`

func TestParseYahooNDXMinutes(t *testing.T) {
	// 1790895000 = 2026-10-01 22:50:00 UTC，出现两次（真实响应里同一分钟两条）→ last-wins；
	// 1790895060 = 22:51:00；1790981400 = 2026-10-02 13:30 UTC（h<l 脏数据 → 整条丢弃）。
	got, err := parseYahooNDXMinutes([]byte(yahooFixture))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(got) != 1 { // 10-02 那条是脏数据被丢，只剩 10-01 一天
		t.Fatalf("天数 = %d，要 1（脏数据天应被整条丢弃）：%v", len(got), got)
	}
	list, ok := got[1790895000 - 1790895000%86400]
	if !ok {
		t.Fatalf("10-01 天没有分组")
	}
	if len(list) != 2 {
		t.Fatalf("10-01 记录数 = %d，要 2（重复分钟 last-wins）", len(list))
	}
	// 同一分钟重复：后值覆盖（22:50 取的是 30504 那条，不是 30535 那条）
	if list[0].Off != 82200 || list[0].O != 30504.45 {
		t.Fatalf("22:50 记录 = %+v，应为 Off=82200 / O=30504.45（last-wins）", list[0])
	}
	if list[1].Off != 82260 || list[1].V != 9764682 {
		t.Fatalf("22:51 记录 = %+v，应为 Off=82260 / V=9764682", list[1])
	}
}

func TestParseYahooNDXMinutes_Empty(t *testing.T) {
	if _, err := parseYahooNDXMinutes([]byte(`{"chart":{"result":[],"error":null}}`)); err == nil {
		t.Fatal("空结果应当报错")
	}
	if _, err := parseYahooNDXMinutes([]byte(`{"chart":{"result":null,"error":{"code":"Bad","description":"x"}}}`)); err == nil {
		t.Fatal("error 块应当报错")
	}
}

func TestParseYahooNDXMinutes_RealShape(t *testing.T) {
	// 用真实响应里出现过的形态（个别分钟整条 null）验证不炸
	body := []byte(`{"chart":{"result":[{"meta":{"regularMarketTime":1790970028},
	  "timestamp":[1790970000, 1790970060],
	  "indicators":{"quote":[{
	    "open":[null, 30804.892578125], "high":[null, 30804.892578125],
	    "low":[null, 30804.892578125], "close":[null, 30804.892578125],
	    "volume":[null, 0]}]}}],"error":null}}`)
	got, err := parseYahooNDXMinutes(body)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(got) != 1 {
		t.Fatalf("天数 = %d，要 1", len(got))
	}
}
