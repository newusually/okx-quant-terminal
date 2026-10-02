package main

// cmd/okxweb —— OKX 全合约量化终端（网页客户端 + 数据服务）
//
// 三层架构下的位置：这是「接口层」的可执行入口。
//
//	internal/repo      数据层：MySQL 仓储 + OKX SDK
//	internal/service   业务层：回补、行情、信号、账户
//	internal/handler   接口层：HTTP 路由（本文件起的就是它）
//
// 干三件事：
//   1. 连上本机 MySQL 并把表建好（不需要装 Python，也不需要 CGO）
//   2. 从 OKX 回补 K 线（1m/3m/5m/15m 四个周期，默认 10 天）到 MySQL，并持续增量更新
//      （2026-10-01 二期：1m/3m/5m 已恢复 —— 四个周期都参与开仓，
//       保留窗口统一收到 10 天，由每日凌晨任务 DROP PARTITION 清理）
//   3. 起一个网页服务，用 AJAX + TradingView 图表把行情、持仓、盈亏、历史都展示出来
//
// 端口约定：
//   Go 网页服务监听 127.0.0.1:8090（只对本机）
//   Apache 监听 0.0.0.0:80，反向代理到 8090 对外提供访问
//
// 用法：
//   go run ./cmd/okxweb                        # 默认监听 127.0.0.1:8090
//   go run ./cmd/okxweb -addr 127.0.0.1:9000 -days 45
//   go run ./cmd/okxweb -focus BTC-USDT-SWAP,ETH-USDT-SWAP
//   go run ./cmd/okxweb -init-only             # 只建库建表 + 同步一次行情，然后退出
//   go run ./cmd/okxweb -no-backfill           # 只起网页 + 实时行情
//   go run ./cmd/okxweb -pprof-addr ""         # 关掉火焰图端口
//
// 火焰图在独立的 127.0.0.1:8091（见 startPprof）：绝不能被 Apache 反代到公网。

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"finally-main/internal/conf"
	"finally-main/internal/handler"
	"finally-main/internal/logx"
	"finally-main/internal/model"
	"finally-main/internal/perf"
	"finally-main/internal/rbac"
	"finally-main/internal/repo"
	"finally-main/internal/service"
)

// ---------------------------------------------------------------------------
// 命令行参数（提到包级：服务模式和控制台模式共用同一套）
// ---------------------------------------------------------------------------

var (
	addr    = flag.String("addr", "127.0.0.1:8090", "网页监听地址（Apache 反代到这里）")
	proxy   = flag.String("proxy", "", "HTTP/SOCKS5 代理，如 http://127.0.0.1:7890；留空直连")
	// ★ 2026-10-01 二期：默认天数改成 0 = 「跟着 kline_retain_days 走」（当前 30 天）。
	//   写死 365 会让首次回补去拉一年 —— 而库里只留 10 天，
	//   拉回来的 355 天转头就被每日任务删掉，纯属白烧 OKX 限频与磁盘 IO。
	days    = flag.Int("days", 0, "K 线回补天数（0 = 用配置里的 kline_retain_days，当前 30 天）")
	focus   = flag.String("focus", "", "启动即回补的合约，逗号分隔；留空 = 按成交额取 TopN")
	focusN  = flag.Int("focusn", 8, "focus 留空时取成交额前 N 名")
	workers = flag.Int("workers", 10, "回补并发数（受 OKX 限频约束，10 已接近上限）")
	bars    = flag.String("bars", "", "要回补的周期，逗号分隔；留空 = 用 model.EnabledBars（1m,3m,5m,15m）")
	rtSec   = flag.Int("rt", 5, "实时行情落库间隔（秒）")
	scope   = flag.String("backfill-scope", "plan",
		"回补范围：plan（默认，全部 live 合约 × 四个周期）| tradeable | live（看磁盘）| focus | none")
	minFree = flag.Int("min-free-mb", 800, "剩余磁盘低于此值就暂停回补（0=不检查）")

	// 性能诊断：pprof 火焰图。单开端口、只绑回环，**绝不能挂在 8090 上**
	// （8090 被 Apache 反代到公网 80，挂上去 = 把 goroutine 栈暴露给全世界）。
	pprofAddr = flag.String("pprof-addr", "127.0.0.1:8091",
		"pprof 火焰图监听地址（只允许回环地址，留空=关闭）。独立端口，不走 Apache")

	// MySQL 连接参数：口令**不再有默认值**（原来这里写死明文，仓库一公开就泄漏）。
	// 留空 = 走 internal/conf/secret.go 的解析链：
	//     环境变量 OKX_MYSQL_PASS → <项目根>\.mysql-pass → 空
	// 要设置 / 轮换口令，双击 scripts\set_db_pass.bat。
	mHost  = flag.String("mysql-host", "127.0.0.1", "MySQL 主机")
	mPort  = flag.Int("mysql-port", 3306, "MySQL 端口")
	mUser  = flag.String("mysql-user", "", "MySQL 用户（留空 = OKX_MYSQL_USER 或默认 okx）")
	mPass  = flag.String("mysql-pass", "", "MySQL 密码（留空 = OKX_MYSQL_PASS 或 .mysql-pass 文件）")
	mDB    = flag.String("mysql-db", "", "MySQL 库名（留空 = 默认 okx）")
	mOpen  = flag.Int("mysql-maxopen", 64, "MySQL 连接池上限")
	mBatch = flag.Int("mysql-batch", 500, "批量写入分片大小")

	initOnly = flag.Bool("init-only", false, "只建库建表 + 同步一次合约与行情，然后退出")
	noBf     = flag.Bool("no-backfill", false, "不做历史回补（只起网页 + 实时行情）")

	// ---- 自动交易（纯 Go，不需要人盯着）----
	noTrade  = flag.Bool("no-trade", false, "关掉自动交易：只跑数据 + 网页，不下任何单")
	exitSec  = flag.Int("exit-sec", 3, "止盈巡检间隔（秒）—— 实时盯浮盈，够线就平")
	entrySec = flag.Int("entry-sec", 60, "买入信号扫描间隔（秒）—— 全市场扫买入信号")
	liveBar  = flag.String("live-bar", "", "自动交易用哪个周期判买卖（留空 = 读配置里的 bar）")

	// ---- 一次性数据清理（记录表 30 天 / 日志 30 天红线）----
	//   bin\okxweb.exe -cleanup-dry   只看超期数据有多少，一行不删
	//   bin\okxweb.exe -cleanup       真删（常驻服务里每月 1 号也会自动跑）
	cleanupNow  = flag.Bool("cleanup", false, "立即执行一次记录表/日志清理（30 天红线），然后退出")
	cleanupDry  = flag.Bool("cleanup-dry", false, "预演：只统计超期数据量，不删除任何东西")

	// ---- 月度 / 年度维护任务（2026-10-01 新增）----
	// 归档、磁盘守卫、清日志、清回收站会动真格，所以默认走预演。
	//   bin\okxweb.exe -maint-dry     月度任务预演（只报告不删）
	//   bin\okxweb.exe -maint         月度任务真跑（含归档 + 清回收站）
	//   bin\okxweb.exe -maint-yearly  年度 K 线清理（红线 kline_retain_days=365）
	maintNow    = flag.Bool("maint", false, "立即执行一次月度维护（归档上月 + 记录表清理 + 日志清理 + 磁盘守卫 + 清回收站），然后退出")
	maintDry    = flag.Bool("maint-dry", false, "预演月度维护：只报告会做什么，不删任何东西")
	maintYearly = flag.Bool("maint-yearly", false, "立即执行一次年度清理（删除早于 kline_retain_days 的 K 线），然后退出")
	// ★ 年度清理是 DROP PARTITION，真删不可逆，所以必须给一个预演开关。
	//   少了它，任何「先预演、再确认」的批处理都会把预演那一步变成真删。
	maintYearlyDry = flag.Bool("maint-yearly-dry", false, "预演年度清理：只报告会 DROP 哪些分区，不删任何东西")

	// ---- 每日 K 线清理（2026-10-01 二期新增）----
	//   bin\okxweb.exe -maint-daily-dry  预演：只报告会 DROP 哪些日分区
	//   bin\okxweb.exe -maint-daily      真删（常驻服务里每天凌晨也会自动跑）
	maintDaily    = flag.Bool("maint-daily", false, "立即执行一次每日 K 线清理（删除早于 kline_retain_days 的 K 线），然后退出")
	maintDailyDry = flag.Bool("maint-daily-dry", false, "预演每日 K 线清理：只报告会 DROP 哪些分区，不删任何东西")

	// ---- 月度归档导出 ----
	//   bin\okxweb.exe -archive 2026-09          导出到默认 archive/ 目录
	//   bin\okxweb.exe -archive 2026-09 -archive-dir D:\bak
	archiveYM  = flag.String("archive", "", "导出指定月份（YYYY-MM）的 15m K 线到归档目录，然后退出")
	archiveDir = flag.String("archive-dir", "", "归档目录（留空 = 读配置 archive_dir，默认项目根 archive/）")

	// ---- Windows 服务 ----
	installSvc   = flag.Bool("install", false, "注册为 Windows 服务 OKXWeb（需管理员，幂等）")
	uninstallSvc = flag.Bool("uninstall", false, "停止并卸载 Windows 服务 OKXWeb（需管理员）")

	// ---- 交易链路自检 ----
	// 传合约名就探测持仓模式 + 试设杠杆（不下单），用来确认下单参数会不会被 OKX 拒。
	// 起因：账户切到双向持仓后 posSide=net 被 OKX 拒（51000），表现是
	// 「图上有信号、后台也在扫，但一条买入记录都没有」。
	probeInst = flag.String("probe", "", "交易链路自检：探测持仓模式并试设杠杆（传合约名，如 ETH-USDT-SWAP）")
	acctDiag  = flag.Bool("acct", false, "账户只读诊断：保证金模式 acctLv / 持仓模式 posMode / 顶层 upl / 持仓汇总 upl")
	partition = flag.Bool("partition", false, "一次性把 kline 改造成分区表（重建整表，务必先停引擎）")
	// 分区方案从「纯按月」升级成「冷区按月 + 热区按周」时要强制重建。
	// 已经建过月度分区的库，-partition 会因为「已是分区表」直接跳过。
	repartition = flag.Bool("repartition", false, "强制重建分区方案为「冷区按月 + 热区按天」（重建整表，务必先停引擎）")

	// 注册服务时 CreateService 会带上 -service；这里必须显式认领，
	// 否则 flag.Parse() 会当成未知参数直接打 usage 退出。
	_ = flag.Bool("service", false, "内部使用：由服务控制器拉起时自动带上，勿手动指定")
)

func main() {
	flag.Parse()

	// ---- 服务安装 / 卸载：干完就退出，不启动业务 ----
	if *installSvc {
		if err := installService(); err != nil {
			fmt.Printf("✘ 安装服务失败：%v\n", err)
			os.Exit(1)
		}
		return
	}
	if *uninstallSvc {
		if err := uninstallService(); err != nil {
			fmt.Printf("✘ 卸载服务失败：%v\n", err)
			os.Exit(1)
		}
		return
	}

	// ---- 一次性数据清理：跑完就退出 ----
	//
	// 常驻服务里维护任务是自动的（启动后 90 秒起，每 30 分钟巡检一次，
	// 按 meta 表记的「上次跑的月份/年份」决定要不要补跑），这几个开关是给
	// 手工核对用的：先 -cleanup-dry / -maint-dry 看看会删多少，再真跑。
	if *cleanupNow || *cleanupDry {
		rep, err := service.RunRetentionCleanup(*cleanupDry)
		fmt.Print(service.RetentionText(rep))
		if err != nil {
			fmt.Printf("✘ %v\n", err)
			os.Exit(1)
		}
		if *cleanupDry {
			fmt.Println("（预演模式：什么都没删。去掉 -cleanup-dry 才会真删）")
		}
		return
	}

	// ---- 月度维护：归档 + 记录表清理 + 日志清理 + 磁盘守卫 + 清回收站 ----
	if *maintNow || *maintDry {
		rep, err := service.RunMonthlyMaintenance(*maintDry)
		fmt.Print(service.MaintenanceText(rep))
		if err != nil {
			fmt.Printf("✘ %v\n", err)
			os.Exit(1)
		}
		if *maintDry {
			fmt.Println("（预演模式：什么都没删。去掉 -maint-dry 才会真跑）")
		}
		return
	}

	// ---- 每日 K 线清理：删掉早于 KlineRetainDays（当前 30 天）的 K 线 ----
	//  用户口径「只能查询保存最近 10 天数据 + 自动每天凌晨删除数据一次」。
	//  常驻服务里每天凌晨自动跑一次，这两个开关是给手工核对用的。
	if *maintDaily || *maintDailyDry {
		rep, err := service.RunDailyMaintenance(*maintDailyDry)
		fmt.Print(service.MaintenanceText(rep))
		if err != nil {
			fmt.Printf("✘ %v\n", err)
			os.Exit(1)
		}
		if *maintDailyDry {
			fmt.Println("（预演模式：什么都没删。去掉 -maint-daily-dry 才会真删）")
		}
		return
	}

	// ---- 年度清理：删掉早于 KlineRetainDays 的 K 线 ----
	if *maintYearly || *maintYearlyDry {
		rep, err := service.RunYearlyMaintenance(*maintYearlyDry)
		fmt.Print(service.MaintenanceText(rep))
		if err != nil {
			fmt.Printf("✘ %v\n", err)
			os.Exit(1)
		}
		if *maintYearlyDry {
			fmt.Println("（预演模式：什么都没删。去掉 -maint-yearly-dry 才会真删）")
		}
		return
	}

	// ---- 月度归档导出：把某个月的 15m K 线导成 gzip 分片 ----
	if *archiveYM != "" {
		dir := *archiveDir
		if dir == "" {
			dir = service.ArchiveDir()
		}
		man, err := service.ExportMonth(*archiveYM, dir)
		if err != nil {
			fmt.Printf("✘ 归档 %s 失败：%v\n", *archiveYM, err)
			os.Exit(1)
		}
		fmt.Print(service.ArchiveText(man))
		p, werr := service.WriteManifest(dir, man)
		if werr != nil {
			fmt.Printf("✘ 清单写入失败：%v\n", werr)
			os.Exit(1)
		}
		fmt.Printf("清单：%s\n", p)
		fmt.Printf("目录：%s\n", dir)
		return
	}

	// ---- 账户只读诊断：打印 acctLv / posMode / 顶层 upl / 持仓汇总 upl ----
	// 用来回答「为什么顶栏浮盈是 0」这类问题，全走原始字段，不猜。
	if *acctDiag {
		lines, err := service.ProbeAccountDiag()
		for _, ln := range lines {
			fmt.Println(ln)
		}
		if err != nil {
			fmt.Printf("✘ %v\n", err)
			os.Exit(1)
		}
		return
	}

	// ---- 交易链路自检：探测持仓模式 + 试设杠杆，干完就退出 ----
	if *probeInst != "" {
		mode, err := service.ProbeTrading(*probeInst, 0)
		fmt.Printf("账户持仓模式：%s\n", mode)
		if err != nil {
			fmt.Printf("✘ %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✔ %s 设杠杆成功 —— 下单参数会被 OKX 接受，有信号就会真实成交\n", *probeInst)
		return
	}

	// ---- 一次性：把大表改造成分区表 ----
	//
	// 这不是启动路径上的动作：把整张 kline 从「无分区」变成「有分区」，
	// 或改分片粒度（按周 → 按天），MySQL 只能 ALGORITHM=COPY，
	// 全程 LOCK=SHARED 禁写。所以必须停机做：
	//     net stop OKXWeb
	//     bin\okxweb.exe -repartition
	//     net start OKXWeb
	// 之后每天的「补新分区」是 INPLACE/LOCK=NONE，由启动流程自动完成。
	if *partition || *repartition {
		mcfg := repo.DefaultMySQLConfig()
		applyMySQLFlags(&mcfg)
		// 整表重建要跑几分钟，必须关掉 DSN 的读超时（否则 60 秒被掐断）
		mcfg.LongDDL = true
		db, err := repo.OpenMySQL(mcfg)
		if err != nil {
			fmt.Printf("✘ 连接 MySQL 失败：%v\n", err)
			os.Exit(1)
		}
		defer db.Close()

		mode := "按需分区（已是分区表则跳过）"
		if *repartition {
			mode = "强制重建分区方案（冷区按月 + 热区按天）"
		}
		fmt.Printf("== 分区改造：%s ==\n", mode)
		ok := true
		for _, t := range repo.PartitionedTables {
			if err := db.MigrateToPartition(t, *repartition); err != nil {
				fmt.Printf("✘ %s：%v\n", t, err)
				ok = false
			}
		}
		fmt.Println("-- 结果 --")
		for _, s := range db.PartitionSummary() {
			if s["error"] != nil {
				fmt.Printf("  · %-14s ✘ %v\n", s["table"], s["error"])
				continue
			}
			names, _ := s["names"].([]string)
			fmt.Printf("  · %-14s 分区 %d 个（日 %v / 月 %v / 老周分区 %v / 兜底 %v）：%s\n",
				s["table"], len(names), s["daily"], s["monthly"], s["legacyWeeks"], s["old"],
				strings.Join(names, " "))
		}
		fmt.Println("-- 各分区占用 --")
		for _, t := range repo.PartitionedTables {
			detail, err := db.PartitionDetail(t)
			if err != nil {
				fmt.Printf("  · %s：%v\n", t, err)
				continue
			}
			var total float64
			for _, d := range detail {
				total += d["dataMB"].(float64)
			}
			fmt.Printf("  · %s 共 %.1f MB\n", t, total)
			for _, d := range detail {
				fmt.Printf("      %-12s %8.1f MB  bound=%v\n", d["name"], d["dataMB"], d["bound"])
			}
		}
		if !ok {
			os.Exit(1)
		}
		fmt.Println("✔ 分区改造完成")
		return
	}

	// ---- 被 SCM 拉起：走服务协议（没有黑窗口，开机自启，崩了自动重启）----
	if runningAsService() {
		if err := runAsService(runApp); err != nil {
			os.Exit(1)
		}
		return
	}

	// ---- 普通控制台方式：Ctrl-C 退出 ----
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runApp(ctx); err != nil {
		log.Printf("%v", err)
		os.Exit(1)
	}
}

// runApp 真正的启动流程：连库 → 回补 → 网页 → 策略引擎。
// 阻塞到 ctx 结束（服务模式由 SCM 的 Stop 触发，控制台模式由 Ctrl-C 触发）。
func runApp(ctx context.Context) error {
	root := projectRoot()

	mcfg := repo.DefaultMySQLConfig()
	applyMySQLFlags(&mcfg)
	mcfg.MaxOpenConns, mcfg.BatchSize = *mOpen, *mBatch

	fmt.Println("==============================================================")
	fmt.Println(" OKX 全合约量化终端 · 纯 Go 后端 + MySQL（无 Python / 无 CGO）")
	fmt.Println("==============================================================")
	fmt.Printf(" 项目根目录 : %s\n", root)
	fmt.Printf(" 数据库     : mysql://%s@%s:%d/%s（连接池 %d）\n",
		mcfg.User, mcfg.Host, mcfg.Port, mcfg.Database, mcfg.MaxOpenConns)
	// 只报「口令从哪来」，不报口令本身 —— 日志经常被人截图贴出去。
	fmt.Printf(" 口令来源   : %s\n", repo.MySQLSecretSource())
	if mcfg.Password == "" {
		fmt.Printf("\n%s\n\n", repo.MySQLHint())
	}
	fmt.Printf(" 回补天数   : %d%s\n", resolveBackfillDays(*days), daysNote(*days))
	fmt.Printf(" 回补周期   : %s\n", backfillBarsText(*bars))
	fmt.Printf(" 回补并发   : %d\n", *workers)
	fmt.Printf(" 网页地址   : http://%s\n", *addr)
	fmt.Printf(" 对外入口   : http://<本机IP>/（由 Apache 80 反代）\n")
	fmt.Println("--------------------------------------------------------------")

	// ---- 1. 数据库（数据层）----
	db, err := repo.OpenMySQL(mcfg)
	if err != nil {
		log.Fatalf("连接 MySQL 失败：%v\n（请先运行 scripts\\start_all.bat 启动 MySQL）", err)
	}
	defer db.Close()
	fmt.Printf("[DB] MySQL %s 已连接，数据库 %s\n", db.ServerVersion(), db.DBName())

	// ★ 大表行数缓存（关键性能设施）。
	//   `COUNT(*) FROM kline` 在 400 万行时要 7.76 秒，而前端每 2 秒就轮询
	//   /api/account（会走到行数统计）。必须在监听端口前把缓存挂上，
	//   让所有接口只读内存值 —— 详见 internal/repo/rowcount.go 的复盘。
	db.StartRowCountRefresher()

	// ★ K 线覆盖情况缓存（第二个 CPU 大户）。
	//   「回补进度」页每 2 秒轮询 /api/backfill，而它原来会为每个合约各发一条
	//   GROUP BY（476 条 × 20 个分区），单请求 10~20 秒 → 3 条并发就把
	//   mysqld 的一颗核跑满。现在改成一条聚合 SQL + 10 分钟缓存，
	//   请求路径只读内存。详见 internal/repo/coverage.go。
	db.StartCoverageRefresher()

	// ★ 分区自维护：补齐未来月份的分区。
	//   ADD PARTITION 在 MySQL 8 是 INPLACE/LOCK=NONE，不阻塞读写，可以放心自动跑；
	//   若表还没分区，这里只打提示（首次改造要重建整表，必须走 -partition 开关停机做）。
	go func() {
		time.Sleep(15 * time.Second)
		db.EnsurePartitions()

		// 交易事件流水就绪性检查：trade_event 是后加的表，
		// 老成交要先从 trade 表搬过来，不然图上看不到历史买入标记。
		if n, err := db.BackfillTradeEvents(); err != nil {
			logx.Logf("WARN", "[EVENT] 交易事件回填失败：%v", err)
		} else {
			logx.Logf("INFO", "[EVENT] 交易事件流水就绪：共 %d 条（开仓/加仓/平仓）", n)
		}
	}()

	// ---- 1.5 自愈：清掉不符合不变量的脏 K 线 ----
	// 时间戳非整秒 / 价格非正的，一定是外部工具或异常写入塞进来的。
	// 教训：压测工具曾把 28.68 万行合成数据写进生产表，把每个合约的
	// MA25/MA99/布林带全算歪 —— 这里每次都兜一道底。
	//
	// ★ 放到后台、延迟 45 秒再跑：这个 DELETE 用不上索引，是纯全表扫描，
	//   kline 到 400 万行时要跑几十秒。以前同步执行，服务得等它跑完才
	//   ListenAndServe，表现就是「启动卡死、网页打不开」。
	go func() {
		time.Sleep(45 * time.Second)
		if n, err := db.PurgeBadKlines(); err != nil {
			logx.Logf("WARN", "[DB] 脏 K 线自检失败：%v", err)
		} else if n > 0 {
			logx.Logf("INFO", "[DB] 自愈：清掉 %d 行非法 K 线（时间戳非整秒或价格非正）", n)
		}
	}()

	tabs, err := db.Tables()
	if err != nil {
		log.Fatalf("读表结构失败：%v", err)
	}
	fmt.Printf("[DB] 表已就绪（%d 张）：%s\n", len(tabs), strings.Join(tabs, ", "))
	counts, _ := db.TableCounts()
	for _, t := range tabs {
		fmt.Printf("     · %-14s %8d 行\n", t, counts[t])
	}

	// ---- 2. 策略配置（热插拔：改 configs/okx_strategy.json 即刻生效）----
	//
	// ★ 口径只有一处真源：configs/okx_strategy.json。
	//   Go 里的默认值只是「文件缺失 / 解析失败」的兜底，不构成任何要求。
	//   strategyStore.Get() 每次都会比对文件，改了自动重读 —— 不用重启服务。
	cfgPath := service.StrategyConfigPath(root)
	cfgLog := func(format string, args ...any) {
		fmt.Printf("%s [CFG] %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
		logx.Logf("INFO", "[CFG] "+format, args...)
	}
	strategyStore := service.NewStrategyStore(cfgPath, cfgLog)
	strategy := strategyStore.Get()
	fmt.Printf("[CFG] 策略参数：%s，共振阈值 %d/8，dry_run=%v\n",
		strategy.MarginText(), strategy.ScoreThreshold, strategy.DryRun)
	// ★ 四期口径：止盈 +0.3% **保留**，只关掉了布林上轨（原「秒进秒出」的元凶），
	//   超时收紧到 1 小时。措辞交给 service.ExitText 统一生成 ——
	//   它只列**真正开着**的通道，别在这里写死一句「已关闭 X」，
	//   那种硬编码在口径再变一次时立刻变成假的（这里就刚错过一次）。
	fmt.Printf("[CFG] 出场规则：%s\n",
		service.ExitText(strategy.Exit.TakeProfitPct, strategy.Exit.BollUpperExit, strategy.Exit.MaxHoldMinutes))
	// ★ 三期新增的入场条件必须和共振阈值一起打出来 ——
	//   只看到一个数字，会以为「score 够了就买」。
	riseRule := "K线涨幅条件已关闭"
	if mr := strategy.MinBarRisePct(); mr > 0 {
		riseRule = fmt.Sprintf("触发那根 K 线涨幅 > %.2f%%", mr)
	}
	fmt.Printf("[CFG] 买入/加仓条件：score ≥ %d 且 %s（加仓与买入走同一判据）\n",
		strategy.ScoreThreshold, riseRule)
	fmt.Printf("[CFG] 准入上限：最小一手保证金 ≤ %.2fU 才进 symbolList（品类不限；改 JSON 即时生效，无需重启）\n",
		strategy.MaxOrderMarginUSDT)

	// ---- 3. 数据服务（业务层）----
	feed := service.NewDataFeed(*proxy)

	cfg := service.DefaultBackfillConfig()
	cfg.Days = resolveBackfillDays(*days)
	cfg.Workers = *workers
	cfg.RealtimeSec = *rtSec
	cfg.FocusN = *focusN
	cfg.Scope = *scope
	cfg.MinFreeMB = *minFree
	cfg.RootDir = root
	// 「续最新一根」只做扫描真正会用的那批（top_n_by_volume，默认 80）。
	//
	// 不截的话，四周期 × 169 个可交易合约 = 676 条/轮，最少 67.6 秒
	// —— 超过 1m K 线的一分钟，1m 本地永远不新鲜，扫描只能一直回退网络，
	// 和续 K 线抢同一把 OKX 闸门，把 eng.scan 顶到 100 秒以上、出场巡检饿死。
	// 详见 service.BackfillConfig.RefreshTopNFn 的注释。
	//
	// 走回调而不是取一次定值：top_n_by_volume 是热插拔的，启动读一次会跟 JSON 脱节。
	cfg.RefreshTopNFn = func() int { return strategyStore.Get().TopNByVolume }
	// Light 铺底至少要铺到扫描读本地的门槛（min_candles，默认 400）。
	// 单次 /market/candles 上限只有 300，只铺 300 的话 1m/3m/5m 永远不够长、
	// 每轮都回退网络，和回补抢同一把闸门。详见 service.BackfillConfig.MinCandlesFn。
	//
	// ★ 必须走 conf.LoadConfig()，不能自己再读一份 JSON 或另加字段 ★
	//   扫描器 Scan(cfg *conf.Config, …) 用的就是它（每次调用都重新加载、带 mtime 缓存）。
	//   两处各取一份口径 = 迟早对不上，这是本项目反复踩过的坑。
	cfg.MinCandlesFn = func() int { return conf.LoadConfig().MinCandles }
	if *focus != "" {
		for _, s := range strings.Split(*focus, ",") {
			if s = strings.TrimSpace(s); s != "" {
				cfg.FocusInst = append(cfg.FocusInst, s)
			}
		}
	}
	validBars := []string{}
	for _, s := range strings.Split(*bars, ",") {
		if s = strings.TrimSpace(s); s != "" && service.IsSupportedBar(s) {
			validBars = append(validBars, s)
		}
	}
	if len(validBars) > 0 {
		cfg.Bars = validBars
	}

	bf := service.NewBackfillManager(db, feed, cfg, func(format string, args ...any) {
		fmt.Printf("%s [DATA] %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
	})

	if err := bf.SyncInstruments(); err != nil {
		fmt.Printf("[DATA] ⚠ 合约列表同步失败：%v\n", err)
	} else {
		insts, _ := db.ListInstruments()
		fmt.Printf("[DATA] 合约列表已入库：%d 个 USDT 永续\n", len(insts))
	}
	if err := bf.SyncTickers(); err != nil {
		fmt.Printf("[DATA] ⚠ 行情同步失败：%v\n", err)
	} else {
		fmt.Println("[DATA] 行情快照已入库")
	}

	// ---- 2.5 合约准入过滤（哪些能买）----
	//   不买刚上线 + 不买要下线 + 最小一手保证金 ≤ 准入上限
	//   （上限来自 configs/okx_strategy.json 的 max_order_margin_usdt）
	//   ★ 三期起不再按品类排除：用户口径「取消美股 etf 不做的功能，
	//     只要买入上限小于 1U 就做」。
	service.SetAnnounceCacheDB(db)
	applyUniverseFilter(db, bf, strategyStore, resolveBackfillDays(*days))

	if *initOnly {
		fmt.Println("[DB] -init-only：建库建表 + 准入过滤完成，退出")
		return nil
	}

	if !*noBf && *scope != "none" {
		if err := bf.Start(ctx); err != nil {
			fmt.Printf("[DATA] ⚠ 数据服务启动异常：%v\n", err)
		}

		// ---- 3.5 准入过滤不是一次性的 ----
		//   (a) 外部原因：新币上线会老过 30 天、成交额会塌、公告会新出下线通知，
		//       所以每 30 分钟兜底重跑一次，把结论刷新回 inst.tradeable。
		//       （公告黑名单自身有 24h 缓存，重跑不会打爆 OKX 接口。）
		//   (b) ★ 配置热插拔 ★：configs/okx_strategy.json 一被改动，
		//       立刻重算一遍准入 —— 这样改 max_order_margin_usdt 就能
		//       当场改掉 symbolList，不用重启 OKXWeb。
		//       用内容 sha256 判定（不是 mtime），编辑器「另存为」不会误触发。
		go strategyStore.Watch(ctx, 2*time.Second, func(st *service.StrategyConfig) {
			fmt.Printf("[CFG] 检测到策略配置变更 → 立刻重算合约准入（symbolList）\n")
			applyUniverseFilter(db, bf, strategyStore, *days)
		})

		go func() {
			t := time.NewTicker(30 * time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					applyUniverseFilter(db, bf, strategyStore, *days)
				}
			}
		}()
	}

	// ---- 4. 网页服务（接口层）----
	//
	// ★★ 2026-10-02 修订：logf 必须同时写日志文件，不能只 fmt.Printf ★★
	//   第 597 行原来是纯 stdout（自定义打印格式），而 SCM 拉起的 Windows 服务
	//   **没有控制台，stdout 被系统丢弃** → 管理员发码 / 登录 / 登录失败
	//   这些安全事件全都写进了空气，日志文件里一条都查不到。
	//   实测踩到：用户在网页点「发送验证码」收不到邮件，
	//   我翻了 1.6 万行服务日志，既看不到「发出去了」也看不到「失败了」，
	//   连「有人请求过发码」都看不到 —— 只能靠猜。
	//   现在改成 logx.Logf（它内部就是 stdout + 文件 + runlog 三写），
	//   前缀 [WEB] 保留，便于和引擎日志区分。
	srv := handler.NewServer(db, feed, bf, strategyStore, handler.AssetsDir(root), root,
		func(format string, args ...any) {
			logx.Logf("INFO", "[WEB] "+format, args...)
		})

	// ---- 4.5 管理员鉴权（★ 2026-10-02 八期）----
	//
	//   管理台能改真实下单口径，所以要求「邮箱验证码 + 会话」。
	//   验证码发到唯一授权邮箱（硬编码在 internal/rbac，不放进可热插拔的 JSON ——
	//   那是「谁能改我的钱」的根，不能变成配置项）。
	//
	//   SMTP 凭据来源：环境变量 → 工作目录 .smtp-pass（格式 邮箱:授权码）。
	//   拿不到就只记一条日志，服务照常起 —— 只是管理台会明确报
	//   「邮件服务未配置」，而不是静默失败让人等一封永远不来的邮件。
	//
	//   ★★ 2026-10-02 修订：横幅必须同时进日志文件 ★★
	//   原来只 fmt.Printf 到 stdout，而 SCM 拉起的 Windows 服务**没有控制台**，
	//   stdout 直接丢弃 → 日志里既没有「已就绪」也没有「未配置」，
	//   排查「点了发送验证码收不到邮件」时只能靠猜。
	//   实测踩到：服务日志 1.6 万行里一条 [MAIL] 都没有。
	//   现在两边都写，并且把凭据**来源路径**也打出来 ——
	//   工作目录不对时，一眼就能看出它到底读了哪个文件。
	{
		smtpCfg, ok := rbac.LoadSMTPConfig(root)
		if ok {
			srv.SetMailer(rbac.NewSMTPMailer(smtpCfg, func(format string, args ...any) {
				logx.Logf("WARN", "[MAIL] "+format, args...)
			}))
			msg := fmt.Sprintf("[MAIL] 管理员验证码发送已就绪（%s:%d，发件人 %s，凭据来源 %s，工作目录 %s）",
				smtpCfg.Host, smtpCfg.Port, smtpCfg.User, smtpCfg.Source, cwdOf())
			fmt.Print(msg + "\n")
			logx.Logf("INFO", msg)
		} else {
			msg := fmt.Sprintf("[MAIL] ⚠ 未配置 SMTP 凭据 → 管理台登录不可用。"+
				"设置 OKX_SMTP_USER/OKX_SMTP_PASS 环境变量，或在工作目录放 .smtp-pass（内容：邮箱:授权码）。"+
				"当前 root=%s 工作目录=%s", root, cwdOf())
			fmt.Print(msg + "\n")
			logx.Logf("WARN", msg)
		}
	}
	// 保存配置成功后立刻重算合约准入（改金额 / 上限会影响哪些合约买得起）
	srv.SetConfigSavedHook(func() {
		applyUniverseFilter(db, bf, strategyStore, *days)
	})


	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	webErr := make(chan error, 1)
	go func() {
		fmt.Printf("\n✔ 客户端已启动：http://%s\n\n", *addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			webErr <- err
		}
	}()

	// ---- 4.1 pprof 火焰图（只绑回环，独立端口）----
	//
	// 为什么不开在 8090 上：8090 被 Apache 反代到公网 80，挂上去等于把
	// /debug/pprof 暴露给全世界（里面能读到 goroutine 栈、命令行、堆快照）。
	// 所以单开一个只绑 127.0.0.1 的端口，Apache 完全不知道它的存在。
	//
	// 抓法：
	//   go tool pprof -http=:9999 http://127.0.0.1:8091/debug/pprof/profile?seconds=30
	//   curl "http://127.0.0.1:8091/debug/pprof/heap?debug=1"
	startPprof(*pprofAddr)

	// ---- 4.5 自动交易引擎（业务层）----
	//
	// 两条心跳，全自动，不需要人盯：
	//   · 止盈巡检（-exit-sec，默认 3 秒）—— 实时算持仓浮盈，够线立刻市价平掉
	//   · 信号扫描（-entry-sec，默认 60 秒）—— 全市场扫买入信号，命中就下单
	//
	// 是否真下单由 configs/okx_strategy.json 决定：
	//   enabled=false → 整个引擎停摆；dry_run=true → 只算信号不下单。
	if *noTrade {
		fmt.Println("[TRADE] -no-trade：自动交易已关闭（只跑数据 + 网页）")
		liveBarVal := *liveBar
		if liveBarVal == "" {
			liveBarVal = strategyStore.Get().Bar
		}
		service.StartSignalBackfillLoop(ctx, db, func(format string, args ...any) {
			logx.Logf("INFO", "[SIG-BF] "+format, args...)
		})
	} else {
		liveBarVal := *liveBar
		if liveBarVal == "" {
			liveBarVal = strategyStore.Get().Bar
		}
		startCfg := strategyStore.Get()
		// ---- 4.6 历史信号回算 ----
		// 回补只入库 K 线，历史 K 线图上没有 🚀。这里后台逐根跑 8 因子，
		// 与实时扫描同一套 ComputeSignal（口径一致），写进 signals 表；
		// 图上的历史买入信号、信号 tab 的历史记录就都有了。
		service.StartSignalBackfillLoop(ctx, db, func(format string, args ...any) {
			// 走 logx 写文件：作为 Windows 服务运行时 stdout 被 SCM 收走，
			// 用 fmt.Printf 的话回算日志在 logs/ 里一条都看不到，没法排查。
			logx.Logf("INFO", "[SIG-BF] "+format, args...)
		})
		service.StartLive(ctx, service.LiveOptions{
			ExitEvery:  time.Duration(*exitSec) * time.Second,
			EntryEvery: time.Duration(*entrySec) * time.Second,
			Bar:        liveBarVal,
			Log: func(format string, args ...any) {
				fmt.Printf("%s [TRADE] %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
			},
		})
		fmt.Printf("[TRADE] 自动交易已挂载：dry_run=%v 周期=%s 出场=%s\n",
			startCfg.DryRun, liveBarVal,
			service.ExitText(startCfg.Exit.TakeProfitPct, startCfg.Exit.BollUpperExit, startCfg.Exit.MaxHoldMinutes))
	}

	// ---- 4.7 OKX 成交明细同步 ----
	//
	// 本地 trade 表只记「程序自己下的单」，用户手工在 OKX 上做的交易本地一无所知。
	// 这里每 5 分钟拉一次 /api/v5/trade/fills-history（OKX 保留最近 3 天），
	// 合成进 trade_event 流水 —— 历史面板/交易明细就能看到最近 3 天的全部成交。
	// 无条件启动：跟自动交易开不开没关系，网页展示需要它。
	service.StartOKXFillsSync(ctx)

	// ---- 4.75 OKX 已平仓仓位历史同步（历史仓位面板的数据源）----
	//
	// 上面那个 fills 同步只写 trade_event（逐笔流水），而「历史仓位」面板读的是
	// trade 表（一个仓位一行）。本地 trade 原来只记程序自己下的单 —— 实测
	// OKX 账户上有 100+ 个已平仓仓位、8000+ 笔成交，本地却只有 8 行。
	// 这里每 10 分钟把 /api/v5/account/positions-history（最近 3 个月）拉回来
	// upsert 进 trade 表，历史面板才真的「有东西看」。
	service.StartOKXPositionsSync(ctx)

	// ---- 4.76 NQ（纳斯达克100）只读行情同步 —— ★ 二十一期已下线 ★
	//
	// 2026-10-03 用户口径：「取消NQ所有东西 包括并且删除NQ按钮 数据等页面还有信号」。
	// StartNQSync（Dukascopy 历史）与 StartNQIntraday（Yahoo ^NDX 当天）两个启动项
	// 都已摘除 —— 不停同步的话，删掉的 NQ-INDEX 数据会被下一轮重新写回来。
	// 存量数据（kline / signals / inst / signal_scan_state 里的 NQ-INDEX）已在
	// 本次换壳停机窗口内手工清库。代码本体（dukascopy.go / nq_intraday.go）暂留
	// 不删，方便日后想恢复时把这两个启动项加回来即可。

	// ---- 4.8 自动维护程序（月度任务 + 年度任务）----
	//
	// 两条独立红线：
	//   记录表（trade/trade_event/signals/equity/runlog）30 天 → 月度任务里清
	//   K 线 15m 保留 365 天                                  → 年度任务里清
	//
	// 月度任务包含：归档上月 K 线 → 记录表清理 → 日志清理（30 天，含
	// apache/logs 与 mysql 的 error/slow log）→ 磁盘守卫（C 盘 < 10GB 就把
	// K 线收缩到当月）→ 清空回收站。
	//
	// 启动后 90 秒起，每 30 分钟巡检一次，用 meta 表记的「上次跑的月份/年份」
	// 决定要不要补跑 —— 停机期间错过的任务重启后会补上，跑过的不会重复。
	// 手工核对：`bin\okxweb.exe -maint-dry`（预演）/ `-maint`（真跑）/
	//           `-maint-yearly`（年度清理）/ `-cleanup-dry` / `-cleanup`。
	service.StartMaintenance(ctx)

	// ---- 4.8 性能自检 ----
	//
	// 每秒采一次本进程 CPU，每 30 秒往日志打一行汇总（CPU / goroutine /
	// 各热点调用次数与耗时）。「CPU 占用太高」这种问题必须先能量化 ——
	// 这台机器上 wmic / PowerShell 全被黑名单拦了，只能进程自测。
	// 同时暴露 /api/perf，网页和 curl 都能随时取。
	perf.Start(ctx, 30*time.Second, func(format string, args ...any) {
		logx.Logf("INFO", format, args...)
	})

	// ---- 5. 常驻：等退出（服务被 Stop / 控制台 Ctrl-C / 网页端口起不来）----
	var runErr error
	select {
	case <-ctx.Done():
		fmt.Println("\n收到退出信号，正在停止…")
	case e := <-webErr:
		runErr = fmt.Errorf("网页服务异常退出：%w", e)
		fmt.Printf("\n%v\n", runErr)
	}

	service.StopLive()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	_ = httpSrv.Shutdown(shutCtx)
	bf.Stop()
	fmt.Println("已退出")
	return runErr
}

// applyUniverseFilter 按「哪些合约能买」的策略过滤，并把结果写回 inst 表
//
// 过滤规则（详细口径见 internal/service/universe.go）：
//
//	规则 1  不买美股 / ETF / 商品（★ 三期默认已关闭）
//	规则 2  不买刚上线的（listTime 距今不足 N 天）
//	规则 3  不买要下线的（OKX 公告中心 announcements-delistings 解析出来的名单）
//	规则 4  单笔保证金必须 ≤ max_order_margin_usdt 才买得起
//
// ★ 所有阈值都从配置现取（热读 configs/okx_strategy.json），
//   所以改完 JSON 不用重启就能改掉 symbolList。
func applyUniverseFilter(db *repo.DB, bf *service.BackfillManager, store *service.StrategyStore, days int) service.FilterStats {
	// 热读一次只做「配置是否可用」的守卫
	if store.Get() == nil {
		return service.FilterStats{}
	}

	// ★★ 准入策略必须走唯一构造入口，不要在这里手写字面量 ★★
	//
	// 这里原来手写了一份 service.UniversePolicy{...} —— 与二期 Scan() 漏填
	// MarginPolicy 是**同一个坑的复现**（当时准入上限静默从 1U 掉回 0.01U，
	// 480 个候选只剩 33 个）。三期复查时发现它同样漏填了 ExtraExclude：
	// 也就是说 exclude_inst 这条手动黑名单**只在下单扫描路径生效**，
	// 而「写回 inst 表」这条路径完全不看 —— 手动拉黑的合约照样会显示成
	// 「可交易」，两条路的结果互相打架。
	//
	// 另外：策略在这个进程里有两套解析器 —— conf（下单与准入计算用）与
	// service.StrategyConfig（前端展示用）。准入是会决定「买不买」的判定，
	// 必须与下单同源，所以取 conf.LoadConfig()。
	policy := service.UniversePolicyFromConfig(conf.LoadConfig())

	insts, err := db.ListInstruments()
	if err != nil {
		fmt.Printf("[准入] ⚠ 读合约列表失败：%v\n", err)
		return service.FilterStats{}
	}
	tks, err := db.ListTickers()
	if err != nil {
		fmt.Printf("[准入] ⚠ 读行情失败：%v\n", err)
		return service.FilterStats{}
	}
	tkMap := make(map[string]model.Ticker, len(tks))
	for _, t := range tks {
		tkMap[t.InstID] = t
	}

	// 下线名单：抓 OKX 公告（有 24 小时缓存）
	var delist map[string]service.DelistEntry
	delistLine := ""
	if policy.ExcludeDelisting {
		uni := make(map[string]bool, len(insts))
		for _, it := range insts {
			sym := it.BaseCcy
			if sym == "" {
				sym = service.SymbolOf(it.InstID)
			}
			uni[sym] = true
		}
		entries := service.LoadDelistList(bf.Feed().AnnouncementFetcher(), db, uni, policy.ExcludeNewListingDays+30)
		delist = service.DelistSymbolSet(entries)
		delistLine = fmt.Sprintf("%d 个币种 %s", len(entries), previewSymbols(entries, 12))
	}

	kept, stats := service.FilterUniverse(insts, tkMap, delist, policy)

	// 落库：哪些能买 / 为什么不能买
	keepSet := make(map[string]bool, len(kept))
	for _, k := range kept {
		keepSet[k.InstID] = true
	}
	upd := make([]model.Instrument, 0, len(insts))
	for _, it := range insts {
		row := model.Instrument{InstID: it.InstID}
		switch {
		case keepSet[it.InstID]:
			row.Tradeable, row.ExcludeReason = 1, ""
		case service.IsReadonlyInst(it.InstID):
			// ★ 只读板块（NQ）：有行情、有信号，但**永不交易**。
			//
			// 这一支必须写在 reasonOf 之前，否则它会被归到
			// 「成交额不足」「最小一手超上限」之类的技术原因上，
			// 界面上看起来像「只是暂时不合格」，哪天参数一放宽就可能被放回可交易。
			// 只读是**设计约束**，不是准入结论。
			row.Tradeable, row.ExcludeReason = 0, "readonly"
		default:
			row.Tradeable, row.ExcludeReason = 0, reasonOf(it, tkMap, policy, delist)
		}
		upd = append(upd, row)
	}
	if err := db.UpdateTradeable(upd); err != nil {
		fmt.Printf("[准入] ⚠ 写回可交易标记失败：%v\n", err)
	}

	fmt.Println("[准入] ── 合约准入过滤 ─────────────────────────────────")
	fmt.Printf("[准入] 抓到合约      : %d 个 USDT 永续\n", stats.Total)
	fmt.Printf("[准入] 分类分布      : %v（1=加密 3=美股ETF 4=商品）\n", stats.ByCategory)
	// 规则 1 三期已默认关闭：这里必须把开关状态打出来，
	// 否则「排除 0 个」既可能是「已关闭」也可能是「本来就没有」。
	catState := "已关闭（三期：只看最小一手 ≤ 上限）"
	if policy.ExcludeStockETF {
		catState = "启用中"
	}
	fmt.Printf("[准入] 排除 美股/ETF/商品 : %d（规则1·%s）\n", stats.DroppedCategory, catState)
	fmt.Printf("[准入] 排除 非 live       : %d\n", stats.DroppedState)
	fmt.Printf("[准入] 排除 刚上线(%d天)  : %d（规则2）\n", policy.ExcludeNewListingDays, stats.DroppedNew)
	if policy.ExcludeDelisting {
		fmt.Printf("[准入] 排除 即将下线      : %d（规则3·公告黑名单 %s）\n", stats.DroppedDelist, delistLine)
	}
	fmt.Printf("[准入] 排除 成交额<%.0f万 : %d\n", policy.MinQuoteVolume24h/10000, stats.DroppedVolume)
	fmt.Printf("[准入] 排除 单张>%.2fU     : %d（规则5·最小一手保证金超上限）\n", policy.OrderMarginCap(), stats.DroppedNotional)
	fmt.Printf("[准入] ✔ 可交易合约   : %d 个\n", stats.Kept)
	if stats.ScaledUp > 0 {
		fmt.Printf("[准入]   · 其中 %d 个 %.2fU 买不起 1 张，下单会放大到刚好 1 张（≤%.2fU）\n",
			stats.ScaledUp, policy.MarginUSDT, policy.OrderMarginCap())
	}
	if len(stats.DelistSymbols) > 0 {
		fmt.Printf("[准入] 命中下线公告币种 : %v\n", stats.DelistSymbols)
	}
	if len(stats.NewListingSymbol) > 0 {
		n := len(stats.NewListingSymbol)
		if n > 6 {
			n = 6
		}
		fmt.Printf("[准入] 新上线币种(%d个)  : %v…\n", len(stats.NewListingSymbol), stats.NewListingSymbol[:n])
	}
	fmt.Printf("[准入] 单笔口径      : 目标 %.2f U/笔 · %dx杠杆 · 准入上限 %.2f U/笔\n",
		policy.MarginUSDT, policy.Leverage, policy.OrderMarginCap())
	fmt.Println("[准入] ─────────────────────────────────────────────────")
	return stats
}

// reasonOf 给出被排除的原因（写进 inst.exclude_reason，前端可直接显示）
func reasonOf(it model.Instrument, tks map[string]model.Ticker,
	p service.UniversePolicy, delist map[string]service.DelistEntry) string {
	if p.ExcludeStockETF && it.InstCategory != "" && it.InstCategory != "1" {
		switch it.InstCategory {
		case "3":
			return service.ReasonStockETF
		case "4":
			return service.ReasonCommodity
		default:
			return service.ReasonCategory
		}
	}
	if it.State != "" && it.State != "live" {
		return service.ReasonState
	}
	if p.ExcludeNewListingDays > 0 && it.ListTime > 0 {
		cut := time.Now().UnixMilli() - int64(p.ExcludeNewListingDays)*86400000
		if it.ListTime > cut {
			return service.ReasonNewListing
		}
	}
	if p.ExcludeDelisting && len(delist) > 0 {
		sym := it.BaseCcy
		if sym == "" {
			sym = service.SymbolOf(it.InstID)
		}
		if _, ok := delist[sym]; ok {
			return service.ReasonDelisting
		}
	}
	tk, hasTk := tks[it.InstID]
	if !hasTk || tk.Last <= 0 {
		return service.ReasonNoTicker
	}
	if p.MinQuoteVolume24h > 0 && tk.QuoteVol24h < p.MinQuoteVolume24h {
		return service.ReasonLowVolume
	}
	cap := p.OrderMarginCap()
	if p.MarginUSDT > 0 && p.Leverage > 0 && cap > 0 {
		need := service.MinOrderMargin(it, tk.Last, service.LeveragePolicy{Leverage: p.Leverage})
		if need > cap+1e-9 {
			return service.ReasonNotional
		}
	}
	return service.ReasonOther
}

// previewSymbols 把下线名单压成一行短摘要（最多 n 个）
func previewSymbols(items []service.DelistEntry, n int) string {
	if len(items) == 0 {
		return "（空）"
	}
	syms := make([]string, 0, len(items))
	for _, it := range items {
		syms = append(syms, it.Symbol)
	}
	sort.Strings(syms)
	if len(syms) > n {
		return "[" + strings.Join(syms[:n], " ") + " …]"
	}
	return "[" + strings.Join(syms, " ") + "]"
}

// projectRoot 从当前目录往上找 go.mod；找不到就照 exe 的位置再找一遍。
//
// 为什么要有第二条：把 okxweb 注册成 Windows 服务后，进程的工作目录是
// C:\Windows\System32，靠 cwd 永远找不到项目根，日志/配置/网页资源全会跑偏。
// 服务方式下只能靠 exe 自己（bin\okxweb.exe）往上退一级反推。
// applyMySQLFlags 把命令行**显式传入**的 MySQL 参数盖到 c 上。
//
// ★ 为什么是「显式传入才盖」而不是像原来那样无条件赋值 ★
//
//	原来写的是 `c.User, c.Password, c.Database = *mUser, *mPass, *mDB`，
//	而 mPass 的 flag 默认值就是明文口令 —— 等于 flag 默认值**永远压过**
//	配置文件和环境变量。想让口令来自环境变量，这一行必须先变成条件赋值，
//	否则在 secret.go 里做得再干净也没用（这就是「改了源码但没生效」的经典坑）。
//
//	空串 / 0 一律解释成「没传」，保留 DefaultMySQLConfig() 已经从
//	环境变量 / 密钥文件解析出来的值。
func applyMySQLFlags(c *repo.MySQLConfig) {
	if *mHost != "" {
		c.Host = *mHost
	}
	if *mPort != 0 {
		c.Port = *mPort
	}
	if *mUser != "" {
		c.User = *mUser
	}
	if *mPass != "" {
		c.Password = *mPass
	}
	if *mDB != "" {
		c.Database = *mDB
	}
}

// startPprof 起一个**只绑回环地址**的 pprof 服务。
//
// 存在的理由：优化之前必须先知道 CPU 花在哪。没有火焰图就只能靠推断，
// 而本项目的推断已经错过一次（以为瓶颈在指标计算，实测计算只占墙钟 0.21%）。
//
// 两条安全约束（都不是可选的）：
//
//  1. 独立端口 —— 绝不能挂在 8090 上。8090 被 Apache 反代到公网 80，
//     挂上去等于把 goroutine 栈 / 命令行 / 堆快照暴露给全世界。
//  2. 强制回环 —— 哪怕有人手滑传 `-pprof-addr 0.0.0.0:8091`，这里直接拒绝启动。
//     端口配错不该变成一次安全事件。
//
// 抓法：
//
//	go tool pprof -http=:9999 "http://127.0.0.1:8091/debug/pprof/profile?seconds=30"
//	go tool pprof -http=:9999 http://127.0.0.1:8091/debug/pprof/heap
//
// 抓完记得关：`-pprof-addr ""` 或改回默认。
func startPprof(addr string) {
	if strings.TrimSpace(addr) == "" {
		fmt.Println("[PPROF] 已关闭（-pprof-addr 留空）")
		return
	}
	if _, ok := pprofAddrIsLoopback(addr); !ok {
		fmt.Printf("[PPROF] ✘ 拒绝启动：%q 不是回环地址。\n"+
			"        pprof 能读到 goroutine 栈与堆快照，只允许 127.0.0.1（或 ::1 / localhost）。\n", addr)
		return
	}

	// 显式建 mux，不用 http.DefaultServeMux —— 避免和别处的全局注册互相污染。
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	ps := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		fmt.Printf("[PPROF] 火焰图已开：http://%s/debug/pprof/\n", addr)
		if err := ps.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("[PPROF] ⚠ 监听失败（不影响主服务）：%v\n", err)
		}
	}()
}

// pprofAddrIsLoopback 校验 pprof 监听地址是否安全。
//
// 抽成纯函数是为了能被穷举单测（见 main_pprof_test.go）：
// 「端口配错」不该变成一次把 goroutine 栈和堆快照送出门的安全事件，
// 所以这条判据必须被测试钉死，而不是靠 code review 的注意力。
//
// 只接受：127.0.0.0/8、::1、以及字面量 localhost。
// 明确拒绝：0.0.0.0、::、空主机（= 全接口）、任何公网/内网具体 IP、以及解析不了的字符串。
func pprofAddrIsLoopback(addr string) (string, bool) {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return "", false
	}
	// "localhost:8091" 这种写法要放行：它解析到的就是回环
	if strings.EqualFold(host, "localhost") {
		return host, true
	}
	// 空主机（如 ":8091"）等于绑全部接口，必须拒绝
	if host == "" {
		return "", false
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", false
	}
	return host, true
}

func projectRoot() string {
	if dir, err := os.Getwd(); err == nil {
		if r := walkUpToRoot(dir, 6); r != "" {
			return r
		}
	}
	if exe, err := os.Executable(); err == nil {
		if r := walkUpToRoot(filepath.Dir(exe), 6); r != "" {
			return r
		}
	}
	if dir, err := os.Getwd(); err == nil {
		return dir
	}
	return "."
}

// cwdOf 取当前工作目录，仅用于日志定位。
//
// ★ 为什么要专门打这个：Windows 服务由 SCM 拉起时工作目录是
//   C:\Windows\System32，而相对路径的 .smtp-pass 就是相对它的。
//   「配置明明在，服务就是读不到」这类问题的第一个分叉点就在这一行。
func cwdOf() string {
	if d, err := os.Getwd(); err == nil {
		return d
	}
	return "?"
}

// walkUpToRoot 从 dir 开始逐级向上找含 go.mod 的目录，找不到返回空串。
func walkUpToRoot(dir string, max int) string {
	for i := 0; i < max; i++ {
		if _, e := os.Stat(filepath.Join(dir, "go.mod")); e == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// ---------------------------------------------------------------------------
// 回补参数解析（2026-10-01 二期）
// ---------------------------------------------------------------------------

// resolveBackfillDays 把 -days 解析成真正要回补的天数。
//
//	-days > 0  → 用它（运维想临时多拉几天历史时用）
//	-days <= 0 → 用配置里的 kline_retain_days（当前 30 天）
//
// 为什么要这个函数而不是把 flag 默认值写成 10：
// 保留窗口的唯一真源是 configs/okx_strategy.json，写死两份迟早走岔
// ——「回补 365 天、只留 10 天」那种自相矛盾的组合就是这么来的。
func resolveBackfillDays(flagVal int) int {
	if flagVal > 0 {
		return flagVal
	}
	if d := repo.KlineRetainDays(); d > 0 {
		return d
	}
	return 10
}

// daysNote 打印时标注这个天数是从哪来的，方便排查「为什么只补了 10 天」
func daysNote(flagVal int) string {
	if flagVal > 0 {
		return "（-days 指定）"
	}
	return "（跟随 kline_retain_days）"
}

// backfillBarsText 打印实际要回补的周期（-bars 留空 = model.EnabledBars）
func backfillBarsText(flagVal string) string {
	if strings.TrimSpace(flagVal) == "" {
		return strings.Join(model.EnabledBars, ",") + "（全部，来自 model.EnabledBars）"
	}
	return flagVal
}
