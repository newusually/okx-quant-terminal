package conf

// config.go —— 策略配置加载（热加载，改文件即生效，不用重启）
//
// 只读 configs/okx_strategy.json，不碰项目里任何其它文件。
// 支持 // 与 /* */ 注释（读之前先剥掉），也支持 "_xxx" 形式的注释键（JSON 解析时自动忽略）。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"finally-main/internal/logx"
)

// ---------------------------------------------------------------------------
// 配置结构
// ---------------------------------------------------------------------------

type EntryCfg struct {
	TdMode                 string  `json:"td_mode"`
	PosSide                string  `json:"pos_side"`
	OrdType                string  `json:"ord_type"`
	MarginUSDT             float64 `json:"margin_usdt"`
	Leverage               int     `json:"leverage"`
	MaxConcurrentPositions int     `json:"max_concurrent_positions"`
	CooldownBars           int     `json:"cooldown_bars"`
	DailyMaxEntries        int     `json:"daily_max_entries"`

	// MarginPolicy 决定「算出来买不起 1 张」怎么办：
	//   "fixed"   —— 严格用 MarginUSDT，买不起就跳过（默认，忠于原口径）
	//   "min_one" —— 自动把保证金放大到刚好能买 1 张，但不超过 MaxMarginUSDT
	MarginPolicy  string  `json:"margin_policy"`
	MaxMarginUSDT float64 `json:"max_margin_usdt"`

	// MinBarRisePct 触发信号的那根 K 线的**带符号涨跌幅门槛**（% = (收−开)÷开×100），
	// 共振达标但涨跌幅不过门槛也不下单。**买入与加仓共用同一个判据**
	// （service.SignalQualified），所以它天然满足「加仓条件与买入一致」。
	//
	//	> 0  RisePct 严格大于该值才通过（「必须真涨」；涨恰好该值不算）
	//	< 0  RisePct 严格小于该值才通过（「必须真跌」；默认 -0.7，即「必须跌超 0.7%」）
	//	= 0  显式关闭这个条件（只看 score）
	//	nil  没写这个键 → 用默认 DefaultMinBarRisePct（当前 -0.7）
	//
	// ⚠ 两个方向都是**严格**比较：RisePct 恰好等于门槛值 → 拒绝。
	//
	// ★ 为什么用指针而不是 float64 ★
	//   值类型下「没写」和「写了 0」都是 0，两者语义完全相反：
	//     没写 → 应当用默认 DefaultMinBarRisePct（漏配时条件仍在，不会静默放开全市场下单）
	//     写 0 → 应当真的关掉这个条件
	//   二期在 MaxConcurrentPositions / DailyMaxEntries 上正是栽在
	//   「0 被 `<= 0` 反压回默认值」这一步（用户写了 0 想取消限制，配置却静默失效）。
	//   指针是唯一能不歧义表达三态的写法。读取一律走 Config.MinBarRisePct()。
	//   ★ 六期起负数是有意义的（「必须真跌」），**不许**再把负数归一化回默认值。
	MinBarRisePct *float64 `json:"min_bar_rise_pct"`
}

// ExitCfg 出场参数。
//
// ★ 2026-10-02 四期：用户口径「不准平仓，不准爆仓，只能超时 1 小时自动平仓」。
// 这里的「不准平仓」= 关掉**布林上轨那种乱平仓**，**不是**把止盈也关掉 ——
// 用户后续明确纠正：「止盈 1% 不平仓有问题」。
// 所以保留两条通道：**止盈 +0.3%**（锁利）+ **超时 60 分钟**（兜底离场）；
// 布林上轨关闭，不设止损。
//
// 判定处（service/trader.go 的 runExits）一律带 `> 0` / bool 前置，
// 所以 0 / false 就是「关闭」，不会被别的兜底逻辑反压回默认值。
type ExitCfg struct {
	// TakeProfitPct 止盈线（%）。**七期口径 0.35** —— 浮盈到 +0.35% 立刻市价平。
	// 0 才是关闭；实测把它关掉后，超时平仓的盈亏纯随机且净值为负
	// （2026-10-02 00:40~00:55 的 9 笔：6 笔未止盈合计 -0.0924U）。
	TakeProfitPct float64 `json:"take_profit_pct"`

	// BollUpperExit 布林上轨出场：收盘价 > SMA20 + 2σ 就平。
	// **四期起默认关闭** —— 它与买入判据读同一根 K 线，会「秒进秒出」：
	// 那根既涨过入场门槛、又被判上轨，开仓后下一轮 3 秒巡检立刻反手平掉
	// （实测 SNDK 开仓 15 秒即平、亏 0.17%）。
	BollUpperExit bool `json:"boll_upper_exit"`

	// MaxHoldBars 超时平仓（按「根」算）。0 = 关闭。
	// 注意它依赖持仓自己的周期，1H 图和 15m 图的 4 根完全不是一个时长，
	// 所以更推荐用 MaxHoldMinutes。
	MaxHoldBars int `json:"max_hold_bars"`

	// MaxHoldMinutes 超时平仓（按「分钟」算）。>0 时优先于 MaxHoldBars。
	// 七期起默认 **1440**（开仓满 24 小时自动市价平掉），兜底出场通道之一。
	// 实时巡检每 3 秒判一次，到点立刻出，不用等下一根 K 线收盘。
	MaxHoldMinutes int `json:"max_hold_minutes"`

	// StopLossPct 硬止损（%）：浮盈亏 pnlPct <= -StopLossPct 就平。
	// **七期口径 300** —— 用户要求「止损 -300%」：价格类浮亏到 300% 物理上不可能
	// （浮亏 100% 即归零），所以这条线只是形式兜底，实际效果 = 不设止损。
	// 判定在 trader.go runExits：`StopLossPct > 0 && pnlPct <= -StopLossPct`。
	// 0 = 关闭。
	StopLossPct float64 `json:"stop_loss_pct"`
}

// AddonAutoBar RiseBar 的取值：用「该仓位自己的周期」。
//
// 加仓条件已改成与买入一致（8 指标共振），而买入是按某个周期扫出来的，
// 所以加仓也应该按「这笔仓位当初是哪个周期开的」去判 —— 1m 开的仓按 1m 判、
// 15m 开的仓按 15m 判。写 "auto" 就是让引擎去读 p.Bar。
const AddonAutoBar = "auto"

// 加仓判据的两套模式（★ 2026-10-02 八期新增，用户要求「两套都留、页面可选」）。
//
// 历史包袱说明：加仓条件的语义被来回改过三次（一期价格 → 二期共振 → 七期价格），
// 每次改都要重写判定、回滚时又找不到旧逻辑。八期不再「推翻重来」，
// 而是把两种判据**并存**，由 addon.mode 选一个生效：
//
//	AddonModeResonance —— 共振模式（用户八期口径）：
//	    最后一根已收盘 K 线 score > addon.score_threshold
//	    且该根涨幅 > addon.bar_rise_pct（严格大于，正数=必须真涨）。
//
//	AddonModePrice —— 价格模式（七期口径，保留可用）：
//	    收盘价比买入价低超过 addon.drop_pct%
//	    且该根涨幅 > addon.price_rise_pct（严格大于）。
//
// 空字符串按 resonance 处理（老配置没写 mode 时走用户当前想要的那套）。
const (
	AddonModeResonance = "resonance"
	AddonModePrice     = "price"
)

// AddonCfg 加仓（补仓 / 摊薄均价）。
//
// 现行口径（2026-10-02 八期，用户指定）：
//
//	加仓额 = 显式金额 MarginUSDT（>0）或 原持仓保证金 × Ratio（默认 1/3）
//	触发   = 由 Mode 决定的两套判据之一（见上面两个常量）
//	次数   = **不限**（MaxTimes = 0）
//
// ★ 历史变迁：一期「15m 先跌 DropPct% 后转涨」（相对窗口低点）→ 二期改成与买入一致
//
//	（8 因子共振，DropPct 因此废弃）→ 七期改回价格条件（DropPct 复活，语义变为
//	「相对**买入价**低 N%」）→ 八期两套并存，加 Mode 开关。
type AddonCfg struct {
	Enabled bool `json:"enabled"`

	// Mode 用哪套加仓判据：AddonModeResonance（默认）/ AddonModePrice。
	// 空 = resonance。写别的值会被 fillDefaults 反压回 resonance（不静默走错分支）。
	Mode string `json:"mode"`

	// ScoreThreshold 【共振模式】加仓要求的分数门槛。0 = 用顶层 ScoreThreshold
	//   （这样「加仓门槛」与「买入门槛」默认联动，改一处两处都动）。
	//   ★ 判定符号是**严格大于**（用户八期口径「score_threshold>2」），
	//     与买入那边的 `>=` 不同 —— 见 service.decideAddon 的说明。
	ScoreThreshold int `json:"score_threshold"`

	// DropPct 【七期复活】最新已收盘 K 线的收盘价比**买入价**低超过这个百分比（%）
	//   才允许加仓 —— 「跌到位置」。默认 1.0。
	//   判定（service.decideAddon）：sig.Close < p.EntryPx × (1 - DropPct/100)。
	//   ≤ 0 会被 fillDefaults 反压回默认 1.0（不支持关闭；要关就 addon.enabled=false）。
	DropPct float64 `json:"drop_pct"`

	// BarRisePct 【共振模式】该根涨幅必须**严格大于**这个百分比（%）。
	//   判定用 Signal.RisePct（同一根、同一个数，买入扫描与加仓不会各算一遍）。
	//   ≤ 0 会被 fillDefaults 反压回默认 1.0。
	BarRisePct float64 `json:"bar_rise_pct"`

	// PriceRisePct 【价格模式】该根涨幅必须**严格大于**这个百分比（%）。
	//   ★ 为什么价格模式的涨幅要用独立键（八期）：
	//     两套模式的「涨幅」语义不完全一样（共振模式配合分数用，价格模式配合跌幅用），
	//     共用 bar_rise_pct 的话，切模式时前一套的值会污染后一套 —— 用户切来切去
	//     就总得重填。独立键 = 两套参数各自 remember。
	//   ≤ 0 时退回 BarRisePct（老配置只写了 bar_rise_pct 也能照常工作）。
	PriceRisePct float64 `json:"price_rise_pct"`

	// Ratio 加仓额 = 原持仓保证金 × Ratio。默认 1/3。仅当 MarginUSDT <= 0 时生效。
	Ratio float64 `json:"ratio"`

	// RiseBar 用哪个周期判断。默认 AddonAutoBar（"auto"）=
	//   用「该仓位自己的周期」（1m 开的按 1m 判、15m 开的按 15m 判）；
	//   老仓（库 bar 列为空）退回配置里的主周期 cfg.Bar。
	RiseBar string `json:"rise_bar"`

	// LookbackBars 【已废弃】旧口径「回看多少根找先跌的低点」；
	//   新口径只看「最新已收盘那根 vs 买入价」，不再回看窗口。
	LookbackBars int `json:"lookback_bars"`

	// MaxTimes 每个仓位最多加几次。**<= 0 = 不限**（默认 0）。
	//
	// ★ 注意：这只限制「还能不能继续补仓」，**不是**出场条件。
	//   2026-10-01 起「加满就自动平仓」那条规则已删除（用户要求取消），
	//   runAddons 的第二个返回值恒为空。
	//   ★ 判定处必须带 `MaxTimes > 0` 前置，否则 0 会被当成「已达上限 0」第一笔就拦掉。
	MaxTimes int `json:"max_times"`

	// MinGapBars 两次加仓之间至少隔多少根 RiseBar。默认 1。
	MinGapBars int `json:"min_gap_bars"`

	// MarginUSDT 固定加仓额（U）。>0 时优先于 Ratio ——
	//   八期管理台允许直接输入加仓金额，走的就是这个字段。0 = 用 Ratio 比例。
	MarginUSDT float64 `json:"margin_usdt"`

	// OnlyWhenPriceUp 【已废弃】旧口径「只在上行时加」，新逻辑不读。
	OnlyWhenPriceUp bool `json:"only_when_price_up"`

	// FibRatio 旧的斐波那契口径，已废弃，仅保留读兼容
	FibRatio float64 `json:"fib_ratio,omitempty"`
}

type RiskCfg struct {
	AccountEquityStop    float64 `json:"account_equity_stop"`
	DailyLossStopPct     float64 `json:"daily_loss_stop_pct"`
	MaxTotalMarginPct    float64 `json:"max_total_margin_pct"`
	MinAvailableUSDT     float64 `json:"min_available_usdt"`
	ConsecutiveLossPause int     `json:"consecutive_loss_pause"`
	PauseOnAPIError      int     `json:"pause_on_api_error"`
}

type AICfg struct {
	Enabled        bool   `json:"enabled"`
	Provider       string `json:"provider"`
	BaseURL        string `json:"base_url"`
	APIKey         string `json:"api_key"`
	Model          string `json:"model"`
	TimeoutSec     int    `json:"timeout_sec"`
	MaxCallsPerDay int    `json:"max_calls_per_day"`
	OnlyOnOrder    bool   `json:"only_on_order"`
}

type OKXCfg struct {
	BaseURL      string   `json:"base_url"`
	FallbackURLs []string `json:"fallback_urls"`
	APIKey       string   `json:"api_key"`
	SecretKey    string   `json:"secret_key"`
	Passphrase   string   `json:"passphrase"`
	Simulated    bool     `json:"simulated"`
	Proxy        string   `json:"proxy"`
}

// StoreCfg 持久化配置。
//
// 数据库已从 SQLite 切换为 MySQL（400+ 合约并发写入，SQLite 单写者扛不住）。
// 旧字段 DBPath / Python 仅作兼容保留，新代码不再使用。
type StoreCfg struct {
	Enabled bool   `json:"enabled"`
	Driver  string `json:"driver"` // 固定 mysql

	Host string `json:"host"`
	Port int    `json:"port"`
	User string `json:"user"`

	// Password 可以不写 —— 留空（或整段删掉）时会自动落到
	// 环境变量 OKX_MYSQL_PASS → 密钥文件 <项目根>\.mysql-pass。
	//
	// 建议**就留空**：本文件虽然已在 .gitignore 里，但"配置文件里明文写口令"
	// 这个习惯一旦扩散（比如被谁拷进 example.json），泄漏只是时间问题。
	// 详见 internal/conf/secret.go。
	Password string `json:"password"`

	Database string `json:"database"`
	DSN      string `json:"dsn"` // 若填写则优先于上面 5 个字段

	MaxOpenConns int `json:"max_open_conns"`
	MaxIdleConns int `json:"max_idle_conns"`
	BatchSize    int `json:"batch_size"` // 批量写入分片大小

	// KeepKlineDays 旧字段（K 线保留天数），已被 KlineRetainDays 取代。
	//
	// 只在老配置里出现时做兜底：KlineRetainDays 没写就沿用它。
	// 用「天」而不是「根数」：1m 一天 1440 根、4H 一天 6 根，
	// 同一个根数对两个周期是完全不同的时间跨度。
	KeepKlineDays int `json:"keep_kline_days"`

	// KeepKlineBars 旧字段（按根数），已废弃，只在老配置里出现时做换算兜底。
	KeepKlineBars int `json:"keep_kline_bars,omitempty"`

	// KlineRetainDays **K 线**保留窗口（天）。
	//
	// ★ 2026-10-02 十九期：**当前口径 30 天** ★
	//   二期曾是 10 天，本次放宽到 30（用户要「全市场 5m 30 天数据入库」）。
	//
	// ⚠ 本字段是「多副本口径」，改动时必须四处同步，否则会出现
	// 「文件里写了 30、兜底却是别的值」这种静默不一致：
	//   ① configs/okx_strategy.json          ② configs/okx_strategy.example.json
	//   ③ defaultConfig()（本函数）            ④ 旧键 keep_kline_days 保持 0
	//
	// ⚠ 改本值还有两个**非配置文件**的连带项（本项目真实踩过）：
	//   · 分区：kline 按天分区只铺「now-HotDays → now+Ahead」，往前不自动补。
	//     保留期变大而分区没扩 → 旧数据全挤进第一个日分区，
	//     将来那个分区整段 DROP 时会连带删掉还没到期的数据。需手动 REORGANIZE。
	//   · 回补：resolveBackfillDays 跟随本值，但只在进程启动时读一次 → 必须重启服务。
	//
	// 与 RetainDays（记录表 30 天）**分开**：权益曲线每 3 秒一条、
	// 一年 1000 万行，没必要跟 K 线一个窗口。
	KlineRetainDays int `json:"kline_retain_days"`

	// RetainDays **记录表**保留窗口（天），默认 30。
	//
	// 覆盖：历史仓位(trade) / 交易记录(trade_event) / 交易信号(signals) /
	//       权益曲线(equity) / 运行日志表(runlog) / AI 调用(ai_call)
	//
	// 注意：这些表的清理**只跟年度任务走**（用户口径：「一年才运行一次
	// 清除任务就行」），30 天只是「删到哪一代」的红线，不是「多久删一次」。
	RetainDays int `json:"retain_days"`

	// LogRetainDays 日志文件保留窗口（天），默认 30。
	//
	// 用户口径：「每个月要清除所有超过一个月的日志记录，包括数据库、
	// 客户端、网页等日志」。覆盖 logs/*.log（应用 + MySQL error/slow）
	// 与 apache/logs/*.log —— 两处都会扫。
	LogRetainDays int `json:"log_retain_days"`

	// ArchiveDir 月度归档目录（相对项目根）。K 线按月导出成 gzip 分片放这里，
	// 再作为一个**独立 git 仓库**推到 GitHub 数据仓。
	ArchiveDir string `json:"archive_dir"`

	// ArchiveMinFreeGB 磁盘守卫阈值（GB），默认 10。
	//
	// 用户口径：「每个月月底 C 盘剩余总量小于 10G 余额就删除掉多余的
	// 之前几个月的数据，只保留当月数据就行」。
	ArchiveMinFreeGB int `json:"archive_min_free_gb"`

	// DisableRecycleClean 关掉回收站清理。
	//
	// 默认 false（即**开启**，符合用户口径「清理回收站的垃圾文件，
	// 自动运行」）。之所以给一个反向开关：清空回收站是**不可逆**的，
	// 万一里面还躺着用户想恢复的东西，能立刻停下来。
	// 设为 true 后月度任务会跳过回收站这一步并打日志说明。
	//
	// 实现说明：okxweb 以服务身份跑在 Session 0 / LocalSystem，
	// SHEmptyRecycleBin 清的是 SYSTEM 自己的回收站、清不到用户那份，
	// 所以实际是直接删 C:\$Recycle.Bin\<SID>\ 下的文件（见 recycle_windows.go）。
	DisableRecycleClean bool `json:"disable_recycle_clean"`

	LogDir   string `json:"log_dir"`
	LogMaxMB int    `json:"log_max_mb"`
	LogKeep  int    `json:"log_keep"`

	// 兼容旧配置（不再使用）
	DBPath string `json:"db_path,omitempty"`
	Python string `json:"python,omitempty"`
}

// NQSignalCfg 只读板块（NQ / 纳斯达克100）**单独一套**的买入信号口径。
//
// ★ 2026-10-02 十三期新增（用户口径：「买入信号共振给我 NQ 单独算，
//   只算共振 4+ 下跌情况买入」）。
//
// 为什么必须单开一块 —— 指数和加密货币的波动尺度差一个量级，实测证据
// （Dukascopy 真实数据，2026-09-03 一整天）：
//
//	3m  300 根：|涨跌幅| 最大 0.275%，跌破 -0.7% 的 **0 根**
//	5m  288 根：最大 0.258%，**0 根**
//	15m  96 根：最大 0.343%，**0 根**
//
// 若沿用全市场的 entry.min_bar_rise_pct = -0.7，NQ 会**永远没有买入信号**
// —— 不是"少"，是恒等于 0。这种「配置写了却一辈子不触发」是最难被发现的一类
// 失效：页面上不报错、日志里没异常，只是永远什么都没有。
//
// 字段语义（**注意与全局 min_bar_rise_pct 的带符号三态不同**）：
//
//	Enabled        nil/缺键 = 启用（与项目里其它开关一致：不写就是开）
//	               false   = 显式关闭，NQ 退回全市场通用口径
//	ScoreThreshold 共振门槛（Score >= 它）。全局 score_threshold 是 3；
//	               NQ 用 4（用户口径「共振 4+」，比全市场更严）。
//	               写成 <= 0 时归一化会补成默认 4 —— 这里 0 没有合理含义
//	               （「共振 0 个以上」比全局还松，不可能是用户想要的），
//	               所以补默认是安全的，不会造成"静默失效"。
//	MaxRisePct     触发那根 K 线的涨跌幅必须**严格小于**它（%）：
//	               0    = 只要收阴（RisePct < 0）—— 默认值，就是用户说的"下跌情况"
//	               -0.1 = 必须跌超 0.1%
//	               100  = 等效关闭（写个大正数即可，不需要再加开关）
//
// ⚠ 为什么不复用全局那套「带符号三态」：那个语义里 0 被占用成"关闭这个条件"，
//   而这里要表达的恰恰是「跌任意幅度」—— 语义冲突。硬凑只能写成 -0.0001 这种
//   魔法值，下一个人看到只会以为是笔误。所以这里 0 = 只要收阴，显式定义。
type NQSignalCfg struct {
	Enabled        *bool   `json:"enabled"`
	ScoreThreshold int     `json:"score_threshold"`
	MaxRisePct     float64 `json:"max_rise_pct"`
}

// NQ 专属口径的兜底值（与 defaultConfig() / configs/okx_strategy.json 同口径）。
//
// 定义成常量而不是各处再写一遍字面量：这块的三个值散落在「真源 JSON /
// 示例 JSON / defaultConfig / NQSignalRule 兜底」四处，任何一处漏改都会造成
// 「配置读不到时跑的是另一套口径」——而 NQ 的失效形态是"一条信号都没有"，
// 不报错，最难查。
const (
	// DefaultNQSignalScoreThreshold NQ 共振门槛兜底（用户口径「共振 4+」）
	DefaultNQSignalScoreThreshold = 4
	// DefaultNQSignalMaxRisePct NQ 涨跌幅门槛兜底（0 = 只要收阴）
	DefaultNQSignalMaxRisePct = 0.0
)

// IsEnabled 未配置 / 未写 enabled 都视为启用，只有显式 false 才关闭。
func (n *NQSignalCfg) IsEnabled() bool {
	return n == nil || n.Enabled == nil || *n.Enabled
}

// NQSignalRule 取 NQ 专属买入口径的**生效值**（调用方直接用这个，不要去解指针）。
//
//	ok=false → 显式关闭，调用方退回全市场通用口径
//	ok=true  → scoreThreshold / maxRisePct 生效
//
// ★ 整块缺失时返回**默认值且启用**，不是"关闭" ★
//
// 这是刻意的方向选择：缺配置时最坏的结果是「和默认口径一样」，
// 绝不能是「彻底不出信号」。后者正是这块配置存在的意义（全局 -0.7% 在
// 指数上恒不触发），如果因为漏配置就退回去，等于把老问题原样复活。
func (c *Config) NQSignalRule() (scoreThreshold int, maxRisePct float64, ok bool) {
	if c != nil && c.NQSignal != nil {
		if !c.NQSignal.IsEnabled() {
			return 0, 0, false
		}
		th := c.NQSignal.ScoreThreshold
		if th <= 0 {
			th = DefaultNQSignalScoreThreshold
		}
		if th > 8 {
			th = 8
		}
		return th, c.NQSignal.MaxRisePct, true
	}
	return DefaultNQSignalScoreThreshold, DefaultNQSignalMaxRisePct, true
}

type Config struct {
	Enabled     bool     `json:"enabled"`
	DryRun      bool     `json:"dry_run"`
	OrderVia    string   `json:"order_via"`
	Bar         string   `json:"bar"`
	BarsEnabled []string `json:"bars_enabled"`
	// SignalBars 信号回算/图上展示的周期。
	// 与 BarsEnabled（真正执行「扫描 + 开仓」的周期）分开：
	// 回算便宜、多多益善，交易昂贵、只认主周期。空 = 全部 6 个周期。
	SignalBars        []string       `json:"signal_bars"`
	MinCandles        int            `json:"min_candles"`
	TopNByVolume      int            `json:"top_n_by_volume"`
	MinQuoteVolume24h float64        `json:"min_quote_volume_24h"`
	ExcludeInst       []string       `json:"exclude_inst"`
	Workers           int            `json:"workers"`
	CandleLimit       int            `json:"candle_limit"`
	HistoryPages      int            `json:"history_pages"`
	ScoreThreshold    int            `json:"score_threshold"`
	ScoreThresholdMap map[string]int `json:"score_threshold_map"`
	// NQSignal 只读板块（NQ / 纳斯达克100）单独的买入信号口径，见 NQSignalCfg。
	NQSignal          *NQSignalCfg `json:"nq_signal"`
	SignalTimeoutSec  int          `json:"signal_timeout_sec"`
	RequestTimeoutSec int          `json:"request_timeout_sec"`

	// ---- 合约准入（「哪些能买」）----
	// ExcludeStockETF 不买美股 / ETF / 商品，只做加密（OKX instCategory=1）。
	// ★ 2026-10-02 三期默认 false（用户「取消美股 etf 不做的功能」）——
	//   现在只看「最小一手保证金 ≤ max_order_margin_usdt」。
	ExcludeStockETF bool `json:"exclude_stock_etf"`
	// ExcludeNewListingDays 上市不足这么多天的不买（0 = 不排除）
	ExcludeNewListingDays int `json:"exclude_new_listing_days"`
	// ExcludeDelisting 排除 OKX 公告里说要下线的
	ExcludeDelisting bool `json:"exclude_delisting"`
	// MaxOrderMarginUSDT 单笔保证金硬上限（防止算错了买多）
	MaxOrderMarginUSDT float64 `json:"max_order_margin_usdt"`

	Entry *EntryCfg `json:"entry"`
	Exit  *ExitCfg  `json:"exit"`
	Addon *AddonCfg `json:"addon"`
	Risk  *RiskCfg  `json:"risk"`
	AI    *AICfg    `json:"ai"`
	OKX   *OKXCfg   `json:"okx"`
	Store *StoreCfg `json:"store"`

	// 运行时字段（不来自 JSON）
	path string // 配置文件绝对/相对路径
	dir  string // 配置文件所在目录（db.py 就从这里找）
	root string // 项目根目录（有 go.mod 的那层），相对路径都按它解析
}

// Resolve 把配置里的相对路径解析成绝对路径。
// 这样无论从哪个目录启动（项目根 / internal/service / 计划任务的任意工作目录），
// 数据库和日志都落在同一个地方。
func (c *Config) Resolve(p string) string {
	if p == "" {
		return p
	}
	if filepath.IsAbs(p) {
		return p
	}
	root := c.root
	if root == "" {
		root = "."
	}
	return filepath.Join(root, p)
}

// Dir 配置文件所在目录（外部包要用时走这个方法，别去碰私有字段）
func (c *Config) Dir() string { return c.dir }

// Root 项目根目录（有 go.mod 的那层）
func (c *Config) Root() string { return c.root }

// ---------------------------------------------------------------------------
// logx.Sink 实现 —— 让 logx 拿到「日志往哪写」而不用反过来依赖 conf
// ---------------------------------------------------------------------------

// LogEnabled 对应 store.enabled
func (c *Config) LogEnabled() bool { return c.Store != nil && c.Store.Enabled }

// LogDirResolved 日志目录（已解析成绝对路径）
func (c *Config) LogDirResolved() string {
	if c.Store == nil {
		return ""
	}
	return c.Resolve(c.Store.LogDir)
}

// LogMaxMBytes 单文件体积上限（MB）
func (c *Config) LogMaxMBytes() int {
	if c.Store == nil {
		return 0
	}
	return c.Store.LogMaxMB
}

// LogKeepFiles 滚动保留份数
func (c *Config) LogKeepFiles() int {
	if c.Store == nil {
		return 0
	}
	return c.Store.LogKeep
}

// projectRoot 从当前目录往上找 go.mod；找不到就退回当前目录
func projectRoot() string {
	// 1) 当前工作目录往上找 go.mod —— 命令行 / 批处理启动时走这条
	if dir, err := os.Getwd(); err == nil {
		if r := walkUpToRoot(dir, 5); r != "" {
			return r
		}
	}
	// 2) 可执行文件目录往上找 —— Windows 服务启动时 cwd 是 C:\Windows\System32，
	//    只能靠 exe 自己的位置反推（bin\okxweb.exe → ..\ = 项目根）。
	if exe, err := os.Executable(); err == nil {
		if r := walkUpToRoot(filepath.Dir(exe), 5); r != "" {
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

// ---------------------------------------------------------------------------
// 默认值（文件缺失或字段缺失时兜底）
// ---------------------------------------------------------------------------

// DefaultConfig 返回一份纯内置默认配置（不读磁盘、不受策略文件影响）。
// 单测和「配置缺失时手工构造」都走这个入口。
func DefaultConfig() *Config { return defaultConfig() }

func defaultConfig() *Config {
	// 口令不进源码：环境变量 OKX_MYSQL_PASS → 密钥文件 <根>\.mysql-pass → 空。
	// 细节见 secret.go。写死在这里的后果是「仓库一公开，口令就公开」。
	mysqlPass, _ := MySQLSecret()
	return &Config{
		Enabled:  true,
		DryRun:   true,
		OrderVia: "go",
		Bar:      "15m",
		// ★ 2026-10-01 二期：1m/3m/5m 重新上线（用户口径「选项卡重新生成并补充数据」）。
		//   与 model.EnabledBars 保持一致 —— 那份是全项目唯一权威，
		//   这里只是「配置块缺失」时的兜底。
		// ★ 2026-10-02 四期：1m 下线（用户「取消 1 分钟买入条件和买入信号和选项卡和 K 线图」）。
		//   与 model.EnabledBars 保持一致 —— 那份是全项目唯一权威。
		BarsEnabled:       []string{"3m", "5m", "15m"},
		SignalBars:        []string{"3m", "5m", "15m"},
		MinCandles:        400,
		TopNByVolume:      80,
		MinQuoteVolume24h: 1000000,
		ExcludeInst:       []string{},
		// 合约准入：不买美股/ETF/商品；不买 30 天内新上线；不买要下线的
		//
		// ★ 下面这些数字全是**兜底值**，只在 configs/okx_strategy.json
		//   缺失或解析失败时才会被用到。真正生效的口径一律来自那个 JSON
		//   （改完即刻生效，热插拔，不用重启服务、更不用改代码）。
		// ★ 2026-10-02 三期：默认关闭品类过滤（用户「取消美股 etf 不做的功能，
		//   只要买入上限小于 1U 就做」）。这是兜底值，与 universe.go 的
		//   DefaultUniversePolicy 必须一致 —— 两处都开着，任何一处漏改都会让
		//   「已取消的规则」在配置缺失时悄悄复活。
		ExcludeStockETF:       false,
		ExcludeNewListingDays: 30,
		ExcludeDelisting:      true,
		MaxOrderMarginUSDT:    1.0,
		Workers:               4,
		CandleLimit:           300,
		HistoryPages:          2,
		// ★ 2026-10-02 五期：阈值 4 → **3**（用户口径「Score >= 3 且 RisePct < -0.7」）。
		//   判定处本就是 sig.Score >= threshold，Score 是 0~8 的整数，
		//   所以写 3 就等于「≥ 3」，判定符号一个字都不用动。
		//   （三期曾写 4 来表达「> 3」，那是当时「8 个共振中 4 个及以上」的口径。）
		//   ⚠ 二期实测近 30 天 2329 条信号里 score 8 → 0 条，阈值 8 长期不出单；
		//     3 的把关交给下面的 min_bar_rise_pct（六期起：触发那根必须真跌 < -0.7%）。
		ScoreThreshold:    3,
		ScoreThresholdMap: map[string]int{},
		// ★ 2026-10-02 十三期：只读板块（NQ）**单独一套**买卖信号口径。
		//   兜底值必须与 configs/okx_strategy.json 同口径（4 / 0 = 共振≥4 且收阴），
		//   否则「JSON 读不到」时 NQ 会退回全局的 -0.7%，直接变成永不出信号。
		NQSignal: &NQSignalCfg{
			ScoreThreshold: DefaultNQSignalScoreThreshold,
			MaxRisePct:     DefaultNQSignalMaxRisePct,
		},
		SignalTimeoutSec:  900,
		RequestTimeoutSec: 20,
		Entry: &EntryCfg{
			TdMode: "isolated", PosSide: "net", OrdType: "market",
			MarginUSDT: 0.1, Leverage: 20,
			// ★ 2026-10-01：MaxConcurrentPositions / DailyMaxEntries 用 **0 = 不限**
			// （用户口径「取消限制」）。这两个的兜底值也刻意设成 0，
			// 免得「entry 块缺失 / 键名写错」时限制悄悄复活 —— 那正是用户这次反馈的现象。
			// 真正的兜底是账户可用余额与 risk.* 那几条，不是这里。
			// ★ 2026-10-02 十期：**冷却取消 + 改成持仓数量限制** ★
			//   用户口径：「冷却条件全部删除，改成数量 就是持仓数量，持仓数量要求 <30 就行」。
			//     · CooldownBars  30 → **0**（0 = 不冷却；归一化只反压负数，所以 0 是真生效的）
			//     · MaxConcurrentPositions 0 → **30**（最多同时持有 30 个合约）
			//   兜底值必须与 configs/okx_strategy.json 同口径 —— 否则配置读不到时
			//   会跑出「有冷却 / 不限持仓」这种和配置里写的完全不同的行为。
			MaxConcurrentPositions: 30, CooldownBars: 0, DailyMaxEntries: 0,
			// ★ 2026-10-02 三期：单笔口径 0.01U → **0.1U**（用户：「买入价格 0.1 美金就行，
			//   最高封顶 1 美金」）。0.1U × 20x = 2U 名义，比二期好买得多。
			//   min_one 口径不变：买得起就按 0.1U 成交，买不起就放大到「刚好 1 张」，
			//   硬顶 MaxMarginUSDT = 1U（与准入上限 max_order_margin_usdt 同值）。
			MarginPolicy: "min_one", MaxMarginUSDT: 1.0,
			// ★ 2026-10-02 三期新增 / 六期改语义：触发信号的那根 K 线的涨跌幅门槛
			//   （带符号，买入与加仓共用同一判据 service.SignalQualified）：
			//     > 0 → 必须真涨超过它（三期 1.0 / 四期 1.1 / 五期 0.5 的用法）
			//     < 0 → 必须真跌低于它（六期用户口径「RisePct < -0.7（严格小于）」）
			//   指针三态见 EntryCfg.MinBarRisePct 的注释。
			MinBarRisePct: f64ptr(-0.7),
		},
		// ★ 2026-10-02 七期：止盈 **0.35%**、止损 **300**（= -300%，物理上到不了，
		//   等效不设止损）、超时放宽到 **24 小时**（1440 分钟）。
		//   兜底默认值必须与 JSON 一致 —— 否则 JSON 读不到时布林上轨会静默复活
		//   （与三期 exclude_stock_etf 的兜底同一个道理）。
		Exit: &ExitCfg{TakeProfitPct: 0.35, BollUpperExit: false,
			MaxHoldBars: 0, MaxHoldMinutes: 1440, StopLossPct: 300},
		// 加仓（2026-10-02 七期，用户口径）：**不再与买入条件一致**，改为纯价格条件：
		//   最新已收盘 K 线的收盘价比买入价低超过 DropPct%（默认 1）——「跌到位置」
		//   且 这根 K 线自身涨幅超过 BarRisePct%（默认 1）——「反弹启动」
		// 金额 = 原持仓保证金 × 1/3。
		//
		// ★ 历史：二期曾改成「与买入一致（8 因子共振）」，七期按用户口径改回价格条件
		//   —— 注意这与二期下线的旧口径不同：旧口径是「15m 先跌后转涨」（相对**窗口低点**），
		//   新口径是「收盘价相对**买入价**低 1% + 当前 K 线涨 1%」。
		// ★ MaxTimes = 0 = **不限**（用户口径「加仓没有任何限制」）；
		//   RiseBar = "auto" = 用「该仓位自己的周期」（p.Bar），
		//   这样 1m 开的仓按 1m 判、15m 开的仓按 15m 判。
		// ★ 八期：Mode 必须显式给值。
		//   兜底值与 configs/okx_strategy.json **同口径**（当前 resonance）——
		//   否则「配置文件读不到」时跑的是另一套加仓判据，
		//   而现象只是「加仓条件和配的对不上」，最难查。
		//   Mode 留空会让 decideAddon 落进 default 分支，
		//   在那之前所有调用方就得先自己归一化一遍，属于隐性契约。
		// ★ 2026-10-02 十期：加仓改成**价格模式 -3% 上涨 0.3%** ★
		//   用户口径：「加仓条件改成 -3% 上涨 0.3%」。
		//     · Mode      resonance → **price**
		//     · DropPct   1.0 → **3.0**（收盘价比买入价低超 3% = 「跌到位置」）
		//     · PriceRisePct 1.0 → **0.3**（该根涨幅 > 0.3% = 「反弹启动」）
		//   兜底值必须与 configs/okx_strategy.json 同口径，理由同上。
		//   BarRisePct 是共振模式的参数（当前不参与判定），一并对齐成 0.7，
		//   免得哪天切回 resonance 时默认值和文件里写的不是一回事。
		Addon: &AddonCfg{
			Enabled: true, Mode: AddonModePrice,
			ScoreThreshold: 2, PriceRisePct: 0.3,
			Ratio: 1.0 / 3.0, DropPct: 3.0, BarRisePct: 0.7, RiseBar: AddonAutoBar,
			LookbackBars: 24, MaxTimes: 0, MinGapBars: 1,
			MarginUSDT: 0, OnlyWhenPriceUp: true,
		},
		Risk: &RiskCfg{
			// 小资金口径（账户就几毛到几 U）：
			//   百分比类的保护要按笔算，不能用「5U 可用余额」「30% 总保证金」这种大账户默认值。
			//   ★ 单笔 0.1U，可用余额门槛 0.1U。
			AccountEquityStop: 0, DailyLossStopPct: 50, MaxTotalMarginPct: 100,
			MinAvailableUSDT: 0.1, ConsecutiveLossPause: 5, PauseOnAPIError: 10,
		},
		AI: &AICfg{
			Enabled: true, Provider: "openai_compatible",
			BaseURL: "https://api.deepseek.com/v1", APIKey: "", Model: "deepseek-chat",
			TimeoutSec: 30, MaxCallsPerDay: 50, OnlyOnOrder: true,
		},
		OKX: &OKXCfg{
			BaseURL:      "https://www.okx.com",
			FallbackURLs: []string{"https://aws.okx.com", "https://okx.com"},
			Simulated:    true,
		},
		Store: &StoreCfg{
			Enabled: true, Driver: "mysql",
			Host: "127.0.0.1", Port: 3306,
			User: MySQLUser(), Password: mysqlPass, Database: DefaultMySQLDatabase,
			MaxOpenConns: 64, MaxIdleConns: 32, BatchSize: 500,
			// ★ 2026-10-02 十九期：K 线保留 **10 天 → 30 天** ★
			//   用户口径：「补充所有符合要求的合约的 5m 30 天数据，
			//   保存在数据库，然后计算出买入信号入库，网页打开 ETH 5m 直接显示」。
			//   二期（2026-10-01）曾是 10 天，本次放宽到 30。
			//   ⚠ 与 configs 里那两个键**必须同口径**，否则「文件没写、
			//   兜底生效」时会静默回到另一个值（本项目已踩过 4 次）。
			//   记录表仍是独立的 30 天红线，两者互不影响。
			//
			// ★ KeepKlineDays（废弃的老键）的兜底值保持 0 ★
			//   原来它是 30，而归一化的顺序是「先把 KeepKlineDays 兜成 30，
			//   再让 KlineRetainDays 去沿用 KeepKlineDays」——
			//   于是「两个键都没写」时拿到的是废弃字段的值，
			//   真实口径被一个废弃字段的默认值劫持。保持 0 之后，
			//   只有老配置文件里**显式写了** keep_kline_days 才会被沿用。
			KeepKlineDays: 0, KlineRetainDays: 30, RetainDays: 30, LogRetainDays: 30,
			ArchiveDir: "archive", ArchiveMinFreeGB: 10,
			LogDir: "logs", LogMaxMB: 20, LogKeep: 5,
		},
		path: "configs/okx_strategy.json",
		dir:  "configs",
	}
}

// ---------------------------------------------------------------------------
// 加载
// ---------------------------------------------------------------------------

var (
	cfgMu      sync.Mutex
	cfgCache   *Config
	cfgModTime time.Time
	cfgLoaded  bool
)

// LoadConfig 返回当前配置。文件被改动过会自动重新读。
// 永远不会返回 nil。
func LoadConfig() *Config {
	cfgMu.Lock()
	defer cfgMu.Unlock()

	p := resolveConfigPath()

	if cfgLoaded && cfgCache != nil && cfgCache.path == p {
		if fi, err := os.Stat(p); err != nil || !fi.ModTime().After(cfgModTime) {
			return cfgCache
		}
		// 有更新 → 往下走重新加载
	}

	cfg, mt, err := readConfig(p)
	if err != nil {
		if cfgCache != nil {
			// 读失败（比如改到一半保存了）→ 继续用上一份，别让策略中断
			logx.Logf("ERROR", "配置读取失败，继续沿用上一份配置：%v", err)
			cfgCache.path = p
			return cfgCache
		}
		logx.Logf("ERROR", "配置读取失败，改用内置默认值：%v", err)
		def := defaultConfig()
		def.path = p
		def.dir = filepath.Dir(p)
		def.root = projectRoot()
		logx.SetSink(def)
		cfgCache, cfgModTime, cfgLoaded = def, time.Time{}, true
		return def
	}

	cfg.path = p
	cfg.dir = filepath.Dir(p)
	cfg.root = projectRoot()
	logx.SetSink(cfg) // 必须先装好，下面 logf 才不会重入
	if cfgCache == nil && !cfgLoaded {
		logx.Logf("INFO", "策略配置已加载：%s（dry_run=%v simulated=%v bar=%s）",
			p, cfg.DryRun, cfg.OKX.Simulated, cfg.Bar)
	} else {
		logx.Logf("INFO", "策略配置已热加载：%s", p)
	}
	cfgCache, cfgModTime, cfgLoaded = cfg, mt, true
	return cfg
}

// ConfigPath 暴露当前配置文件路径（给外部日志用）
func ConfigPath() string {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return resolveConfigPath()
}

func resolveConfigPath() string {
	if p := strings.TrimSpace(os.Getenv("OKX_STRATEGY_CONFIG")); p != "" {
		return p
	}
	cands := []string{
		filepath.Join("configs", "okx_strategy.json"),
		"okx_strategy.json",
	}
	if r := projectRoot(); r != "." {
		cands = append(cands,
			filepath.Join(r, "configs", "okx_strategy.json"),
			filepath.Join(r, "okx_strategy.json"))
	}
	for _, c := range cands {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	// 相对可执行文件目录再找一遍（服务方式启动时 cwd 可能不同）
	// exe 通常在 <项目根>\bin\ 下，所以既要试 exe 同级的 configs\，
	// 也要试上一级（= 项目根）的 configs\。
	if exe, err := os.Executable(); err == nil {
		base := filepath.Dir(exe)
		for _, d := range []string{base, filepath.Dir(base)} {
			for _, c := range []string{
				filepath.Join(d, "configs", "okx_strategy.json"),
				filepath.Join(d, "okx_strategy.json"),
			} {
				if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
					return c
				}
			}
		}
	}
	return cands[0]
}

func readConfig(path string) (*Config, time.Time, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	clean := StripJSONComments(raw)
	cfg := defaultConfig()
	if err := json.Unmarshal(clean, cfg); err != nil {
		return nil, time.Time{}, fmt.Errorf("JSON 解析失败：%v", err)
	}
	fillDefaults(cfg)
	mt := time.Time{}
	if fi, err := os.Stat(path); err == nil {
		mt = fi.ModTime()
	}
	return cfg, mt, nil
}

// fillDefaults 兼容「文件里只写了几个键」的情况
func fillDefaults(c *Config) {
	d := defaultConfig()
	if c.Bar == "" {
		c.Bar = d.Bar
	}
	if len(c.BarsEnabled) == 0 {
		c.BarsEnabled = d.BarsEnabled
	}
	if len(c.SignalBars) == 0 {
		c.SignalBars = d.SignalBars
	}
	if c.MinCandles <= 0 {
		c.MinCandles = d.MinCandles
	}
	if c.TopNByVolume <= 0 {
		c.TopNByVolume = d.TopNByVolume
	}
	if c.MinQuoteVolume24h <= 0 {
		c.MinQuoteVolume24h = d.MinQuoteVolume24h
	}
	if c.Workers <= 0 {
		c.Workers = d.Workers
	}
	if c.Workers > 8 {
		c.Workers = 8
	}
	if c.CandleLimit <= 0 || c.CandleLimit > 300 {
		c.CandleLimit = d.CandleLimit
	}
	if c.HistoryPages < 0 {
		c.HistoryPages = 0
	}
	if c.ScoreThreshold <= 0 {
		c.ScoreThreshold = d.ScoreThreshold
	}
	if c.ScoreThreshold > 8 {
		c.ScoreThreshold = 8
	}
	if c.ScoreThresholdMap == nil {
		c.ScoreThresholdMap = map[string]int{}
	}
	// ★ 2026-10-02 十三期：只读板块（NQ）的信号口径
	//
	// 归一化只做一件事：把 ScoreThreshold <= 0 补成默认值。
	// 这里**允许**反压 0，与 max_concurrent_positions「0 = 不限」那条刚好相反 ——
	// 区别在于 0 在本题里没有合理语义：「共振 0 个以上」比全局门槛还松，
	// 不可能是用户想要的，所以补默认不会造成"用户写的值没生效"。
	// 反过来若不补，JSON 里漏写 score_threshold 就会让 NQ 悄悄退回全局的
	// -0.7% 门槛 —— 那正是本块存在的意义所在，不能让它自己被吞掉。
	if c.NQSignal == nil {
		c.NQSignal = d.NQSignal
	} else {
		n, dn := c.NQSignal, d.NQSignal
		if n.ScoreThreshold <= 0 {
			n.ScoreThreshold = dn.ScoreThreshold
		}
		if n.ScoreThreshold > 8 {
			n.ScoreThreshold = 8 // 6 个指标 + Pot/Fri/Kin 细分，最多 8 分
		}
	}
	if c.OrderVia == "" {
		c.OrderVia = d.OrderVia
	}
	if c.SignalTimeoutSec <= 0 {
		c.SignalTimeoutSec = d.SignalTimeoutSec
	}
	if c.RequestTimeoutSec <= 0 {
		c.RequestTimeoutSec = d.RequestTimeoutSec
	}
	if c.Entry == nil {
		c.Entry = d.Entry
	} else {
		e, de := c.Entry, d.Entry
		if e.TdMode == "" {
			e.TdMode = de.TdMode
		}
		if e.PosSide == "" {
			e.PosSide = de.PosSide
		}
		if e.OrdType == "" {
			e.OrdType = de.OrdType
		}
		if e.MarginUSDT <= 0 {
			e.MarginUSDT = de.MarginUSDT
		}
		if e.Leverage <= 0 {
			e.Leverage = de.Leverage
		}
		// ★ 2026-10-01：这两个字段的语义改成「<= 0 = 不限」★
		//
		// 用户口径：「持仓和当日买入数量太少了，取消限制」。
		//
		// 之前这里写的是「<= 0 → 兜底回默认值（8 / 30）」，后果是**配置文件里写 0 完全没用**：
		// 会被这几行反压回 8 / 30 —— 看起来改了，实际还在拦。所以只改 JSON 是不够的，
		// 归一化必须一起改，并且判定处要加 `> 0` 前置条件
		// （见 internal/service/trader.go 的开仓闸门）。
		//
		// 语义与同文件其它字段一致：`risk.max_total_margin_pct = 0` = 不启用、
		// `exit.max_hold_bars = 0` = 不启用、`exit.stop_loss_pct = 0` = 关闭。
		// 想恢复限制就在 configs/okx_strategy.json 里写正数，热加载即刻生效。
		if e.CooldownBars < 0 {
			e.CooldownBars = de.CooldownBars
		}
		if e.MarginPolicy == "" {
			e.MarginPolicy = de.MarginPolicy
		}
		if e.MaxMarginUSDT <= 0 {
			e.MaxMarginUSDT = de.MaxMarginUSDT
		}
	}

	// 负数一律归 0（这两个字段的 0 就是「不限」）。
	// 归一化成 0 而不是留着负数，是为了让 /api/state 和日志里显示的值干净
	// —— 交易逻辑两处都只看 `> 0`，负数与 0 等价，但显示 -3 会让人以为写错了。
	if c.Entry.MaxConcurrentPositions < 0 {
		c.Entry.MaxConcurrentPositions = 0
	}
	if c.Entry.DailyMaxEntries < 0 {
		c.Entry.DailyMaxEntries = 0
	}

	if c.Exit == nil {
		c.Exit = d.Exit
	} else if c.Exit.MaxHoldMinutes <= 0 && c.Exit.MaxHoldBars <= 0 {
		// 两个都没填 → 用默认的「1 小时超时」（四期口径）
		c.Exit.MaxHoldMinutes = d.Exit.MaxHoldMinutes
	}

	// 加仓次数归一化：**<= 0 = 不限**（2026-10-01 二期，用户口径「加仓没有任何限制」）。
	//
	// 这里原来写的是「<= 0 → 兜底回默认值 3」—— 那是同一个坑的第三次：
	// 配置里写 0 会被归一化反压回 3，看起来改了实际还在拦。
	// 语义与 entry.max_concurrent_positions / daily_max_entries 完全一致：
	// 0 或负数 = 不限，判定处（internal/service/addon.go）用 `> 0` 前置。
	if c.Addon != nil && c.Addon.MaxTimes < 0 {
		c.Addon.MaxTimes = 0
	}
	if c.Addon == nil {
		c.Addon = d.Addon
	} else {
		a, da := c.Addon, d.Addon
		// ★ 八期：Mode 决定用哪套判据。认不出的值一律回 resonance，
		//   绝不「猜一个」—— 走错分支等于加仓条件整体变味，而且不报错。
		switch strings.ToLower(strings.TrimSpace(a.Mode)) {
		case AddonModePrice:
			a.Mode = AddonModePrice
		default:
			a.Mode = AddonModeResonance
		}
		if a.Ratio <= 0 {
			a.Ratio = da.Ratio
		}
		// ★ 七期：DropPct 复活（收盘价比买入价低 N%）、新增 BarRisePct（该根涨 N%）。
		//   两者 ≤ 0 都反压回默认 1.0 —— 这两个条件就是价格模式下加仓的全部触发依据，
		//   静默关闭等于加仓彻底失去闸门，宁可回默认也不放空。
		if a.DropPct <= 0 {
			a.DropPct = da.DropPct
		}
		if a.BarRisePct <= 0 {
			a.BarRisePct = da.BarRisePct
		}
		// ★ 八期：价格模式的涨幅门槛独立成键。没写（0）时退回 BarRisePct，
		//   这样七期只配了 bar_rise_pct 的老配置照常工作。
		if a.PriceRisePct <= 0 {
			if a.BarRisePct > 0 {
				a.PriceRisePct = a.BarRisePct
			} else {
				a.PriceRisePct = da.PriceRisePct
			}
		}
		// ScoreThreshold 允许为 0（= 用顶层 ScoreThreshold 联动），负数无意义
		if a.ScoreThreshold < 0 {
			a.ScoreThreshold = 0
		}
		if a.RiseBar == "" {
			a.RiseBar = da.RiseBar
		}
		if a.LookbackBars <= 0 {
			a.LookbackBars = da.LookbackBars
		}
		if a.MaxTimes < 0 {
			a.MaxTimes = 0
		}
		if a.MinGapBars < 0 {
			a.MinGapBars = da.MinGapBars
		}
		if a.MarginUSDT < 0 {
			a.MarginUSDT = 0
		}
	}
	if c.Risk == nil {
		c.Risk = d.Risk
	}
	if c.AI == nil {
		c.AI = d.AI
	} else {
		a, da := c.AI, d.AI
		if a.BaseURL == "" {
			a.BaseURL = da.BaseURL
		}
		if a.Model == "" {
			a.Model = da.Model
		}
		if a.TimeoutSec <= 0 {
			a.TimeoutSec = da.TimeoutSec
		}
		if a.MaxCallsPerDay <= 0 {
			a.MaxCallsPerDay = da.MaxCallsPerDay
		}
	}
	if c.OKX == nil {
		c.OKX = d.OKX
	} else {
		o, do := c.OKX, d.OKX
		if o.BaseURL == "" {
			o.BaseURL = do.BaseURL
		}
		if len(o.FallbackURLs) == 0 {
			o.FallbackURLs = do.FallbackURLs
		}
	}
	if c.Store == nil {
		c.Store = d.Store
	} else {
		s, ds := c.Store, d.Store
		if s.Driver == "" {
			s.Driver = ds.Driver
		}
		if s.Host == "" {
			s.Host = ds.Host
		}
		if s.Port == 0 {
			s.Port = ds.Port
		}
		if s.User == "" {
			s.User = ds.User
		}
		if s.Password == "" {
			s.Password = ds.Password
		}
		if s.Database == "" {
			s.Database = ds.Database
		}
		if s.MaxOpenConns <= 0 {
			s.MaxOpenConns = ds.MaxOpenConns
		}
		if s.MaxIdleConns <= 0 {
			s.MaxIdleConns = ds.MaxIdleConns
		}
		if s.BatchSize <= 0 {
			s.BatchSize = ds.BatchSize
		}
		if s.KeepKlineDays <= 0 {
			// 老配置只写了 keep_kline_bars（根数）→ 按 1m 口径换算成天数兜底
			if s.KeepKlineBars > 0 {
				s.KeepKlineDays = s.KeepKlineBars/1440 + 1
			}
			if s.KeepKlineDays <= 0 {
				s.KeepKlineDays = ds.KeepKlineDays
			}
		}
		// KlineRetainDays 优先；没写就沿用老的 keep_kline_days，
		// 再没有才用默认值（2026-10-01 二期起是 10 天，原来是 365）。
		//
		// ⚠ 顺序陷阱：上面那几行会先把 KeepKlineDays 兜成默认值，
		//   所以它的默认值必须是 0 —— 否则「两个键都没写」时，
		//   KlineRetainDays 会沿用那个默认值，真实口径被废弃字段劫持。
		if s.KlineRetainDays <= 0 {
			if s.KeepKlineDays > 0 {
				s.KlineRetainDays = s.KeepKlineDays
			} else {
				s.KlineRetainDays = ds.KlineRetainDays
			}
		}
		if s.RetainDays <= 0 {
			// 留空 / 写 0 → 按 30 天兜底（记录表口径）
			s.RetainDays = ds.RetainDays
		}
		if s.LogRetainDays <= 0 {
			// 留空 / 写 0 → 按 30 天兜底（日志口径：「超过一个月的日志」）
			s.LogRetainDays = ds.LogRetainDays
		}
		if s.ArchiveDir == "" {
			s.ArchiveDir = ds.ArchiveDir
		}
		if s.ArchiveMinFreeGB <= 0 {
			s.ArchiveMinFreeGB = ds.ArchiveMinFreeGB
		}
		if s.LogDir == "" {
			s.LogDir = ds.LogDir
		}
		if s.LogMaxMB <= 0 {
			s.LogMaxMB = ds.LogMaxMB
		}
		if s.LogKeep <= 0 {
			s.LogKeep = ds.LogKeep
		}
	}
}

// ThresholdFor 取某合约的达标阈值
func (c *Config) ThresholdFor(instID string) int {
	if c.ScoreThresholdMap != nil {
		if v, ok := c.ScoreThresholdMap[instID]; ok && v > 0 {
			if v > 8 {
				v = 8
			}
			return v
		}
	}
	return c.ScoreThreshold
}

// BarEnabled 该周期是否允许扫描开仓
func (c *Config) BarEnabled(bar string) bool {
	for _, b := range c.BarsEnabled {
		if strings.EqualFold(strings.TrimSpace(b), bar) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

// StripJSONComments 去掉 JSON 里的 // 和 /* */ 注释（字符串内部不动）
//
// 注意：不能用正则 `//.*$` 去剥 —— 那会把 "https://api.deepseek.com" 这类
// 字符串里的 // 也当成注释起点，直接把 JSON 切坏。所以老老实实按字符扫，
// 记录「当前是否在字符串里」和「是否刚吃过转义符」。
func StripJSONComments(b []byte) []byte {
	out := make([]byte, 0, len(b))
	inStr := false
	esc := false
	for i := 0; i < len(b); i++ {
		ch := b[i]
		if inStr {
			out = append(out, ch)
			if esc {
				esc = false
				continue
			}
			if ch == '\\' {
				esc = true
				continue
			}
			if ch == '"' {
				inStr = false
			}
			continue
		}
		if ch == '"' {
			inStr = true
			out = append(out, ch)
			continue
		}
		if ch == '/' && i+1 < len(b) {
			if b[i+1] == '/' {
				for i < len(b) && b[i] != '\n' {
					i++
				}
				if i < len(b) {
					out = append(out, '\n')
				}
				continue
			}
			if b[i+1] == '*' {
				i += 2
				for i+1 < len(b) && !(b[i] == '*' && b[i+1] == '/') {
					i++
				}
				i++
				continue
			}
		}
		out = append(out, ch)
	}
	return out
}

// ---------------------------------------------------------------------------
// 派生口径
// ---------------------------------------------------------------------------

// DefaultMinBarRisePct 「触发那根 K 线涨跌幅门槛」的默认值（%，带符号）。
//
// ★ 2026-10-02 六期：0.5（必须真涨）→ **-0.7（必须真跌）**
//
//	（用户口径「Score >= 3 且 RisePct < -0.7（严格小于）」）。
//
// 这个常量同时被 conf 与 service 两侧读（service.StrategyConfig.MinBarRisePct
// 的兜底就用它），改一处两处都跟着变 —— 这正是它作为常量存在的意义。
const DefaultMinBarRisePct = -0.7

// f64ptr 取一个 float64 的指针（配置里的「三态」字段用）。
func f64ptr(v float64) *float64 { return &v }

// MinBarRisePct 触发信号的那根 K 线的带符号涨跌幅门槛（%）。
//
//	Entry.MinBarRisePct == nil → 默认 DefaultMinBarRisePct（当前 -0.7；键没写：条件仍然生效）
//	Entry.MinBarRisePct == 0   → 0（显式关闭：只看 score）
//	Entry.MinBarRisePct > 0    → 原值（RisePct 必须严格大于它 = 必须真涨）
//	Entry.MinBarRisePct < 0    → 原值（RisePct 必须严格小于它 = 必须真跌，六期新语义）
//
// 买入扫描与加仓判定都必须走这个方法，不要各自解指针
// ——「同一个量两条路算」是本项目反复踩的坑。
func (c *Config) MinBarRisePct() float64 {
	if c == nil || c.Entry == nil || c.Entry.MinBarRisePct == nil {
		return DefaultMinBarRisePct
	}
	// 六期起负数承载「必须真跌」，原样返回 —— 千万别再加「v < 0 → 回默认」的分支，
	// 那会把用户的 -0.7 静默吞掉（正是本项目的头号故障形态）。
	return *c.Entry.MinBarRisePct
}

// OrderMarginCap 单笔保证金硬上限（USDT）。
// 「不要买多，超过太多不好」—— 任何下单路径都不得突破这个数。
func (c *Config) OrderMarginCap() float64 {
	if c.MaxOrderMarginUSDT > 0 {
		return c.MaxOrderMarginUSDT
	}
	if c.Entry != nil && c.Entry.MaxMarginUSDT > 0 {
		return c.Entry.MaxMarginUSDT
	}
	return 0.5
}

// PerTradeMargin 单笔计划保证金（USDT），默认 0.1
func (c *Config) PerTradeMargin() float64 {
	if c.Entry != nil && c.Entry.MarginUSDT > 0 {
		return c.Entry.MarginUSDT
	}
	return 0.1
}

// PlanLeverage 计划杠杆
func (c *Config) PlanLeverage() int {
	if c.Entry != nil && c.Entry.Leverage > 0 {
		return c.Entry.Leverage
	}
	return 20
}
