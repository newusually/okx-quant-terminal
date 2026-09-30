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
//   2. 从 OKX 回补至少一个月的 K 线（1m/3m/5m/15m/1H/4H）到 MySQL，并持续增量更新
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

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"finally-main/internal/handler"
	"finally-main/internal/model"
	"finally-main/internal/repo"
	"finally-main/internal/service"
)

// ---------------------------------------------------------------------------
// 命令行参数（提到包级：服务模式和控制台模式共用同一套）
// ---------------------------------------------------------------------------

var (
	addr    = flag.String("addr", "127.0.0.1:8090", "网页监听地址（Apache 反代到这里）")
	proxy   = flag.String("proxy", "", "HTTP/SOCKS5 代理，如 http://127.0.0.1:7890；留空直连")
	days    = flag.Int("days", 30, "K 线回补天数（至少一个月）")
	focus   = flag.String("focus", "", "启动即回补的合约，逗号分隔；留空 = 按成交额取 TopN")
	focusN  = flag.Int("focusn", 8, "focus 留空时取成交额前 N 名")
	workers = flag.Int("workers", 10, "回补并发数（受 OKX 限频约束，10 已接近上限）")
	bars    = flag.String("bars", "1m,3m,5m,15m,1H,4H", "要回补的周期，逗号分隔")
	rtSec   = flag.Int("rt", 5, "实时行情落库间隔（秒）")
	scope   = flag.String("backfill-scope", "plan",
		"回补范围：plan（默认，全部live×15m/1H/4H + 可交易×5m/3m/1m）| tradeable | live（约3.6GB，看磁盘）| focus | none")
	minFree = flag.Int("min-free-mb", 800, "剩余磁盘低于此值就暂停回补（0=不检查）")

	mHost  = flag.String("mysql-host", "127.0.0.1", "MySQL 主机")
	mPort  = flag.Int("mysql-port", 3306, "MySQL 端口")
	mUser  = flag.String("mysql-user", "okx", "MySQL 用户")
	mPass  = flag.String("mysql-pass", "OkxQuant2026", "MySQL 密码")
	mDB    = flag.String("mysql-db", "okx", "MySQL 库名")
	mOpen  = flag.Int("mysql-maxopen", 64, "MySQL 连接池上限")
	mBatch = flag.Int("mysql-batch", 500, "批量写入分片大小")

	initOnly = flag.Bool("init-only", false, "只建库建表 + 同步一次合约与行情，然后退出")
	noBf     = flag.Bool("no-backfill", false, "不做历史回补（只起网页 + 实时行情）")

	// ---- 自动交易（纯 Go，不需要人盯着）----
	noTrade  = flag.Bool("no-trade", false, "关掉自动交易：只跑数据 + 网页，不下任何单")
	exitSec  = flag.Int("exit-sec", 3, "止盈巡检间隔（秒）—— 实时盯浮盈，够线就平")
	entrySec = flag.Int("entry-sec", 60, "买入信号扫描间隔（秒）—— 全市场扫买入信号")
	liveBar  = flag.String("live-bar", "", "自动交易用哪个周期判买卖（留空 = 读配置里的 bar）")

	// ---- Windows 服务 ----
	installSvc   = flag.Bool("install", false, "注册为 Windows 服务 OKXWeb（需管理员，幂等）")
	uninstallSvc = flag.Bool("uninstall", false, "停止并卸载 Windows 服务 OKXWeb（需管理员）")

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
	mcfg.Host, mcfg.Port = *mHost, *mPort
	mcfg.User, mcfg.Password, mcfg.Database = *mUser, *mPass, *mDB
	mcfg.MaxOpenConns, mcfg.BatchSize = *mOpen, *mBatch

	fmt.Println("==============================================================")
	fmt.Println(" OKX 全合约量化终端 · 纯 Go 后端 + MySQL（无 Python / 无 CGO）")
	fmt.Println("==============================================================")
	fmt.Printf(" 项目根目录 : %s\n", root)
	fmt.Printf(" 数据库     : mysql://%s@%s:%d/%s（连接池 %d）\n",
		mcfg.User, mcfg.Host, mcfg.Port, mcfg.Database, mcfg.MaxOpenConns)
	fmt.Printf(" 回补天数   : %d\n", *days)
	fmt.Printf(" 回补周期   : %s\n", *bars)
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

	// ---- 1.5 自愈：清掉不符合不变量的脏 K 线 ----
	// 时间戳非整秒 / 价格非正的，一定是外部工具或异常写入塞进来的。
	// 教训：压测工具曾把 28.68 万行合成数据写进生产表，把每个合约的
	// MA25/MA99/布林带全算歪 —— 这里每次都兜一道底。
	if n, err := db.PurgeBadKlines(); err != nil {
		fmt.Printf("[DB] ⚠ 脏 K 线自检失败：%v\n", err)
	} else if n > 0 {
		fmt.Printf("[DB] ✔ 自愈：清掉 %d 行非法 K 线（时间戳非整秒或价格非正）\n", n)
	}

	tabs, err := db.Tables()
	if err != nil {
		log.Fatalf("读表结构失败：%v", err)
	}
	fmt.Printf("[DB] 表已就绪（%d 张）：%s\n", len(tabs), strings.Join(tabs, ", "))
	counts, _ := db.TableCounts()
	for _, t := range tabs {
		fmt.Printf("     · %-14s %8d 行\n", t, counts[t])
	}

	// ---- 2. 策略配置（给前端显示 0.1U/笔 这些参数）----
	cfgPath := service.StrategyConfigPath(root)
	strategy, serr := service.LoadStrategy(cfgPath)
	if serr != nil {
		fmt.Printf("[CFG] 读取 %s 失败（用默认值）：%v\n", cfgPath, serr)
	}
	fmt.Printf("[CFG] 策略参数：%s，止盈 %.2f%%，共振阈值 %d/8，dry_run=%v\n",
		strategy.MarginText(), strategy.Exit.TakeProfitPct, strategy.ScoreThreshold, strategy.DryRun)

	// ---- 3. 数据服务（业务层）----
	feed := service.NewDataFeed(*proxy)

	cfg := service.DefaultBackfillConfig()
	cfg.Days = *days
	cfg.Workers = *workers
	cfg.RealtimeSec = *rtSec
	cfg.FocusN = *focusN
	cfg.Scope = *scope
	cfg.MinFreeMB = *minFree
	cfg.RootDir = root
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
	//   不买美股/ETF/商品 + 不买刚上线 + 不买要下线 + 0.1U 必须买得起
	service.SetAnnounceCacheDB(db)
	applyUniverseFilter(db, bf, strategy, *days)

	if *initOnly {
		fmt.Println("[DB] -init-only：建库建表 + 准入过滤完成，退出")
		return nil
	}

	if !*noBf && *scope != "none" {
		if err := bf.Start(ctx); err != nil {
			fmt.Printf("[DATA] ⚠ 数据服务启动异常：%v\n", err)
		}

		// ---- 3.5 准入过滤不是一次性的 ----
		//   新币上线会老过 30 天、成交额会塌、公告会新出下线通知，
		//   所以每 30 分钟重跑一次，把结论刷新回 inst.tradeable。
		//   （公告黑名单自身有 24h 缓存，重跑不会打爆 OKX 接口。）
		go func() {
			t := time.NewTicker(30 * time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					applyUniverseFilter(db, bf, strategy, *days)
				}
			}
		}()
	}

	// ---- 4. 网页服务（接口层）----
	srv := handler.NewServer(db, feed, bf, strategy, handler.AssetsDir(root), root,
		func(format string, args ...any) {
			fmt.Printf("%s [WEB]  %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
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
			liveBarVal = strategy.Bar
		}
		service.StartSignalBackfillLoop(ctx, db, liveBarVal, func(format string, args ...any) {
			fmt.Printf("%s [SIG-BF] %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
		})
	} else {
		liveBarVal := *liveBar
		if liveBarVal == "" {
			liveBarVal = strategy.Bar
		}
		// ---- 4.6 历史信号回算 ----
		// 回补只入库 K 线，历史 K 线图上没有 🚀。这里后台逐根跑 8 因子，
		// 与实时扫描同一套 ComputeSignal（口径一致），写进 signals 表；
		// 图上的历史买入信号、信号 tab 的历史记录就都有了。
		service.StartSignalBackfillLoop(ctx, db, liveBarVal, func(format string, args ...any) {
			fmt.Printf("%s [SIG-BF] %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
		})
		service.StartLive(ctx, service.LiveOptions{
			ExitEvery:  time.Duration(*exitSec) * time.Second,
			EntryEvery: time.Duration(*entrySec) * time.Second,
			Bar:        liveBarVal,
			Log: func(format string, args ...any) {
				fmt.Printf("%s [TRADE] %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
			},
		})
		fmt.Printf("[TRADE] 自动交易已挂载：dry_run=%v 周期=%s 止盈 %.2f%% 布林上轨出场=%v\n",
			strategy.DryRun, liveBarVal, strategy.Exit.TakeProfitPct, strategy.Exit.BollUpperExit)
	}

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
//	规则 1  不买美股 / ETF / 商品（OKX instCategory != 1 的全砍掉）
//	规则 2  不买刚上线的（listTime 距今不足 N 天）
//	规则 3  不买要下线的（OKX 公告中心 announcements-delistings 解析出来的名单）
//	规则 4  单笔 0.1 USDT 必须买得起（minSz × ctVal × ctMult × 价格 ÷ 杠杆 ≤ 0.1）
func applyUniverseFilter(db *repo.DB, bf *service.BackfillManager, st *service.StrategyConfig, days int) service.FilterStats {
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

	policy := service.UniversePolicy{
		ExcludeStockETF:       st.ExcludeStockETF,
		ExcludeNewListingDays: st.ExcludeNewListingDays,
		ExcludeDelisting:      st.ExcludeDelisting,
		MarginUSDT:            st.Entry.MarginUSDT,
		Leverage:              st.Entry.Leverage,
		MarginPolicy:          st.Entry.MarginPolicy,
		MaxMarginUSDT:         st.MaxOrderMarginUSDT,
		MinQuoteVolume24h:     st.MinQuoteVolume24h,
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
		if keepSet[it.InstID] {
			row.Tradeable, row.ExcludeReason = 1, ""
		} else {
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
	fmt.Printf("[准入] 排除 美股/ETF/商品 : %d（规则1）\n", stats.DroppedCategory)
	fmt.Printf("[准入] 排除 非 live       : %d\n", stats.DroppedState)
	fmt.Printf("[准入] 排除 刚上线(%d天)  : %d（规则2）\n", policy.ExcludeNewListingDays, stats.DroppedNew)
	if policy.ExcludeDelisting {
		fmt.Printf("[准入] 排除 即将下线      : %d（规则3·公告黑名单 %s）\n", stats.DroppedDelist, delistLine)
	}
	fmt.Printf("[准入] 排除 成交额<%.0f万 : %d\n", policy.MinQuoteVolume24h/10000, stats.DroppedVolume)
	fmt.Printf("[准入] 排除 单张>%.2fU     : %d（规则5·最小一手保证金超上限）\n", policy.OrderMarginCap(), stats.DroppedNotional)
	fmt.Printf("[准入] ✔ 可交易合约   : %d 个\n", stats.Kept)
	if stats.ScaledUp > 0 {
		fmt.Printf("[准入]   · 其中 %d 个 0.1U 买不起 1 张，下单会放大到刚好 1 张（≤%.2fU）\n",
			stats.ScaledUp, policy.OrderMarginCap())
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
