package service

// cleanup.go —— 自动维护程序（月度任务 + 年度任务）
//
// ---------------------------------------------------------------------------
// 用户口径（2026-10-01）
// ---------------------------------------------------------------------------
//
//	「15 分钟一年数据保留，其余的分文件上传到 github 中，
//	  并且同步每个月月底按月份上传到 github 中，自动上传；
//	  每年清除多余超过一年的数据，一年才运行一次清除任务就行。
//	  如果每个月月底 C 盘剩余总量小于 10G 余额就删除掉多余的之前几个月的数据，
//	  只保留当月数据就行；另外每个月要清除所有超过一个月的日志记录，
//	  包括数据库、客户端、网页等日志，自动运行；并且清理回收站的垃圾文件。」
//
// 拆成两个任务：
//
//	【月度】每月第一次巡检时跑（回补 + 归档 + 收缩 + 清日志 + 清回收站）
//	  1. 归档上月 K 线 → archive/ 分片 gzip（供推 GitHub）
//	  2. 记录表清理（trade/trade_event/signals/equity/runlog…，红线 30 天）
//	  3. 日志清理（logs/ + apache/logs/，红线 30 天）
//	  4. 磁盘守卫：C 盘可用 < 10GB → K 线只留当月（DROP PARTITION 秒删）
//	  5. 清空回收站
//
//	【年度】每年第一次巡检时跑
//	  1. K 线清理：DROP PARTITION 扔掉早于 365 天的整段分区（秒级）
//	  2. 残余行分批 DELETE 兜底
//
// ---------------------------------------------------------------------------
// 为什么用「meta 表记上次跑的月份/年份」而不是定时器
// ---------------------------------------------------------------------------
// 机器会重启、服务会被拉起来又停掉。如果按「上次运行时间 + 30 天」算，
// 一次长时间停机就会把任务永久推迟；如果按 cron 定点算，
// 正好关机的那一分钟就永远错过了。
//
// 所以这里记的是**「上次跑的是哪个月/哪一年」**：
// 每个 tick（30 分钟）比一次，只要当前月份 ≠ 上次跑的月份就补跑。
// 停机三天、重启二十次都只会老老实实跑一次，不重不漏。

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"finally-main/internal/conf"
	"finally-main/internal/logx"
	"finally-main/internal/repo"
)

const (
	// maintTickInterval 巡检间隔。
	//
	// ★ 2026-10-01 二期：30 分钟 → **10 分钟** ★
	// 因为多了一条「每天凌晨清 K 线」的任务，30 分钟 tick 意味着实际执行时间
	// 落在 00:00~00:30 之间，运气不好就是 00:29 才删 —— 用户口径是「凌晨」。
	// 10 分钟 tick 把它收进 00:00~00:10。每次 tick 只是读三行 meta 做字符串比较
	// （走的是常驻连接池，不开新连接），代价可以忽略。
	maintTickInterval = 10 * time.Minute

	// maintFirstDelay 启动后延迟。回补 / 信号回算 / 成交同步都在前 60 秒内起跑，
	// 维护任务会跑批量 DELETE 和 ALTER TABLE，错开一下避免抢 IO。
	maintFirstDelay = 90 * time.Second

	// metaLastMonthly / metaLastYearly / metaLastDaily
	// meta 表里的「上次跑过哪个月 / 哪一年 / 哪一天」
	metaLastMonthly = "maint_last_monthly"
	metaLastYearly  = "maint_last_yearly"
	metaLastDaily   = "maint_last_daily"
)

// ---------------------------------------------------------------------------
// 调度器
// ---------------------------------------------------------------------------

// StartMaintenance 常驻维护循环（服务生命周期内一直跑）。
//
// 每个 tick 判断「这个月/这一年跑过了吗」，没跑过就补跑。
// 每次跑完都会往 meta 写回标记，所以重启不会重复执行。
func StartMaintenance(ctx context.Context) {
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(maintFirstDelay):
		}
		for {
			if err := RunDueMaintenance(); err != nil {
				logx.Logf("WARN", "[MAINT] 维护任务失败：%v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(maintTickInterval):
			}
		}
	}()
}

// RunDueMaintenance 检查并按需执行到期的月度 / 年度任务。
func RunDueMaintenance() error {
	cfg := conf.LoadConfig()
	store := repo.NewStore(cfg)
	db, err := store.DB()
	if err != nil {
		return fmt.Errorf("连接数据库失败：%w", err)
	}

	now := time.Now()
	monthTag := now.Format("2006-01")
	yearTag := now.Format("2006")
	dayTag := now.Format("2006-01-02")

	lastDay, _, _ := db.GetMeta(metaLastDaily)
	lastMonth, _, _ := db.GetMeta(metaLastMonthly)
	lastYear, _, _ := db.GetMeta(metaLastYearly)

	// ---- 每日：K 线超期删除（2026-10-01 二期新增）----
	//
	// 用户口径：「只能查询保存最近 10 天数据，不能多，多出来就删除」
	//          +「自动每天凌晨删除数据一次」。
	//
	// 调度口径与月度/年度完全一致：记「上次跑的是哪一天」，不同就补跑。
	// 所以凌晨关机、服务没起、重启二十次，都只会老老实实跑一次，不重不漏；
	// 而如果凌晨那会儿服务恰好不在，当天第一次巡检（比如下午三点）会补跑
	// —— 宁可晚几小时，也不要因为「错过的正是凌晨」而永远不删。
	if lastDay != dayTag {
		logx.Logf("INFO", "[MAINT] 今天（%s）还没清过 K 线，开始执行…", dayTag)
		rep, err := RunDailyMaintenance(false)
		if err != nil {
			// 每日任务失败不能拖累月度/年度 —— 那两条的窗口长得多，更该跑。
			logx.Logf("WARN", "[MAINT] 每日 K 线清理失败：%v（下个 tick 重试）", err)
		} else {
			if err := db.SetMeta(metaLastDaily, dayTag); err != nil {
				logx.Logf("WARN", "[MAINT] 写回 %s 失败：%v（下个 tick 会重跑一次）", metaLastDaily, err)
			}
			logx.Logf("INFO", "[MAINT] 每日 K 线清理完成：%s", rep.Summary)
		}
	}

	if lastMonth != monthTag {
		logx.Logf("INFO", "[MAINT] 本月（%s）还没跑过维护任务，开始执行…", monthTag)
		rep, err := RunMonthlyMaintenance(false)
		if err != nil {
			return err
		}
		if err := db.SetMeta(metaLastMonthly, monthTag); err != nil {
			logx.Logf("WARN", "[MAINT] 写回 %s 失败：%v（下个 tick 会重跑一次）", metaLastMonthly, err)
		}
		logx.Logf("INFO", "[MAINT] 月度任务完成：%s", rep.Summary)
	}

	if lastYear != yearTag {
		logx.Logf("INFO", "[MAINT] 本年度（%s）还没跑过年度清理，开始执行…", yearTag)
		rep, err := RunYearlyMaintenance(false)
		if err != nil {
			return err
		}
		if err := db.SetMeta(metaLastYearly, yearTag); err != nil {
			logx.Logf("WARN", "[MAINT] 写回 %s 失败：%v（下个 tick 会重跑一次）", metaLastYearly, err)
		}
		logx.Logf("INFO", "[MAINT] 年度清理完成：%s", rep.Summary)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 报告结构
// ---------------------------------------------------------------------------

// MaintenanceReport 一次月度/年度维护的完整结果
type MaintenanceReport struct {
	Kind      string `json:"kind"` // monthly | yearly
	DryRun    bool   `json:"dryRun"`
	StartedAt string `json:"startedAt"`
	Ms        int64  `json:"ms"`

	// 归档是「补齐式」的：一次可能导出好几个月（首次部署、停机几个月、
	// 上一次导出失败，都会留下缺口月份），所以这里是列表而不是单值。
	ArchivedMonths []string           `json:"archivedMonths,omitempty"`
	Archives       []*ArchiveManifest `json:"archives,omitempty"`
	ArchiveSkipped []string           `json:"archiveSkipped,omitempty"` // 早已归档、本轮跳过
	ArchiveDir     string             `json:"archiveDir,omitempty"`
	ArchiveErr     string             `json:"archiveErr,omitempty"`
	PushOut        string           `json:"pushOut,omitempty"`
	Pushed         bool             `json:"pushed"`
	PushErr        string           `json:"pushErr,omitempty"`

	Retention *RetentionReport `json:"retention,omitempty"`

	LogFiles []string `json:"logFiles,omitempty"`
	LogBytes int64    `json:"logBytes,omitempty"`

	FreeGBBefore float64 `json:"freeGbBefore"`
	FreeGBAfter  float64 `json:"freeGbAfter"`
	GuardFired   bool    `json:"guardFired"`
	GuardDropped []string `json:"guardDropped,omitempty"`
	GuardRows    int64   `json:"guardRows"`
	// 守卫的归档侧动作与它的「不许删」原因（见 RunMonthlyMaintenance 第 4 步）
	GuardArcFiles int    `json:"guardArcFiles"`
	GuardArcBytes int64  `json:"guardArcBytes"`
	GuardKeepYM   string `json:"guardKeepYm,omitempty"`
	GuardArcNote  string `json:"guardArcNote,omitempty"`

	KlineDropped []string `json:"klineDropped,omitempty"`
	KlineRows    int64    `json:"klineRows"`
	KlineMonths  int      `json:"klineMonths"`
	KlineMB      float64  `json:"klineMb,omitempty"`

	Recycle *RecycleResult `json:"recycle,omitempty"`

	Summary string `json:"summary"`
	Err     string `json:"err,omitempty"`
}

// ---------------------------------------------------------------------------
// 月度任务
// ---------------------------------------------------------------------------

// RunMonthlyMaintenance 跑一次月度维护。
//
// 顺序是刻意排的 —— **先归档、再收缩**：
// 万一磁盘守卫要删掉上个月的数据，必须保证它已经安全落在 archive/ 里。
func RunMonthlyMaintenance(dryRun bool) (*MaintenanceReport, error) {
	start := time.Now()
	rep := &MaintenanceReport{
		Kind:      "monthly",
		DryRun:    dryRun,
		StartedAt: start.Format("2006-01-02 15:04:05"),
	}
	cfg := conf.LoadConfig()
	rep.FreeGBBefore = freeGB()

	// ---- 1. 归档所有「还没归档过」的、早于本月的月份 ----
	//
	// ★ 为什么不是只导「上个月」★
	// 第 4 步的磁盘守卫会把 K 线收缩到「只留当月」。如果某个早于当月的月份
	// 从来没见过归档文件，那一刀下去就是永久损失 —— 库里删了、远端也没有。
	// 首次部署、停机几个月、上一次导出失败，都会留下这种「缺口月份」。
	// 所以这里按库里**实际存在的月份**逐个检查，缺哪个月补哪个月（幂等：
	// 已有 manifest 的月份直接跳过，不会被重复导出）。
	now := time.Now()
	thisYM := now.Format("2006-01")
	dir := ArchiveDir()
	rep.ArchiveDir = dir

	kmons, merr := klineMonthsOf()
	if merr != nil {
		rep.ArchiveErr = "读取 K 线月份清单失败：" + merr.Error()
	}
	for _, km := range kmons {
		ym := km.Month
		if ym >= thisYM {
			continue // 本月还没过完，不能归档
		}
		if km.Rows == 0 {
			continue
		}
		if ArchiveExists(dir, ym) {
			rep.ArchiveSkipped = append(rep.ArchiveSkipped, ym)
			continue
		}
		if dryRun {
			rep.ArchivedMonths = append(rep.ArchivedMonths, ym)
			continue
		}
		man, err := ExportMonth(ym, dir)
		if err != nil {
			// 一个月失败不影响其它月份：缺的月份下一轮还会被补上
			if rep.ArchiveErr == "" {
				rep.ArchiveErr = ym + " 导出失败：" + err.Error()
			}
			logArchive(nil, dir, err)
			continue
		}
		if _, werr := WriteManifest(dir, man); werr != nil {
			if rep.ArchiveErr == "" {
				rep.ArchiveErr = ym + " 清单写入失败：" + werr.Error()
			}
			continue
		}
		logArchive(man, dir, nil)
		rep.Archives = append(rep.Archives, man)
		rep.ArchivedMonths = append(rep.ArchivedMonths, ym)
	}

	// ---- 1.5 推送到 GitHub（必须在磁盘守卫之前！）----
	//
	// 顺序很关键：磁盘守卫会删掉本地 archive/ 里早于当月的归档，
	// 先删后推一旦推送失败就是「本地没了、远端也没有」。所以先把手里
	// 所有归档推到远端，确认成功了，第 4 步才允许删本地。
	// 推送失败不算任务失败（本地 archive/ 还在），但要记警告，
	// 并且第 4 步会因此**跳过归档删除**。
	if !dryRun {
		out, err := pushGitHub("data")
		rep.PushOut = out
		if err != nil {
			rep.PushErr = err.Error()
			logx.Logf("WARN", "[PUSH] 归档推送 GitHub 失败：%v\n%s", err, out)
		} else {
			rep.Pushed = true
			logx.Logf("INFO", "[PUSH] 归档已推送到 GitHub 数据仓")
		}
	}

	// ---- 2. 记录表清理（红线 RetainDays，默认 30 天）----
	ret, _ := RunRetentionCleanup(dryRun)
	rep.Retention = ret

	// ---- 3. 日志清理（红线 LogRetainDays，默认 30 天）----
	files, bytes := purgeAllLogs(cfg, repo.LogRetainDays(), dryRun)
	rep.LogFiles, rep.LogBytes = files, bytes

	// ---- 4. 磁盘守卫：C 盘可用 < 阈值 → 只保留当月 ----
	guard := repo.ArchiveMinFreeGB()
	if rep.FreeGBBefore < float64(guard) {
		rep.GuardFired = true
		rep.GuardKeepYM = thisYM
		// 收缩点默认是「本月月初」，但**不许越过还没归档的月份**：
		// 越过就等于把库里唯一一份数据删掉（缺口月份在远端根本没有副本）。
		// kmons 是升序的，第一个「有数据但没归档」的月份就是回退目标。
		cut := monthStartMs(now)
		for _, km := range kmons {
			if km.Month >= thisYM {
				break
			}
			if ArchiveExists(dir, km.Month) {
				continue
			}
			if ms, _, e := repo.MonthRange(km.Month); e == nil {
				cut = ms
				rep.GuardKeepYM = km.Month
				logx.Logf("WARN", "[GUARD] %s 尚未归档 → 收缩点回退到该月，"+
					"避免删掉库里唯一一份数据", km.Month)
			}
			break
		}
		if !dryRun {
			dr, err := dropKlineBefore(cut)
			if err != nil {
				rep.Err = "磁盘守卫失败：" + err.Error()
			}
			rep.GuardDropped = dr.Dropped
			rep.GuardRows = dr.Rows
		}
		// 归档侧「只留当月」。本地 archive/ 删掉后就只剩远端那一份，
		// 所以必须先确认推送成功；推送没成功就整轮跳过，宁可多占点磁盘。
		switch {
		case dryRun:
			f, b, _ := PruneArchivesBefore(dir, thisYM, true)
			rep.GuardArcFiles, rep.GuardArcBytes = f, b
			rep.GuardArcNote = "（预演：未删除）"
		case !rep.Pushed:
			rep.GuardArcNote = "推送未成功 → 跳过归档删除（本地 archive/ 是唯一副本）"
			logx.Logf("WARN", "[GUARD] 推送未成功，跳过归档删除 —— 本地 archive/ 是唯一副本")
		default:
			f, b, perr := PruneArchivesBefore(dir, thisYM, false)
			rep.GuardArcFiles, rep.GuardArcBytes = f, b
			if perr != nil {
				rep.GuardArcNote = "部分删除失败：" + perr.Error()
			} else {
				rep.GuardArcNote = "仅保留 " + thisYM + " 的归档"
			}
			logx.Logf("INFO", "[GUARD] 归档只留当月（%s）：删除 %d 个文件 / %.1f MB",
				thisYM, f, float64(b)/1048576)
		}
		logx.Logf("WARN", "[GUARD] C 盘可用 %.1f GB < %d GB 阈值 → K 线收缩到 %s（预演=%v）",
			rep.FreeGBBefore, guard, rep.GuardKeepYM, dryRun)
	}

	// ---- 5. 回收站 ----
	// 不可逆操作，给了一个反向开关（disable_recycle_clean）随时能停。
	if cfg != nil && cfg.Store != nil && cfg.Store.DisableRecycleClean {
		rep.Recycle = &RecycleResult{DryRun: dryRun, Err: "已在配置里关闭（disable_recycle_clean=true）"}
		logx.Logf("INFO", "[RECYCLE] 配置里关闭了回收站清理，本次跳过")
	} else {
		rep.Recycle = PurgeRecycleBin(dryRun)
	}

	rep.FreeGBAfter = freeGB()
	rep.Ms = time.Since(start).Milliseconds()
	rep.Summary = monthlySummary(rep)
	if !dryRun {
		logx.Logf("INFO", "[MAINT] %s（耗时 %d ms）", rep.Summary, rep.Ms)
	}
	return rep, nil
}

// ---------------------------------------------------------------------------
// 每日任务
// ---------------------------------------------------------------------------

// RunDailyMaintenance 每天跑一次：把超过 kline_retain_days（当前 10 天）的 K 线删掉。
//
// 用户口径（2026-10-01 二期）：
//
//	「只能查询保存最近 10 天数据，不能多，多出来就删除」
//	「自动每天凌晨删除数据一次」
//
// 只做 K 线一件事，别的都不碰：
//
//	· 记录表（历史仓位 / 成交 / 信号 / 权益 / 日志表）仍是独立的 30 天红线，
//	  跟月度任务走 —— 那是「查询历史交易记录」用的，10 天太短。
//	· 日志文件、回收站、GitHub 归档同理，都在月度任务里。
//
// 执行方式（这是它能在凌晨几秒内跑完的关键）：
//
//	① DROP PARTITION —— 整段过期的**日分区**直接扔掉，毫秒级。
//	                    分区方案已按天切（见 repo/partition.go），
//	                    所以「早于 cutoff 的那些天」全都能整段删。
//	② PurgeKlineBefore —— 只有跨在 cutoff 上的那一小段残余（通常几分钟的量）
//	                    需要逐行 DELETE，每批 5000 行、批间 20ms。
//
// 幂等：随时可以重复跑，删过一次之后第二次就是 0 行。
func RunDailyMaintenance(dryRun bool) (*MaintenanceReport, error) {
	start := time.Now()
	rep := &MaintenanceReport{
		Kind:      "daily",
		DryRun:    dryRun,
		StartedAt: start.Format("2006-01-02 15:04:05"),
	}
	rep.FreeGBBefore = freeGB()

	days := repo.KlineRetainDays()
	if days <= 0 {
		days = 10
	}
	cut := time.Now().AddDate(0, 0, -days).UnixMilli()

	if dryRun {
		// 只报「会删掉哪些分区」，一行都不删。
		//
		// ★ 判据必须走 repo.DroppablePartitions ★
		// 它和真跑的 DropKlinePartitionsBefore 是**同一个函数**，
		// 预演说删什么、真跑就删什么。原来这里自己写了一遍判断，
		// 漏了 p_old/pmax 的排除，预报出「将 DROP [p_old]」—— 预演在说谎。
		if pi, err := partitionRanges(); err == nil {
			rep.KlineDropped = repo.DroppablePartitions(pi, cut)
		}
		rep.KlineMonths = len(rep.KlineDropped)
		rep.Ms = time.Since(start).Milliseconds()
		rep.Summary = dailySummary(rep, days, cut)
		return rep, nil
	}

	dr, err := dropKlineBefore(cut)
	if err != nil {
		rep.Err = "K 线每日清理失败：" + err.Error()
	}
	if dr != nil {
		rep.KlineDropped = dr.Dropped
		rep.KlineRows = dr.Rows
		rep.KlineMB = dr.MB
	}
	rep.KlineMonths = len(rep.KlineDropped)

	rep.FreeGBAfter = freeGB()
	rep.Ms = time.Since(start).Milliseconds()
	rep.Summary = dailySummary(rep, days, cut)
	if !dryRun {
		logx.Logf("INFO", "[MAINT] %s（耗时 %d ms）", rep.Summary, rep.Ms)
	}
	return rep, nil
}

// partitionRanges 取 kline 现有分区区间（daily 的 dry-run 预告用）。
// 拿不到就返回 nil —— 预告失败不该让整个任务报错。
func partitionRanges() ([]repo.PartRange, error) {
	store := repo.NewStore(conf.LoadConfig())
	db, err := store.DB()
	if err != nil {
		return nil, err
	}
	return db.KlinePartitionRanges()
}

// dailySummary 把一次每日清理压成一行人话
func dailySummary(rep *MaintenanceReport, days int, cutMs int64) string {
	cutTxt := time.UnixMilli(cutMs).Format("2006-01-02 15:04")
	parts := fmt.Sprintf("每日 K 线清理：红线 %d 天（早于 %s 的都删）", days, cutTxt)
	if len(rep.KlineDropped) > 0 {
		// 预演时不报行数 —— 预演只查分区区间、不扫行，报「约 0 行」是假的。
		seg := fmt.Sprintf("；将整段 DROP %d 个日分区 [%s]",
			len(rep.KlineDropped), strings.Join(rep.KlineDropped, ", "))
		if rep.DryRun {
			parts += seg
		} else {
			parts += seg + fmt.Sprintf("，约 %d 行", rep.KlineRows)
		}
	} else if rep.KlineRows > 0 {
		parts += fmt.Sprintf("；边界残余删除 %d 行", rep.KlineRows)
	} else {
		parts += "；没有超期数据"
	}
	if rep.FreeGBAfter > 0 {
		parts += fmt.Sprintf("；C 盘可用 %.2fGB → %.2fGB", rep.FreeGBBefore, rep.FreeGBAfter)
	} else {
		parts += fmt.Sprintf("；C 盘可用 %.2fGB", rep.FreeGBBefore)
	}
	if rep.Err != "" {
		parts += "；⚠ " + rep.Err
	}
	return parts
}

// ---------------------------------------------------------------------------
// 年度任务
// ---------------------------------------------------------------------------

// RunYearlyMaintenance 每年跑一次：把超过一年的 K 线整段扔掉。
//
// 用户口径：「每年清除多余超过一年的数据，一年才运行一次清除任务就行」。
// 注意这带来的实际效果是 **数据跨度最长可达约两年**（上次清完到这次清之间
// 又攒了一整年）—— 期间如果磁盘吃紧，由月度任务的磁盘守卫兜底。
func RunYearlyMaintenance(dryRun bool) (*MaintenanceReport, error) {
	start := time.Now()
	rep := &MaintenanceReport{
		Kind:      "yearly",
		DryRun:    dryRun,
		StartedAt: start.Format("2006-01-02 15:04:05"),
	}
	cfg := conf.LoadConfig()
	rep.FreeGBBefore = freeGB()

	days := repo.KlineRetainDays()
	cut := time.Now().AddDate(0, 0, -days).UnixMilli()

	// 预告一下会删掉哪些月（dry run 也要有数）
	if months, err := monthsBefore(cut); err == nil {
		rep.KlineMonths = len(months)
		rep.GuardDropped = months
	}

	if !dryRun {
		dr, err := dropKlineBefore(cut)
		if err != nil {
			rep.Err = "K 线年度清理失败：" + err.Error()
		}
		rep.KlineDropped = dr.Dropped
		rep.KlineRows = dr.Rows
	}

	// 记录表也顺手扫一遍（红线 30 天），保证记录类数据不跟着留一年
	rep.Retention, _ = RunRetentionCleanup(dryRun)

	// 日志同样按月清理，年度任务里再兜一次
	files, bytes := purgeAllLogs(cfg, repo.LogRetainDays(), dryRun)
	rep.LogFiles, rep.LogBytes = files, bytes

	rep.FreeGBAfter = freeGB()
	rep.Ms = time.Since(start).Milliseconds()
	rep.Summary = yearlySummary(rep, days)
	if !dryRun {
		logx.Logf("INFO", "[MAINT] %s（耗时 %d ms）", rep.Summary, rep.Ms)
	}
	return rep, nil
}

// ---------------------------------------------------------------------------
// 共用动作
// ---------------------------------------------------------------------------

// pushGitHub 调 scripts\push_github.bat 把归档（或代码）推到 GitHub。
//
// what = "data"（只推归档仓）/ "code" / ""（两个都推）
//
// 为什么走 .bat 而不是在 Go 里直接调 git：
//   · 凭据、远端地址、分支名这些运维细节集中在一个脚本里，改的时候不用重编译；
//   · 脚本能在没有 token 时干净退出（退出码 2），不会卡在交互式密码提示上
//     把服务的月度任务永久挂住；
//   · git 对 Windows 服务的 Session 0 环境并不友好，交给 cmd 跑更稳。
//
// 失败**不算月度任务失败**：网络抖一下不该让整轮维护报错，
// 归档文件已经在本地了，下个月或手工补推都行。
func pushGitHub(what string) (string, error) {
	cfg := conf.LoadConfig()
	if cfg == nil {
		return "", fmt.Errorf("配置未加载")
	}
	root := cfg.Root()
	if root == "" {
		return "", fmt.Errorf("项目根目录未知")
	}
	bat := filepath.Join(root, "scripts", "push_github.bat")
	if _, err := os.Stat(bat); err != nil {
		return "", fmt.Errorf("找不到推送脚本 %s", bat)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "cmd", "/c", bat, what)
	cmd.Dir = root
	// 干掉交互式提示：万一 token 失效，宁可立刻失败也不要挂住服务
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// dropKlineBefore 分区级删除 + 残余行分批删。
//
// 两步走的原因见 kline_repo.go 的「分区级删除」注释：
// 整段过期的分区 DROP 掉是毫秒级；跨在边界上的那个分区只能逐行删。
func dropKlineBefore(cutMs int64) (*repo.PartitionDropResult, error) {
	cfg := conf.LoadConfig()
	store := repo.NewStore(cfg)
	db, err := store.DB()
	if err != nil {
		return &repo.PartitionDropResult{}, err
	}
	dr, err := db.DropKlinePartitionsBefore(cutMs)
	if err != nil {
		return dr, err
	}
	n, err := db.PurgeKlineBefore(cutMs)
	dr.Rows += n
	return dr, err
}

// monthsBefore 列出早于 cutMs 的月份标记（用于日志预告）
func monthsBefore(cutMs int64) ([]string, error) {
	cfg := conf.LoadConfig()
	store := repo.NewStore(cfg)
	db, err := store.DB()
	if err != nil {
		return nil, err
	}
	all, err := db.KlineMonths()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range all {
		if m.LastTs < cutMs {
			out = append(out, m.Month)
		}
	}
	return out, nil
}

// freeGB C 盘可用空间（GB）
func freeGB() float64 {
	return float64(FreeDiskMB(`C:\`)) / 1024
}

// monthStartMs 当前自然月 1 号 00:00 的毫秒时间戳
func monthStartMs(t time.Time) int64 {
	y, m, _ := t.Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, t.Location()).UnixMilli()
}

// klineMonthsOf 取「库里实际存在的月份」清单（升序，形如 2026-08 / 2026-09）。
//
// 归档（第 1 步：缺哪个月补哪个月）和磁盘守卫（第 4 步：收缩点不许越过
// 未归档月份）都要用它，所以把 Store/DB 的获取与错误收敛在一处。
// KlineMonths 是 *DB 上的方法，不是包级函数，这里顺手包一层。
func klineMonthsOf() ([]repo.KlineMonth, error) {
	store := repo.NewStore(conf.LoadConfig())
	db, err := store.DB()
	if err != nil {
		return nil, err
	}
	return db.KlineMonths()
}

// purgeAllLogs 清理所有日志目录（应用 + 数据库 + 网页）。
//
// 用户口径：「每个月要清除所有超过一个月的日志记录，包括数据库、
// 客户端、网页等日志」。所以这里扫两处：
//
//	<root>/logs        okxbot.log / apache_access.log / apache_error.log
//	                   mysql-error.log / mysql-slow.log / okxweb_*.log
//	<root>/apache/logs httpd 自身的 error_log / access_log
func purgeAllLogs(cfg *conf.Config, retainDays int, dryRun bool) ([]string, int64) {
	if cfg == nil {
		return nil, 0
	}
	var dirs []string
	if d := cfg.LogDirResolved(); d != "" {
		dirs = append(dirs, d)
	}
	if r := cfg.Root(); r != "" {
		dirs = append(dirs, filepath.Join(r, "apache", "logs"))
	}

	var allFiles []string
	var total int64
	for _, d := range dirs {
		files, bytes := purgeLogDir(cfg, d, retainDays, dryRun)
		allFiles = append(allFiles, files...)
		total += bytes
	}
	sort.Strings(allFiles)
	return allFiles, total
}

// purgeLogDir 删除一个日志目录里超过 retainDays 天没被写过的文件。
//
// 只删「文件」不动目录结构，并且刻意**跳过当前进程正在写的那个文件** ——
// okxbot.log 是 logx 自己写的，Windows 上删正在写入的文件会失败，
// 就算成功也会让后续日志全部丢失。
//
// 判断依据是 ModTime（最后写入时间），不是创建时间 ——
// 一个从 3 个月前就开始 append 的文件，它的创建时间很老但内容是最新的。
func purgeLogDir(cfg *conf.Config, dir string, retainDays int, dryRun bool) ([]string, int64) {
	if dir == "" {
		return nil, 0
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil, 0
	}

	// 当前正在写的文件不碰
	var live string
	if cfg.Store != nil && cfg.Store.Enabled {
		live = filepath.Join(cfg.LogDirResolved(), "okxbot.log")
	}
	cut := time.Now().AddDate(0, 0, -retainDays)

	var removed []string
	var freed int64

	_ = filepath.WalkDir(dir, func(p string, de fs.DirEntry, err error) error {
		if err != nil || de == nil {
			return nil // 权限 / 竞争问题直接跳过，不中断整轮清理
		}
		if de.IsDir() {
			// 只下探已知的子目录，别把别的东西一起扫了
			if p != dir {
				name := strings.ToLower(de.Name())
				if name != "archive" && name != "shots" && name != "old" {
					return fs.SkipDir
				}
			}
			return nil
		}
		if live != "" && strings.EqualFold(p, live) {
			return nil
		}
		// 只认日志 / 归档类文件，避免误删用户放进 logs 目录的别的东西
		low := strings.ToLower(de.Name())
		if !(strings.HasSuffix(low, ".log") || strings.Contains(low, ".log.") ||
			strings.HasSuffix(low, ".gz") || strings.HasSuffix(low, ".err") ||
			isShot(low)) {
			return nil
		}
		info, ierr := de.Info()
		if ierr != nil || info.ModTime().After(cut) {
			return nil
		}
		if dryRun {
			removed = append(removed, p+"（预览）")
			freed += info.Size()
			return nil
		}
		if rerr := os.Remove(p); rerr == nil {
			removed = append(removed, p)
			freed += info.Size()
		}
		return nil
	})

	sort.Strings(removed)
	return removed, freed
}

// isShot 判断是不是截图类文件（logs/shots/ 下的产物）
func isShot(low string) bool {
	for _, ext := range []string{".png", ".jpg", ".jpeg", ".webp"} {
		if strings.HasSuffix(low, ext) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 汇总文案
// ---------------------------------------------------------------------------

func monthlySummary(rep *MaintenanceReport) string {
	head := "月度维护"
	if rep.DryRun {
		head += "（预演，未删除）"
	}
	var parts []string

	if n := len(rep.Archives); n > 0 {
		var rows, bytes int64
		var nparts int
		for _, m := range rep.Archives {
			rows += m.TotalRows
			bytes += m.TotalBytes
			nparts += len(m.Parts)
		}
		parts = append(parts, fmt.Sprintf("归档 %d 个月（%s）：%d 行 / %d 分片 / %.1f MB",
			n, strings.Join(rep.ArchivedMonths, ","), rows, nparts, float64(bytes)/1048576))
	} else if rep.DryRun && len(rep.ArchivedMonths) > 0 {
		parts = append(parts, fmt.Sprintf("预演：将归档 %d 个月（%s）",
			len(rep.ArchivedMonths), strings.Join(rep.ArchivedMonths, ",")))
	} else if rep.ArchiveErr != "" {
		parts = append(parts, "归档失败："+rep.ArchiveErr)
	} else if n := len(rep.ArchiveSkipped); n > 0 {
		parts = append(parts, fmt.Sprintf("归档已是最新（%d 个月已有）", n))
	}
	if rep.Pushed {
		parts = append(parts, "已推送 GitHub")
	} else if rep.PushErr != "" {
		parts = append(parts, "推送失败："+rep.PushErr)
	}
	if rep.Retention != nil {
		_, t := repo.RetentionSummary(rep.Retention.DB)
		parts = append(parts, t)
		if rep.Retention.KlineRows > 0 {
			parts = append(parts, fmt.Sprintf("K线 %d 行", rep.Retention.KlineRows))
		}
	}
	if len(rep.LogFiles) > 0 {
		parts = append(parts, fmt.Sprintf("日志 %d 个（%.1f MB）",
			len(rep.LogFiles), float64(rep.LogBytes)/1048576))
	}
	if rep.GuardFired {
		g := fmt.Sprintf("★磁盘守卫触发（%.1f GB < %d GB）：K 线保留到 %s，DROP %d 个分区 / %d 行",
			rep.FreeGBBefore, repo.ArchiveMinFreeGB(), rep.GuardKeepYM,
			len(rep.GuardDropped), rep.GuardRows)
		if rep.GuardKeepYM != "" && rep.GuardKeepYM < time.Now().Format("2006-01") {
			g += "（有未归档月份，收缩点已回退）"
		}
		if rep.GuardArcNote != "" {
			g += "；归档 " + rep.GuardArcNote
		} else if rep.GuardArcFiles > 0 {
			g += fmt.Sprintf("；归档删 %d 个 / %.1f MB", rep.GuardArcFiles,
				float64(rep.GuardArcBytes)/1048576)
		}
		parts = append(parts, g)
	}
	if rep.Recycle != nil {
		if rep.Recycle.Err != "" {
			parts = append(parts, "回收站："+rep.Recycle.Err)
		} else if rep.Recycle.Items > 0 {
			parts = append(parts, fmt.Sprintf("回收站 %d 个文件（%.1f MB）",
				rep.Recycle.Items, float64(rep.Recycle.Bytes)/1048576))
		} else {
			parts = append(parts, "回收站为空")
		}
	}
	parts = append(parts, fmt.Sprintf("C 盘 %.1f→%.1f GB", rep.FreeGBBefore, rep.FreeGBAfter))
	if rep.Err != "" {
		parts = append(parts, "错误："+rep.Err)
	}
	return head + "：" + strings.Join(parts, " · ")
}

func yearlySummary(rep *MaintenanceReport, days int) string {
	head := "年度清理"
	if rep.DryRun {
		head += "（预演，未删除）"
	}
	var parts []string
	parts = append(parts, fmt.Sprintf("K 线红线 %d 天", days))
	if rep.DryRun {
		parts = append(parts, fmt.Sprintf("将删除 %d 个月的历史分区", rep.KlineMonths))
	} else {
		parts = append(parts, fmt.Sprintf("DROP %d 个分区 / 共 %d 行",
			len(rep.KlineDropped), rep.KlineRows))
	}
	if rep.Retention != nil {
		_, t := repo.RetentionSummary(rep.Retention.DB)
		parts = append(parts, t)
	}
	if len(rep.LogFiles) > 0 {
		parts = append(parts, fmt.Sprintf("日志 %d 个（%.1f MB）",
			len(rep.LogFiles), float64(rep.LogBytes)/1048576))
	}
	parts = append(parts, fmt.Sprintf("C 盘 %.1f→%.1f GB", rep.FreeGBBefore, rep.FreeGBAfter))
	if rep.Err != "" {
		parts = append(parts, "错误："+rep.Err)
	}
	return head + "：" + strings.Join(parts, " · ")
}

// ---------------------------------------------------------------------------
// 手动入口：-cleanup / -cleanup-dry
// ---------------------------------------------------------------------------

// RetentionReport 一次清理的完整结果（控制台 / 接口 / 日志都用它）
type RetentionReport struct {
	Days      int                  `json:"days"`
	KlineDays int                  `json:"klineDays"`
	LogDays   int                  `json:"logDays"`
	DryRun    bool                 `json:"dryRun"`
	StartedAt string               `json:"startedAt"`
	Ms        int64                `json:"ms"`
	DB        []repo.RetentionStep `json:"db"`
	KlineRows int64                `json:"klineRows"`
	LogFiles  []string             `json:"logFiles"`
	LogBytes  int64                `json:"logBytes"`
	Summary   string               `json:"summary"`
	Err       string               `json:"err,omitempty"`
}

// RunRetentionCleanup 跑一次「记录表 + 日志」清理。
//
//	dryRun=true  → 只统计超期数据量，一行不删、一个文件不动
//	dryRun=false → 真删
//
// 注意这里**不碰 K 线**：K 线的红线是 KlineRetainDays（365 天），
// 由年度任务负责，口径和记录表（30 天）完全不同。
func RunRetentionCleanup(dryRun bool) (*RetentionReport, error) {
	start := time.Now()
	days := repo.RetainDays()
	rep := &RetentionReport{
		Days:      days,
		KlineDays: repo.KlineRetainDays(),
		LogDays:   repo.LogRetainDays(),
		DryRun:    dryRun,
		StartedAt: start.Format("2006-01-02 15:04:05"),
	}

	cfg := conf.LoadConfig()
	store := repo.NewStore(cfg)
	db, err := store.DB()
	if err != nil {
		rep.Err = err.Error()
		rep.Summary = "数据保留：连接数据库失败（" + err.Error() + "）"
		return rep, err
	}

	if dryRun {
		steps, err := db.RetentionPreview(days)
		rep.DB = steps
		if err != nil {
			rep.Err = err.Error()
		}
	} else {
		steps, err := db.PurgeExpired(days)
		rep.DB = steps
		if err != nil {
			rep.Err = err.Error()
		}
	}

	// ---- 日志文件：红线 LogRetainDays（30 天）----
	files, bytes := purgeAllLogs(cfg, repo.LogRetainDays(), dryRun)
	rep.LogFiles, rep.LogBytes = files, bytes

	rep.Ms = time.Since(start).Milliseconds()
	rep.Summary = retentionSummary(rep)
	if !dryRun {
		logx.Logf("INFO", "[CLEAN] %s（耗时 %d ms）", rep.Summary, rep.Ms)
	}
	return rep, nil
}

// retentionSummary 一行汇总
func retentionSummary(rep *RetentionReport) string {
	head := "数据保留"
	if rep.DryRun {
		head = "数据保留（预演，未删除）"
	}

	_, dbText := repo.RetentionSummary(rep.DB)
	parts := []string{fmt.Sprintf("记录表 %d 天窗口", rep.Days), dbText}
	if rep.KlineRows > 0 {
		parts = append(parts, fmt.Sprintf("kline %d 行", rep.KlineRows))
	}
	if len(rep.LogFiles) > 0 {
		parts = append(parts, fmt.Sprintf("日志文件 %d 个（%.1f MB）", len(rep.LogFiles), float64(rep.LogBytes)/1024/1024))
	}
	if rep.Err != "" {
		parts = append(parts, "错误："+rep.Err)
	}
	return head + "：" + strings.Join(parts, " · ")
}

// RetentionText 把报告渲染成多行文本（-cleanup 控制台输出 / 网页自查用）
func RetentionText(rep *RetentionReport) string {
	if rep == nil {
		return "（无结果）"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "== 自动数据删除程序 · 记录表 %d 天 / 日志 %d 天 / K线 %d 天 ==\n",
		rep.Days, rep.LogDays, rep.KlineDays)
	if rep.DryRun {
		b.WriteString("模式：预演（只统计，不删除）\n")
	} else {
		b.WriteString("模式：实际删除\n")
	}
	b.WriteString("----------------------------------------------\n")
	fmt.Fprintf(&b, "%-14s %-12s %10s %10s  %s\n", "表", "时间列", "超期行", "已删除", "说明")
	var totBefore, totDel int64
	for _, s := range rep.DB {
		totBefore += s.Before
		totDel += s.Deleted
		note := s.Note
		if s.Err != "" {
			note = "✘ " + s.Err
		}
		fmt.Fprintf(&b, "%-14s %-12s %10d %10d  %s\n", s.Table, s.Column, s.Before, s.Deleted, note)
	}
	if len(rep.DB) == 0 {
		b.WriteString("（没有需要清理的表）\n")
	}
	fmt.Fprintf(&b, "%-14s %-12s %10d %10d\n", "合计", "", totBefore, totDel)
	b.WriteString("----------------------------------------------\n")
	if len(rep.LogFiles) > 0 {
		fmt.Fprintf(&b, "日志文件：删除/命中 %d 个，共 %.1f MB\n",
			len(rep.LogFiles), float64(rep.LogBytes)/1024/1024)
		for _, f := range rep.LogFiles {
			fmt.Fprintf(&b, "  · %s\n", f)
		}
	} else {
		fmt.Fprintf(&b, "日志文件：%d 天内没有过期文件\n", rep.LogDays)
	}
	fmt.Fprintf(&b, "耗时：%d ms\n", rep.Ms)
	if rep.Err != "" {
		fmt.Fprintf(&b, "错误：%s\n", rep.Err)
	}
	return b.String()
}

// MaintenanceText 把月度/年度维护报告渲染成多行文本（-maint / -maint-yearly 控制台输出）
func MaintenanceText(rep *MaintenanceReport) string {
	if rep == nil {
		return "（无结果）"
	}
	var b strings.Builder
	title := "月度维护"
	switch rep.Kind {
	case "yearly":
		title = "年度清理"
	case "daily":
		title = "每日 K 线清理"
	}
	fmt.Fprintf(&b, "== 自动维护程序 · %s ==\n", title)
	if rep.DryRun {
		b.WriteString("模式：预演（只报告，不删除）\n")
	} else {
		b.WriteString("模式：实际执行\n")
	}
	b.WriteString("----------------------------------------------\n")

	if rep.Kind == "monthly" {
		// 注意：这里刻意不用 %-10s 之类的宽度补齐来对齐中文标签 ——
		// Go 的宽度按**字节**算，而「归档目录」(12 字节) 和「待归档」(9 字节)
		// 显示宽度都是 4 个汉字，补齐后反而会错位。标签统一写成等宽的固定串。
		fmt.Fprintf(&b, "归档目录   : %s\n", rep.ArchiveDir)
		if len(rep.ArchiveSkipped) > 0 {
			fmt.Fprintf(&b, "已存在归档 : %s（跳过，不重复导出）\n",
				strings.Join(rep.ArchiveSkipped, ", "))
		}
		switch {
		case len(rep.ArchivedMonths) == 0:
			b.WriteString("待归档月份 : （无 —— 所有早于本月的月份都已有归档）\n")
		case len(rep.Archives) == 0:
			// 预演：能算出要导哪些月份，但没真导，所以没有清单
			fmt.Fprintf(&b, "待归档月份 : %s（预演：未导出）\n",
				strings.Join(rep.ArchivedMonths, ", "))
		default:
			fmt.Fprintf(&b, "本次归档   : %d 个月\n", len(rep.Archives))
			for i, m := range rep.Archives {
				fmt.Fprintf(&b, "  [%d] %s : %d 行 / %d 合约 / %d 分片 / %.1f MB\n",
					i+1, rep.ArchivedMonths[i], m.TotalRows, m.Insts,
					len(m.Parts), float64(m.TotalBytes)/1048576)
				for _, p := range m.Parts {
					fmt.Fprintf(&b, "      · %-42s %8d 行  %6.1f MB\n",
						p.Name, p.Rows, float64(p.Bytes)/1048576)
				}
			}
		}
		if rep.ArchiveErr != "" {
			fmt.Fprintf(&b, "归档异常   : × %s\n", rep.ArchiveErr)
		}
		switch {
		case rep.Pushed:
			b.WriteString("GitHub 推送: 成功（数据仓）\n")
		case rep.PushErr != "":
			fmt.Fprintf(&b, "GitHub 推送: × %s\n", rep.PushErr)
			for _, ln := range strings.Split(rep.PushOut, "\n") {
				if strings.TrimSpace(ln) != "" {
					fmt.Fprintf(&b, "             %s\n", strings.TrimRight(ln, "\r"))
				}
			}
		}
	}

	if rep.Retention != nil {
		var totBefore, totDel int64
		b.WriteString("\n【记录表清理】红线 " + fmt.Sprint(rep.Retention.Days) + " 天\n")
		fmt.Fprintf(&b, "%-14s %-12s %10s %10s  %s\n", "表", "时间列", "超期行", "已删除", "说明")
		for _, s := range rep.Retention.DB {
			totBefore += s.Before
			totDel += s.Deleted
			note := s.Note
			if s.Err != "" {
				note = "✘ " + s.Err
			}
			fmt.Fprintf(&b, "%-14s %-12s %10d %10d  %s\n", s.Table, s.Column, s.Before, s.Deleted, note)
		}
		fmt.Fprintf(&b, "%-14s %-12s %10d %10d\n", "合计", "", totBefore, totDel)
	}

	// 日志清理是月度/年度任务的一步，不属于每日 K 线清理 ——
	// 每日清理时打这行只会让人以为「每日任务也在管日志」。
	if rep.Kind != "daily" {
		if len(rep.LogFiles) > 0 {
			fmt.Fprintf(&b, "\n【日志清理】共 %d 个文件 / %.1f MB\n", len(rep.LogFiles), float64(rep.LogBytes)/1048576)
			for _, f := range rep.LogFiles {
				fmt.Fprintf(&b, "  · %s\n", f)
			}
		} else {
			fmt.Fprintf(&b, "\n【日志清理】%d 天内没有过期日志文件\n", repo.LogRetainDays())
		}
	}

	if rep.Kind == "daily" {
		// 每日清理只有一件事（按 10 天红线 DROP / 删 K 线），所以直接打
		// dailySummary 那一行人话 —— 它与常驻服务日志里的 [MAINT] 行**同源**，
		// 手工核对时看到的就是线上实际会做的事，不会对不上。
		//
		// 原来这里没有 daily 分支：标题掉回「月度维护」、
		// 分区清单被整段吞掉，手工预演等于什么都看不到。
		b.WriteString("\n【K 线每日清理】\n")
		fmt.Fprintf(&b, "  %s\n", rep.Summary)
		if rep.DryRun {
			b.WriteString("  预演：上面列出的分区**一个都没删**；去掉 -maint-daily-dry 才会真删。\n")
		}
	}

	if rep.Kind == "yearly" {
		fmt.Fprintf(&b, "\n【K 线年度清理】红线 %d 天\n", repo.KlineRetainDays())
		if rep.DryRun {
			fmt.Fprintf(&b, "  将删除 %d 个月的历史分区：%s\n", rep.KlineMonths, strings.Join(rep.GuardDropped, ", "))
		} else {
			fmt.Fprintf(&b, "  DROP 分区 %d 个，共 %d 行：%s\n",
				len(rep.KlineDropped), rep.KlineRows, strings.Join(rep.KlineDropped, ", "))
		}
	}

	if rep.Kind == "monthly" {
		fmt.Fprintf(&b, "\n【磁盘守卫】阈值 %d GB · 当前 C 盘可用 %.1f GB → %s\n",
			repo.ArchiveMinFreeGB(), rep.FreeGBBefore,
			map[bool]string{true: "★ 触发：只保留当月", false: "未触发（空间充足）"}[rep.GuardFired])
		if rep.GuardFired {
			fmt.Fprintf(&b, "  K 线保留到：%s", rep.GuardKeepYM)
			if rep.GuardKeepYM < time.Now().Format("2006-01") {
				fmt.Fprintf(&b, "（有未归档月份，收缩点从本月回退，避免删掉唯一副本）")
			}
			b.WriteString("\n")
			if rep.DryRun {
				fmt.Fprintf(&b, "  将 DROP：未知（预演不执行）\n")
			} else {
				fmt.Fprintf(&b, "  DROP %d 个分区 / %d 行：%s\n", len(rep.GuardDropped),
					rep.GuardRows, strings.Join(rep.GuardDropped, ", "))
			}
			if rep.GuardArcNote != "" {
				fmt.Fprintf(&b, "  归档侧：%s", rep.GuardArcNote)
				if rep.GuardArcFiles > 0 {
					fmt.Fprintf(&b, "（%d 个文件 / %.1f MB）", rep.GuardArcFiles,
						float64(rep.GuardArcBytes)/1048576)
				}
				b.WriteString("\n")
			}
		}
	}

	if rep.Recycle != nil {
		fmt.Fprintf(&b, "\n【回收站】")
		switch {
		case rep.Recycle.Err != "":
			fmt.Fprintf(&b, "✘ %s\n", rep.Recycle.Err)
		case rep.Recycle.Items == 0:
			b.WriteString("已经是空的\n")
		default:
			fmt.Fprintf(&b, "清理 %d 个文件 / %.1f MB（%d 个目录，跳过 %d 项）\n",
				rep.Recycle.Items, float64(rep.Recycle.Bytes)/1048576,
				rep.Recycle.Dirs, rep.Recycle.Skipped)
			for _, r := range rep.Recycle.Roots {
				fmt.Fprintf(&b, "  · %s\n", r)
			}
		}
	}

	b.WriteString("----------------------------------------------\n")
	if rep.FreeGBAfter > 0 {
		fmt.Fprintf(&b, "C 盘可用：%.1f GB → %.1f GB\n", rep.FreeGBBefore, rep.FreeGBAfter)
	} else {
		// 预演（或没测到）时 FreeGBAfter 是 0，直接按两段打会变成
		// 「20.5 GB → 0.0 GB」，看着像把 C 盘写爆了。
		fmt.Fprintf(&b, "C 盘可用：%.1f GB\n", rep.FreeGBBefore)
	}
	fmt.Fprintf(&b, "耗时：%d ms\n", rep.Ms)
	if rep.Err != "" {
		fmt.Fprintf(&b, "错误：%s\n", rep.Err)
	}
	return b.String()
}
