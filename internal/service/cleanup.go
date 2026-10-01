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
	// maintTickInterval 巡检间隔。30 分钟足够「月度/年度」这种粗粒度，
	// 又不至于让服务空转 —— 每次 tick 只是读两行 meta 做字符串比较。
	maintTickInterval = 30 * time.Minute

	// maintFirstDelay 启动后延迟。回补 / 信号回算 / 成交同步都在前 60 秒内起跑，
	// 维护任务会跑批量 DELETE 和 ALTER TABLE，错开一下避免抢 IO。
	maintFirstDelay = 90 * time.Second

	// metaLastMonthly / metaLastYearly meta 表里的「上次跑过哪个月/哪一年」
	metaLastMonthly = "maint_last_monthly"
	metaLastYearly  = "maint_last_yearly"
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

	lastMonth, _, _ := db.GetMeta(metaLastMonthly)
	lastYear, _, _ := db.GetMeta(metaLastYearly)

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

	ArchivedMonth  string           `json:"archivedMonth,omitempty"`
	Archive        *ArchiveManifest `json:"archive,omitempty"`
	ArchiveDir     string           `json:"archiveDir,omitempty"`
	ArchiveErr     string           `json:"archiveErr,omitempty"`
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

	KlineDropped []string `json:"klineDropped,omitempty"`
	KlineRows    int64    `json:"klineRows"`
	KlineMonths  int      `json:"klineMonths"`

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

	// ---- 1. 归档上个月 ----
	prev := time.Now().AddDate(0, -1, 0).Format("2006-01")
	rep.ArchivedMonth = prev
	dir := ArchiveDir()
	rep.ArchiveDir = dir
	if dryRun {
		rep.ArchiveErr = "（预演：未导出）"
	} else {
		man, err := ExportMonth(prev, dir)
		rep.Archive = man
		if err != nil {
			rep.ArchiveErr = err.Error()
		} else {
			logArchive(man, dir, nil)
			if _, werr := WriteManifest(dir, man); werr != nil {
				rep.ArchiveErr = "清单写入失败：" + werr.Error()
			}
		}
	}

	// ---- 1.5 推送到 GitHub（必须在磁盘守卫之前！）----
	//
	// 顺序很关键：磁盘守卫可能把 K 线收缩到「只留当月」，
	// 上个月的数据一旦被删就再也导不出来了 —— 所以先确保它安全落在远端。
	// 推送失败不算任务失败（归档已在本地 archive/），只记一条警告。
	if !dryRun && rep.ArchiveErr == "" {
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

	// ---- 4. 磁盘守卫 ----
	guard := repo.ArchiveMinFreeGB()
	if rep.FreeGBBefore < float64(guard) {
		rep.GuardFired = true
		cut := monthStartMs(time.Now())
		if !dryRun {
			dr, err := dropKlineBefore(cut)
			if err != nil {
				rep.Err = "磁盘守卫失败：" + err.Error()
			}
			rep.GuardDropped = dr.Dropped
			rep.GuardRows = dr.Rows
		}
		logx.Logf("WARN", "[GUARD] C 盘可用 %.1f GB < %d GB 阈值 → K 线收缩到当月（预演=%v）",
			rep.FreeGBBefore, guard, dryRun)
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

	if rep.Archive != nil {
		parts = append(parts, fmt.Sprintf("归档 %s：%d 行 / %d 分片 / %.1f MB",
			rep.ArchivedMonth, rep.Archive.TotalRows, len(rep.Archive.Parts),
			float64(rep.Archive.TotalBytes)/1048576))
	} else if rep.ArchiveErr != "" {
		parts = append(parts, "归档失败："+rep.ArchiveErr)
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
		parts = append(parts, fmt.Sprintf("★磁盘守卫触发（%.1f GB < %d GB）：K 线只留当月，DROP %d 个分区 / %d 行",
			rep.FreeGBBefore, repo.ArchiveMinFreeGB(), len(rep.GuardDropped), rep.GuardRows))
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
	if rep.Kind == "yearly" {
		title = "年度清理"
	}
	fmt.Fprintf(&b, "== 自动维护程序 · %s ==\n", title)
	if rep.DryRun {
		b.WriteString("模式：预演（只报告，不删除）\n")
	} else {
		b.WriteString("模式：实际执行\n")
	}
	b.WriteString("----------------------------------------------\n")

	if rep.Kind == "monthly" {
		fmt.Fprintf(&b, "归档月份   : %s\n", rep.ArchivedMonth)
		fmt.Fprintf(&b, "归档目录   : %s\n", rep.ArchiveDir)
		if rep.Archive != nil {
			fmt.Fprintf(&b, "归档结果   : %d 行 / %d 个合约 / %d 个分片 / %.1f MB\n",
				rep.Archive.TotalRows, rep.Archive.Insts, len(rep.Archive.Parts),
				float64(rep.Archive.TotalBytes)/1048576)
			for _, p := range rep.Archive.Parts {
				fmt.Fprintf(&b, "             · %-42s %8d 行  %6.1f MB\n",
					p.Name, p.Rows, float64(p.Bytes)/1048576)
			}
			if rep.Archive.From != "" {
				fmt.Fprintf(&b, "时间范围   : %s ~ %s\n", rep.Archive.From, rep.Archive.To)
			}
		} else if rep.ArchiveErr != "" {
			fmt.Fprintf(&b, "归档结果   : × %s\n", rep.ArchiveErr)
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

	if len(rep.LogFiles) > 0 {
		fmt.Fprintf(&b, "\n【日志清理】共 %d 个文件 / %.1f MB\n", len(rep.LogFiles), float64(rep.LogBytes)/1048576)
		for _, f := range rep.LogFiles {
			fmt.Fprintf(&b, "  · %s\n", f)
		}
	} else {
		fmt.Fprintf(&b, "\n【日志清理】%d 天内没有过期日志文件\n", repo.LogRetainDays())
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
			map[bool]string{true: "★ 触发：K 线只保留当月", false: "未触发（空间充足）"}[rep.GuardFired])
		if rep.GuardFired && !rep.DryRun {
			fmt.Fprintf(&b, "  DROP %d 个分区 / %d 行\n", len(rep.GuardDropped), rep.GuardRows)
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
	fmt.Fprintf(&b, "C 盘可用：%.1f GB → %.1f GB\n", rep.FreeGBBefore, rep.FreeGBAfter)
	fmt.Fprintf(&b, "耗时：%d ms\n", rep.Ms)
	if rep.Err != "" {
		fmt.Fprintf(&b, "错误：%s\n", rep.Err)
	}
	return b.String()
}
