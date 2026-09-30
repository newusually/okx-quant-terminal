package repo

import "testing"

// TestIsValidKline 校验 K 线写入前的不变量。
//
// 背景：压测工具曾把合成数据写进生产 kline 表（价格 ~740、时间戳带毫秒尾巴），
// 每根假 K 线都满足下面这些「不变量」之外的条件，于是把 MA25/MA99/布林带
// 全算歪了。这里把不变量钉死，再犯就会被 UpsertKlines 直接拦下。
func TestIsValidKline(t *testing.T) {
	good := Kline{
		InstID: "ETH-USDT-SWAP", Bar: "15m",
		Ts: 1790766900000, // 整秒
		O: 2689.16, H: 2689.61, L: 2688, C: 2688.13, V: 18386.7,
	}
	if !IsValidKline(good) {
		t.Fatal("正常 K 线被误判为非法")
	}

	cases := []struct {
		name string
		k    Kline
	}{
		{"时间戳带毫秒尾巴（压测数据特征）", func() Kline {
			k := good
			k.Ts = 1790757736374 // 不是 1000 的整数倍
			return k
		}()},
		{"时间戳为 0", func() Kline { k := good; k.Ts = 0; return k }()},
		{"收盘价为 0", func() Kline { k := good; k.C = 0; return k }()},
		{"开盘价为负", func() Kline { k := good; k.O = -1; return k }()},
		{"最高价低于最低价", func() Kline { k := good; k.H = 2000; k.L = 3000; return k }()},
		{"合约名为空", func() Kline { k := good; k.InstID = ""; return k }()},
		{"周期为空", func() Kline { k := good; k.Bar = ""; return k }()},
	}
	for _, c := range cases {
		if IsValidKline(c.k) {
			t.Errorf("%s：应当被判为非法，却通过了", c.name)
		}
	}
}

// TestIsValidKlineRejectsBenchTs 单独把「毫秒尾巴」这条拎出来：
// 它是压测/合成数据唯一可靠的指纹，必须 100% 拦得住。
func TestIsValidKlineRejectsBenchTs(t *testing.T) {
	k := Kline{
		InstID: "ETH-USDT-SWAP", Bar: "15m",
		Ts: 1790757736374, // 16:42:16.374
		O: 738.3857696873154, H: 739.1241554570026,
		L: 737.6473839176281, C: 738.754962572159, V: 962.2208587150949,
	}
	if IsValidKline(k) {
		t.Fatal("带毫秒尾巴的合成 K 线没有被拦下")
	}
}
