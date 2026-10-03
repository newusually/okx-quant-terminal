# 二十一期（2026-10-03）：NQ 整体下线 · 15m 数据清理 · 3m 30 天回补 · 信号带共振数与跌幅

用户口径：「取消NQ所有东西 包括并且删除NQ按钮 数据等页面还有信号」「所有数字货币合约都给我删除掉15分钟K线图数据，并且补充3m数据到30天 包括信号」「把共振几个数字写在买入信号的下面」「标记下跌多少百分比 记录在数据库」「共振必须大于3」。

## 一、NQ 整体下线

| 层 | 动作 |
|---|---|
| 启动项 | `cmd/okxweb/main.go` 摘除 `StartNQSync`（Dukascopy）+ `StartNQIntraday`（Yahoo ^NDX 盘中）——不停同步删掉的数据会被写回来 |
| 前端 | 删 NQ 按钮（index.html）、`openNQ` 与监听（tables.js）、`READONLY_INSTS` 清空（core.js，合成行情/高亮分支自然失效） |
| 配置 | `nq_signal` 块从真源与 example 删除；守门测试 `TestRealConfig_NQSignalRule` 反转为「断言块不存在」 |
| 数据 | 停机窗口清库：kline 25,302 行、signals 945 条、signal_scan_state 3 条、backfill_job 3 条、inst 1 行，全部 NQ-INDEX 归零 |
| 代码保留 | `dukascopy.go` / `nq_intraday.go` 本体未删（死代码），想恢复时把两个启动项加回即可 |

## 二、15m 彻底下线

- `model.EnabledBars`：`["3m","5m","15m"]` → `["3m","5m"]`（全项目唯一权威，采集/回补/信号/前端选项卡自动跟随）
- 配置三处同步：`bars_enabled` / `signal_bars` / `bar: "5m"`（JSON + example + conf.defaultConfig + strategyconf 兜底）
- `addonBarFor` 空值兜底改白名单最后一位（5m）；admin.js 加仓周期下拉去掉 15m
- 存量清理（停机窗口）：kline **1,350,086 行**（485 合约）+ signals 4,240 条 + scan_state 284 条 + backfill_job 485 条
- `repo.CleanupKlines` 会按白名单自动整段删掉下线周期 → 以后不会再积累

## 三、3m 30 天回补（含信号）

- 重启后按合约逐个 POST `/api/backfill {bar:"3m"}` 入队 252 个可交易合约，days=30
- 信号回算：口径 5→4 变更 → **清空 signal_scan_state 作废全部水位线**（signals 行保留），回算自动重跑
- 实测：ETH 3m 14,493 根 / 覆盖 30.19 天；回补队列 490 done / 10 running（约 1 小时收敛）

## 四、信号改造：共振数 + 跌幅落库与展示

- `signals` 表加列 `rise_pct DOUBLE`（`migrate()` 幂等迁移），存量行按 `(c-o)/o*100` SQL 回填 36,398/36,399
- 写入链路：`EngineSignalRow.RisePct` → engine_store 插入列 → 回算（sigbackfill）与实盘（trader）两个写点都带上
- 下发链路：`SignalPoint.RisePct` → `SignalsInRange` → `/api/mark` marker 带 `risePct` + `text: "🚀<score> <±pct>%"`
- 前端：`paintMarkers` 密集时只剥成交长标签，**信号短标签（共振数+跌幅）保留**；悬浮框显示「共振 N/8 · 跌幅 -X.XX%」
- 新索引：`ix_sig_bar_ts (bar, ts)`（按周期跨合约统计）

## 五、买入门槛：共振 > 3

- 用户口径「共振必须大于3」= 严格大于 = Score ∈ {4..8}；判定符号不动（`score >= threshold`），**真源 `score_threshold: 4`**
- 沿革：三期 4（>3）→ 五期 3 → 后调 5 → 二十一期回到 4。JSON / example / conf 默认 / strategyconf 兜底 / admin.js 显示兜底 五处同口径
- 第一轮回算即产出 score=4 的信号（旧门槛 5 下不存在）→ 新门槛确认生效

## 六、验证证据

- `go build ./...` + `go vet` + `go test ./internal/...` 全绿（同步修正 4 个守门测试的口径漂移）
- CDP 无头实测：`nqEntry_exists:false`、周期选项卡仅 3m/5m、ETH 5m 图信号下方「🚀 6 -0.97%」「🚀 7 -0.58%」清晰可见（tmp/sig_zoom_*.png）
- 重启后日志无任何 [NQ] 行、无 15m 写入
- 提交 e591299

## 七、注意

- `mysql.exe` CLI 在沙箱里启动即挂（握手前），本次用项目自带 go-sql-driver 写了一次性执行器 `tmp/sqlrun21.go` 跑迁移；**服务自身连 127.0.0.1:3306 一直正常**——「连不上数据库」先分清是哪个客户端的问题
- 老仓位 trade.bar=15m 的，加仓判定自动退回 5m（与 1m 下线同一套兜底）
- 真源测试曾发现 `max_concurrent_positions` 80 vs 测试期望 30 的存量漂移，已按真源（80，用户管理台所改）修正测试
