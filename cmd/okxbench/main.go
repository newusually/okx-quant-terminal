package main

// cmd/okxbench —— MySQL 并发写入压测
//
// 目的：验证 478 个 USDT-SWAP 合约「同时」写库时 MySQL 的表现，
// 证明换成 MySQL 之后不再有 SQLite 那种「单写者排队」的瓶颈。
//
// 用法：
//   go run ./cmd/okxbench                       # 478 个合约并发，每个写 300 根 K 线
//   go run ./cmd/okxbench -contracts 478 -bars 300 -rounds 3
//   go run ./cmd/okxbench -keep                 # 压测完保留数据（默认压测完清理）
//
// 输出：每轮耗时、总行数、行/秒、P50/P95/P99 单合约耗时。

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"sync"
	"time"

	"finally-main/internal/repo"
)

func main() {
	var (
		mHost = flag.String("mysql-host", "127.0.0.1", "MySQL 主机")
		mPort = flag.Int("mysql-port", 3306, "MySQL 端口")
		mUser = flag.String("mysql-user", "okx", "MySQL 用户")
		mPass = flag.String("mysql-pass", "OkxQuant2026", "MySQL 密码")
		mDB   = flag.String("mysql-db", "okx", "MySQL 库名")

		maxContracts = flag.Int("contracts", 478, "并发合约数（最多取库里实际数量）")
		perContract  = flag.Int("bars", 300, "每个合约每个周期写多少根 K 线")
		rounds       = flag.Int("rounds", 3, "压测轮数")
		batchSize    = flag.Int("batch", 500, "单次批量写入行数上限")
		symbol       = flag.String("symbol", "BENCH-USDT-SWAP", "压测用的合约前缀（默认用真实合约）")
		useReal      = flag.Bool("real", true, "用库里真实合约污染方式压测；false 则全部写 BENCH- 假合约")
		keep         = flag.Bool("keep", false, "压测后保留数据")
		proxy        = flag.String("proxy", "", "保留参数（压测不联网）")
	)
	flag.Parse()
	_ = proxy

	mcfg := repo.DefaultMySQLConfig()
	mcfg.Host, mcfg.Port = *mHost, *mPort
	mcfg.User, mcfg.Password, mcfg.Database = *mUser, *mPass, *mDB
	mcfg.MaxOpenConns = 128 // 压测时连接池放大，避免池本身成为瓶颈
	mcfg.MaxIdleConns = 64
	mcfg.BatchSize = *batchSize

	fmt.Println("==============================================================")
	fmt.Println(" MySQL 并发写入压测 · 模拟 400+ 合约同时落库")
	fmt.Println("==============================================================")

	db, err := repo.OpenMySQL(mcfg)
	if err != nil {
		fmt.Printf("连接 MySQL 失败：%v\n", err)
		os.Exit(1)
	}
	defer db.Close()
	fmt.Printf(" MySQL 版本 : %s\n", db.ServerVersion())
	fmt.Printf(" 连接池     : %d\n", mcfg.MaxOpenConns)
	fmt.Printf(" 批量分片   : %d 行/次\n", mcfg.BatchSize)

	// ---- 选合约 ----
	var contracts []string
	if *useReal {
		ids, err := db.ListInstrumentIDs()
		if err != nil {
			fmt.Printf("读合约列表失败：%v\n", err)
			os.Exit(1)
		}
		contracts = ids
		if len(contracts) > *maxContracts {
			contracts = contracts[:*maxContracts]
		}
	}
	for len(contracts) < *maxContracts {
		contracts = append(contracts, fmt.Sprintf("%s%d", *symbol, len(contracts)))
	}
	fmt.Printf(" 并发合约数 : %d\n", len(contracts))
	fmt.Printf(" 每合约行数 : %d 根 × 6 周期 = %d 行\n", *perContract, *perContract*6)
	fmt.Printf(" 总写入量   : %d 行/轮\n", len(contracts)*(*perContract)*6)
	fmt.Printf(" 轮数       : %d\n", *rounds)
	fmt.Println("--------------------------------------------------------------")

	bars := []string{"1m", "3m", "5m", "15m", "1H", "4H"}
	now := time.Now().UnixMilli()

	for r := 1; r <= *rounds; r++ {
		var (
			wg       sync.WaitGroup
			mu       sync.Mutex
			total    int
			errCnt   int
			latency  = make([]time.Duration, 0, len(contracts))
			firstErr error
		)

		start := time.Now()
		for _, inst := range contracts {
			wg.Add(1)
			go func(inst string) {
				defer wg.Done()

				// 每个合约每个周期造 perContract 根 K 线
				rows := make([]repo.Kline, 0, (*perContract)*len(bars))
				px := 100.0 + rand.Float64()*1000
				for _, bar := range bars {
					for i := 0; i < *perContract; i++ {
						px *= 1 + (rand.Float64()-0.5)*0.002
						ts := now - int64((*perContract)-i)*60000
						rows = append(rows, repo.Kline{
							InstID: inst, Bar: bar, Ts: ts,
							O: px, H: px * 1.001, L: px * 0.999, C: px * 1.0005,
							V: rand.Float64() * 1000,
						})
					}
				}

				t0 := time.Now()
				n, err := db.UpsertKlines(rows)
				d := time.Since(t0)

				mu.Lock()
				total += n
				if err != nil {
					errCnt++
					if firstErr == nil {
						firstErr = err
					}
				}
				latency = append(latency, d)
				mu.Unlock()
			}(inst)
		}
		wg.Wait()
		elapsed := time.Since(start)

		sort.Slice(latency, func(i, j int) bool { return latency[i] < latency[j] })
		pick := func(p float64) time.Duration {
			if len(latency) == 0 {
				return 0
			}
			i := int(float64(len(latency)-1) * p)
			return latency[i]
		}

		fmt.Printf("[第 %d 轮] 并发 %d  总写入 %d 行  耗时 %.3fs  吞吐 %.0f 行/秒\n",
			r, len(contracts), total, elapsed.Seconds(), float64(total)/elapsed.Seconds())
		fmt.Printf("         单合约写入延迟 P50=%.0fms  P95=%.0fms  P99=%.0fms  MAX=%.0fms\n",
			pick(0.50).Seconds()*1000, pick(0.95).Seconds()*1000,
			pick(0.99).Seconds()*1000, pick(1.0).Seconds()*1000)
		if errCnt > 0 {
			fmt.Printf("         ⚠ 失败 %d 个合约：%v\n", errCnt, firstErr)
		}
	}

	// ---- 池状态 ----
	st := db.PoolStats()
	fmt.Println("--------------------------------------------------------------")
	fmt.Printf("连接池收尾：open=%v inUse=%v idle=%v  累计等待=%v 次 %.2fs\n",
		st["open"], st["inUse"], st["idle"], st["waitCount"], st["waitSeconds"])

	if !*keep {
		fmt.Println("清理压测数据…")
		cut := now - int64(*perContract+10)*60000
		n, err := db.CleanupBench(cut)
		if err != nil {
			fmt.Printf("清理失败：%v\n", err)
		} else {
			fmt.Printf("已删除 %d 行压测 K 线\n", n)
		}
	}
	fmt.Println("压测结束")
}
