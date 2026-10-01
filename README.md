# OKX 全合约量化终端

> 纯 Go 后端 + MySQL + Apache 的 OKX 永续合约自动交易与看板系统。
> 478 个 USDT-SWAP 全合约扫描，单笔口径（**1 USDT / 笔**），三层架构，无 Python、无 CGO。
> 数据只保留最近 **30 天**，超期由自动清理程序删除。

---

## 一、这是什么

一套跑在 Windows Server 上的 OKX 永续合约量化终端，三块能力：

| 能力 | 说明 |
|---|---|
| **行情与看板** | 478 个 USDT 永续全合约，**只保留 15m**，回补最近 30 天；网页端 AJAX + TradingView 风格 K 线（币安配色），下拉框选合约。**1m/3m/5m/1H/4H 已全部下线**（2026-10-01：库从 521MB 降到 132MB） |
| **合约准入** | 只买「该买」的：排除美股/ETF/商品、刚上线、即将下线、成交额过低、以及 1U 买不起的合约 |
| **交易策略** | 8 因子共振买入 → 出场三条：**止盈 +1%** / 布林上轨 / **超时 6 小时**；15m 先跌 0.5% 后转涨 → 加仓原仓位的 1/3，最多 3 次（**加仓次数不再是平仓条件**）（不设止损） |
| **自动交易** | 纯 Go 实时引擎：**3 秒**巡检止盈、**60 秒**扫全市场信号，命中自动买入、够线自动卖出，无需人工盯盘 |

## 二、快速开始

```bat
:: 0) 准备配置文件（模板里密钥是空的，从没提交过真实密钥）
copy configs\okx_strategy.example.json configs\okx_strategy.json

:: 填密钥：推荐用环境变量，密钥不落盘
set OKX_API_KEY=你的key
set OKX_SECRET_KEY=你的secret
set OKX_PASSPHRASE=你的passphrase

:: MySQL 口令：双击 scripts\set_db_pass.bat 生成一个随机口令即可
:: （源码 / 脚本里已经没有任何明文口令；解析链见下）

:: 一次性：注册 Windows 服务（需管理员）—— OKXMySQL / OKXApache / OKXWeb 全注册
scripts\install_services.bat

:: 编译
scripts\build.bat

:: 一键启动（平时不用跑：三个服务都是开机自启，重启机器自己就起来）
scripts\start_all.bat

:: 浏览器打开
http://localhost/
```

停止：`scripts\stop_all.bat`（`net stop OKXWeb` 等，无黑窗口）。

> **OKXWeb 是 Windows 服务**（Session 0，无 cmd 窗口），崩溃自动重启
> （5s / 5s / 30s），开机自动启动。改动代码后重新编译，
> 然后 `net stop OKXWeb && net start OKXWeb` 即可换版本。

## 三、架构总览

```
浏览器 ──:80──> Apache 2.4.68（Windows 服务 OKXApache）
                  │ mod_proxy_http + mod_deflate
                  ▼
             okxweb.exe（Go，只监听 127.0.0.1:8090）
                  │ handler / service / repo 三层
                  ▼
             MySQL 8.0.43（Windows 服务 OKXMySQL，3306）
```

三层严格单向依赖，`handler` 不写 SQL、`repo` 不做业务判断。

| 文档 | 内容 |
|---|---|
| [`docs/项目详情.md`](docs/项目详情.md) | **文件清单 / 表结构 / 接口 / 配置全量索引**（找「这个文件干嘛的」看这里） |
| [`docs/架构说明.md`](docs/架构说明.md) | 分层设计、依赖方向、准入规则、加仓口径、K 线分页、验收记录 |
| [`docs/策略说明.md`](docs/策略说明.md) | 交易链路与资金口径 |

## 四、目录速览

```
cmd/            okxweb（网页+引擎） okxdaemon（纯引擎） okxbench（压测） anncheck（公告诊断）
internal/
  handler/      接口层：HTTP ↔ 业务调用
  service/      业务层：准入 / 信号 / 交易 / 加仓 / 回补 / 公告
  repo/         数据层：MySQL 仓储 + OKX SDK
  model/        共享实体
  conf/ logx/   配置热加载 / 日志
configs/        策略配置（模板 okx_strategy.example.json；真正生效的 okx_strategy.json 不入库）
conf/           MySQL 配置 my.ini
web/assets/     前端（币安风格 K 线 + AJAX）
scripts/        一键脚本
docs/           文档（项目详情 / 架构说明 / 策略说明）
```

## 五、合约准入规则

按顺序执行，先命中先出局，结论写回 `inst.tradeable` / `inst.exclude_reason`。

| # | 规则 | 默认阈值 | 排除码 |
|---|---|---|---|
| 1 | 不买美股 / ETF / 商品 | `instCategory != 1` | `stock_etf` `commodity` `category` |
| 2 | 不买刚上线的 | 上市 < 30 天 | `new_listing` |
| 3 | 不买要下线的 | OKX 公告中心下线名单（24h 缓存） | `delisting` |
| 4 | 24h 成交额下限 | ≥ 100 万 USDT | `low_volume` |
| 5 | 最小一手买得起 | `minSz×ctVal×ctMult×价÷杠杆 ≤ 1.5U` | `notional` |

实测：**479 → 171 个可交易**（排除 186 美股/ETF、98 成交额不足、10 新上线、8 商品、5 买不起、1 状态异常）。
单笔口径上调到 1U 后准入上限同步放宽到 1U/张，可交易数量会随之增加。

## 六、资金口径（小资金专用）

```
每笔目标保证金   1 USDT            ← 2026-10-01 由 0.1U 上调
杠杆             20x
单笔硬上限       1.5 USDT（min_one：1U 买不起 1 张时放大到刚好 1 张）
准入上限         最小一手保证金 ≤ 1 USDT
加仓             原保证金 × 1/3，最多 3 次（★ 加满后不再平仓）
出场（三条）     止盈 +1% · 超时 6 小时 · 布林上轨（收盘价 > SMA20 + 2σ）
止损             关闭（合约内有爆仓风险，靠加仓摊薄均价）
数据保留         最近 30 天（DB 表 + 日志文件，超期自动删除）
```

## 七、技术要点

* **MySQL 高并发**：连接池 64、多行 `INSERT … ON DUPLICATE KEY UPDATE` 批量 500 行、
  `interpolateParams=true`、`innodb_flush_log_at_trx_commit=2`、`innodb_autoinc_lock_mode=2`。
  实测 **478 合约 × 6 周期 × 100 根 = 286,800 行 / 11.4~12.7 秒，22.5k~25.2k 行/秒，零失败**
  （`cmd/okxbench` 是独立压测工具，写的是 `kline_bench`，不随生产周期收敛）。
* **准入状态防污染**：`tradeable` / `exclude_reason` **不在** `UpsertInstruments` 的更新列里，
  只由准入过滤写，避免 10 分钟一轮的合约同步把结果冲回 0。
* **公告接口**：`/api/v5/support/announcements` 是**公开接口**，必须走不带鉴权头的请求路径
  （走签名路径会 401）。标题含 `postpone/delay/resume/cancel` 视为撤销更早的下线公告。
* **加仓纯函数**：`service.decideAddon()` 不碰网络，7 个场景有单测覆盖
  （`go test ./internal/service/ -run TestAddon -v`）。
* **配置热加载（热插拔）**：`configs/okx_strategy.json` 支持 `//` 与 `/* */` 注释，
  **改完保存即刻生效，不用重启服务**。这是所有交易口径的唯一真源 ——
  Go 里的数字只是「文件缺失/解析失败」的兜底，不构成要求。
  例：把 `max_order_margin_usdt` 从 `1.5` 改成 `0.4` 保存，约 2 秒后
  symbolList（可买入合约）当场从 176 个变成 167 个。
  实现见 `internal/service/strategy_store.go`，实测数据见架构说明 §二十。
* **改完怎么确认生效**：网页「服务信息」的**可买入上限** / 接口 `/api/state` 的
  `maxOrderMarginUsdt` / 日志 `[CFG] ★准入上限U 1.5 → 0.4`。

## 八、验收命令

```bash
go build ./...
go vet ./...
go test ./internal/service/ -run TestAddon -v

go run ./cmd/okxweb -init-only     # 建库建表 + 同步行情 + 准入过滤
go run ./cmd/okxbench -contracts 478 -bars 6 -rounds 2   # 并发压测
go run ./cmd/anncheck              # 公告接口连通性
```

## 九、注意事项

* **密钥永不入库**：`configs/okx_strategy.json`（含 OKX `api_key/secret_key/passphrase`）
  与 `api.json` **都已在 .gitignore 中排除**，请勿提交。
  仓库里只有脱敏模板 `configs/okx_strategy.example.json`。
  密钥优先从环境变量读：`OKX_API_KEY` / `OKX_SECRET_KEY` / `OKX_PASSPHRASE` / `OKX_FLAG`。
* `mysql/` `apache/` `runtime/` `bin/` `logs/` `tmp/` 均为本地产物，不入库。
* OKX 密钥只勾选 **读取 + 交易**，永远不要勾提现。
* 首次跑请保持 `configs/okx_strategy.json` 的 `dry_run: true`，只算信号不下单。

## 十、批处理脚本（scripts\*.bat）

| 文件 | 作用 | 权限 |
| --- | --- | --- |
| `scripts\install_services.bat` | 一次性安装：VC 运行库检查 → MySQL 初始化 → 注册 `OKXMySQL` / `OKXApache` 服务 | **需管理员** |
| `scripts\build.bat` | 编译出 `bin\okxweb.exe` | 普通 |
| `scripts\start_all.bat` | 一键启动 MySQL + okxweb(8090) + Apache(80)，并做连通性自检 | 普通 |
| `scripts\stop_all.bat` | 一键停止，顺序 Apache → okxweb → MySQL | 普通 |
| `scripts\cleanup_data.bat` | **月度维护**手工触发：`-maint-dry` 预演 → 询问 → `-maint` 真跑；`/yearly` 走年度清理 | 普通 |
| `scripts\rotate_logs.bat` | 单文件过大的日志轮转：停服务 → 搬 `logs\archive\` → 重启 → 清理 | 普通 |
| `scripts\set_db_pass.bat` | **MySQL 口令的唯一入口**：双击走菜单，或 `--gen` 生成 24 位随机口令并轮换、`--pass-file` 从文件读、`--file-only` 只写文件。先验证再落盘、失败自动回退，**不需要 root**，换完自动重启 OKXWeb。**默认只写 `.mysql-pass`，不碰环境变量**（`--env` 才写，不推荐） | 普通 |
| `scripts\set_token.bat` | **第一次用先跑这个**：双击 → 粘贴一次 PAT → 自动验证、存 `.git-token`、用 API 问出账号名写 `.git-owner`、数据仓不存在会自动建私有仓 | 普通 |
| `scripts\push_now.bat` | **一键上传**（双击版）：代码仓 + 归档数据仓，跑完 `pause` 住给人看结果 | 普通 |
| `scripts\push_github.bat` | **推送内核**（给服务/命令行用）：`check` / `code` / `data`；token 读 `%ROOT%\.git-token` 或 `%GITHUB_TOKEN%`，无 token 则干净退出不卡提示 | 普通 |

**编码约定（重要）**：`.bat` 一律保存为 **ANSI / GBK(cp936) + CRLF**，第二行 `chcp 936`。
文件名全 ASCII，中文只出现在 `echo` 文本里。

原因：cmd.exe 是按**字节偏移**重读批处理文件的。若存成 UTF-8 又在文件里 `chcp 65001`，
代码页切换会让偏移错位，中文字节会把后面的 ASCII 吃掉，典型症状是
`'lse' 不是内部或外部命令`（`else` 被啃掉）、`'026'`（`2026`）、`命令语法不正确`。
存 GBK 时控制台代码页不变，偏移恒定，中文还能正常显示。

`.gitattributes` 已写死 `*.bat text eol=crlf`，防止克隆后变回 LF。

**另外三条批处理硬约定**（全是实测踩出来的，不是理论）：

| 坑 | 症状 | 正确写法 |
| --- | --- | --- |
| `if (...)` / `for ... do (...)` **块内部**出现未转义的半角 `)` | `xxx was unexpected at this time`，**整段后续代码一起失效** | 块内只写中文全角 `（）`，或转义成 `^(` `^)`；最稳的是干脆不用块，改 `goto` 平铺 |
| `for /f` 的 `in('...')` 里给命令加引号 | `文件名、目录名或卷标语法不正确。`，且取到空值 | 绝对路径**不加引号**（`%SystemRoot%` 展开后不含空格） |
| 用管道接 `git push … \| findstr …` 再判 `errorlevel` | 拿到的是 `findstr` 的退出码 —— 推送失败被报成「[OK] 已推送」 | 先重定向到临时文件 → 立刻取 `errorlevel` → 再过滤打印 → 删文件 |

**git 不在系统 PATH**：这台机器唯一的 git 是 WorkBuddy 自带的 PortableGit
（`%USERPROFILE%\.workbuddy\binaries\PortableGit\versions\1.2.0`），没有进系统 PATH。
`push_github.bat` 会自己探测（PATH → 常见安装位置 → PortableGit）并把它塞进 PATH，
所以双击运行也能用。**注意：从 Git Bash 里调 `cmd` 会继承 bash 的 PATH，测不出这个问题** ——
必须用一个把 PATH 重置成 `C:\Windows\System32;C:\Windows` 的壳去测才复现得出来。

**脚本里为什么用 `%SystemRoot%\System32\findstr.exe` 的绝对路径**：装了 Git / Cygwin / MSYS
的机器，PATH 里会有同名的 GNU `find`、`timeout`，会把命令解析错（`find: '-/I': No such file`）。
等待用 `ping -n 9` 而非 `timeout`，因为 `timeout` 在 stdin 被重定向时会直接报错退出。


## 十一、Windows 服务化与自动交易（2026-09-30 新增）

### 1. 三个 Windows 服务，开机即用，无黑窗口

| 服务名 | 内容 | 自启 | 崩溃恢复 |
| --- | --- | --- | --- |
| `OKXMySQL` | MySQL 8.0.43 | 自动 | - |
| `OKXApache` | Apache 2.4.68（反代 :80 → 127.0.0.1:8090） | 自动 | - |
| `OKXWeb` | Go 网页 + 数据回补 + **自动交易引擎** | 自动 | 5s / 5s / 30s 重启 |

* `okxweb.exe -install` 注册、`-uninstall` 卸载；服务跑在 **Session 0**，没有 cmd 黑窗口。
* 服务的工作目录是 `System32`，程序用 `os.Executable()` 反推项目根，配置/K 线缓存都能找到。

### 2. 自动交易引擎（全自动，不用人盯）

* **止盈巡检**：每 3 秒读一遍持仓浮盈，≥ +1% 立刻市价平仓。
* **信号扫描**：每 60 秒全市场扫一遍（准入过滤后按成交额取前 80），score ≥ 6/8 自动买入。
* **加仓**：15m 先跌 0.5% 后转涨 → 补原保证金 1/3，**最多 3 次**。加满只是不再补仓，**不再平仓**（2026-10-01 取消「加仓次数」这条出场条件）。
* **超时**：开仓满 **6 小时（360 分钟）**未止盈 → 自动平仓。**不设止损**（`stop_loss_pct: 0`）。
* 所有口径都在 `configs/okx_strategy.json`（支持注释、热加载），网页「数据服务」面板实时显示。

### 3. 历史信号回算（K 线上的 🚀）

回补只入库 K 线，历史信号由 `[SIG-BF]` 后台协程逐根回算：
与实时扫描**同一套 `ComputeSignal`**（口径一致），score 够阈值的写入 `signals`
表（`INSERT IGNORE` 幂等 + **双向水位线**增量）。启动 20 秒后跑第一轮，之后每 30 秒增量一轮。
图上金色 🚀 = 买入信号/开仓，🍃 = 平仓卖出。

* **回算周期**：`signal_bars` 配置，当前 `["15m"]`（全库只保留 15m）
  （1m/3m 已**彻底下线**：数据库清空、前端选项卡移除、回补与信号回算全部不再触碰它们）
* **双向水位线**：K 线回补是「从最近往老补」的（区间向左扩张）。
  早期只记「已算到的最新 ts」当水位线，后补进来的老 K 线全被判成「算过了」跳过 ——
  信号永远追不上 K 线。现在 `signal_scan_state` 表记 `[min_ts, max_ts]` 闭区间，
  两头增量都补，重启接着跑不重算。

### 4. 下单参数自适应（重要）

`set-leverage` / `order` 的 `posSide` 参数按账户**实际持仓模式**自动翻译：

| 账户模式 | posSide | 说明 |
| --- | --- | --- |
| `net_mode`（单向） | `net` | 不传该参数 |
| `long_short_mode`（双向） | `long` | 必填，缺了 OKX 回 51000 |

启动时会探测一次并写日志：`OKX 账户持仓模式=xxx → 下单 posSide 采用 "xxx"`。
**自查命令**：`bin\okxweb.exe -probe ETH-USDT-SWAP` —— 探测持仓模式 + 试设杠杆（不下单），
确认下单参数合法。之前账户切到双向模式后 posSide=net 被 OKX 拒，
表现是「图上有信号、后台也在扫，但一条买入记录都没有」，这个开关就是防它的。

### 5. K 线回补：两遍走
（周期已收敛为 **只有 15m**；1m/3m 先于 2026-10-01 下线，5m/1H/4H 随后同日下架）

| 遍 | 内容 | 代价 |
| --- | --- | --- |
| 第一遍 Light | 每个 (合约,周期) 只拉最新 300 根 | 约 2900 次请求 / 几分钟，铺完就能出信号 |
| 第二遍 Full | 再逐个往前翻满 30 天 | 已铺够的走轻量路径直接跳过 |

周期只剩 `15m`："要用的周期先满" 这条排序逻辑现在只对单元素生效。
（1m 一个月 4.3 万根/合约、全部合约要二十多万次请求，排最后）

### 6. Apache 文件重定向

| 路径 | 指向 | 说明 |
| --- | --- | --- |
| `/files/` | 项目根 | 目录浏览（看代码/日志方便） |
| `/logs/` | `logs/` | 运行日志 |
| `/docs/` | `docs/` | 文档 |
| `/files/configs/` | **403** | 密钥目录已挡住 |

### 7. 前端实时性

* 顶栏权益/可用/本金/浮盈/总盈亏/胜率：`/api/account` 每 2 秒轮询，引擎每 3 秒写权益快照。
  没有成交时显示 `0 / 0.0%` 而不是 `--`（留白会让人以为界面坏了）。
* 持仓表 15 列含**强平价、距爆仓%**（<3% 红、<8% 金）。
* **权益曲线**：`/api/pnl?days=7` 只看最近一周；后端把原始点抽稀到 ≤1500 个再返回
  （一周原始快照约 20 万条，直接给前端会卡死）。鼠标移到曲线任意位置显示那一刻的
  权益 / 相对本金盈亏 / 浮盈 / 持仓数 / 可用。
* **历史仓位**：最近 300 笔；信号 / 持仓 / 历史三张表的**合约名都可点**，点击直切该合约 K 线图。
* K 线悬浮框：十字光标显示 时间/开/收/高/低/涨跌/振幅/量 + 当根的 🚀🍃 标注。
* 静态资源 `Cache-Control: no-cache`，改完刷新即生效，不会再用旧 JS。

### 8. 数据保留：三条红线 · 月度 / 年度自动维护（2026-10-01 改）

这台机器磁盘只有 60GB。而不同类型的行**增长速度差 3 个数量级**，所以不能共用一个窗口：

| 类别 | 代表对象 | 增速 | 保留 | 频率 |
|---|---|---|---|---|
| **记录表** | `trade`（**只删已平仓**，持仓中永远留）/ `trade_event` / `signals` / `equity` / `runlog` / `ai_call` / `pnl_point` | `equity` **每 3 秒 1 条**，留 1 年 = 1000 万行 | **30 天** | **月度** |
| **K 线** | `kline`（只有 15m） | 479 合约 × 96 根/天 ≈ 4.6 万行/天，1 年 ≈ 1700 万行 | **365 天** | **年度** |
| **日志文件** | `logs\*.log`、`logs\archive\**`、`logs\shots\**`、`apache\logs\*` | 几十 MB/天 | **30 天** | **月度** |

> 权益曲线留一年没意义（谁看三个月前某 3 秒的浮盈），但 **K 线必须留一年，因为要拿来做回测**。
> 窗口拆开是硬需求，不是设计偏好。

配置在 `configs/okx_strategy.json` 的 `store` 段：

```jsonc
"retain_days": 30,             // 记录表
"kline_retain_days": 365,      // K 线（年度任务）
"log_retain_days": 30,         // 日志文件
"archive_dir": "archive",
"archive_min_free_gb": 10,     // 磁盘守卫
"disable_recycle_clean": false // true = 不清回收站
```

**月度任务（每月自动跑一次，六步，顺序有讲究）**

```
① 归档所有「还没归档过」的、早于本月的月份
   → archive/kline-15m-YYYY-MM.partNN.csv.gz + manifest
② 推送 GitHub 数据仓      ← ★ 必须在 ⑤ 之前 ★
③ 记录表清理（30 天）
④ 日志清理（30 天）
⑤ 磁盘守卫：C 盘可用 < 10 GB → 只留当月
⑥ 清回收站
```

①**是补齐式而不是「只导上个月」**：⑤ 会把 K 线收缩到只留当月，若某个早于当月的月份
从来没归档过，那一刀下去就是永久损失（库里删了、远端也没有）。首次部署、停机几个月、
上次导出失败都会留下这种缺口月份。所以按库里实际存在的月份逐个检查，缺哪个月补哪个月，
判据是 `manifest-<YYYY-MM>.json` 是否存在（幂等，不会重复导出）。

② 必须在 ⑤ 之前：**先删后推**一旦推送失败（断网 / token 过期）就是「本地没了、远端也没有」，
数据永久丢失；**先推后删**最坏只是磁盘紧张一会儿。

② 失败时的门禁是**精确的**，不是「全停」：

| ⑤ 的动作 | 推送成功 | 推送失败 |
|---|---|---|
| 删库里早于收缩点的 K 线 | 执行 | 执行，但收缩点**回退**到第一个未归档月份 |
| 删本地 `archive/` 里早于当月的归档 | 执行 | **跳过** —— 本地是唯一副本 |

收缩点回退＝「没归档的月份，一行都不删」。正常情况下所有月份都已归档，
收缩点就是本月月初，与用户口径完全一致。

**年度任务**：只做一件事 —— `cut = now - 365 天` → 分区级删除。一年只跑一次。

**为什么 K 线要用 `DROP PARTITION`**：`kline` 是 `RANGE COLUMNS(ts)` 分区表
（`p_old` 兜底 + 冷区按月 + 热区按周 + `pmax`）。`ALTER TABLE kline DROP PARTITION` 是
**毫秒级删数据文件**，不产生 undo/binlog；而 `DELETE FROM kline WHERE ts < ?` 扫千万行
会把 60GB 磁盘写爆。选区规则**必须保守**：只有分区**上界 ≤ cutoff** 才整段 DROP
（上界 ≤ cutoff 才代表分区里每一行都过期）；`p_old` / `pmax` **永不 DROP**，
残余交给 `DELETE … LIMIT 5000` 兜底。

**调度怎么做**：不用 Windows 计划任务（本机 `schtasks` 被安全策略拦），
而是服务内 **90 秒首延迟 + 每 30 分钟 tick**，读 meta 表里的
`maint_last_monthly` / `maint_last_yearly` 和当前月/年比对，**不同才跑**。
所以**停机三天、重启二十次，本月任务也只跑一次，不重不漏**。

**归档分片**：按**压缩后字节数**滚动（40 MB/片），不押行数 —— GitHub 单文件上限 100 MB，
而同样 133 万行的压缩比能差 3 倍，押行数就是在赌。

**回收站**：不能用 `SHEmptyRecycleBin` —— 服务跑在 Session 0 / LocalSystem，
它清的是 SYSTEM 自己那份**空**回收站；回收站按用户隔离，实体在 `C:\$Recycle.Bin\<SID>\`。
所以直接遍历 SID 目录删文件 + 自下而上删空目录。**不可逆**，用
`disable_recycle_clean` 可关掉。

**手工跑**

```bat
bin\okxweb.exe -maint             :: 立刻跑一次月度维护
bin\okxweb.exe -maint-dry         :: 预演，只看不删
bin\okxweb.exe -maint-yearly      :: 立刻跑一次年度清理
bin\okxweb.exe -archive 2026-09   :: 只导出某月归档
```

或双击 `scripts\cleanup_data.bat`（月度维护）/ `scripts\rotate_logs.bat`（大日志轮转）。
推送归档用 `scripts\push_github.bat data`。

**推送怎么用**：第一次先双击 `scripts\set_token.bat` 粘一次 PAT（只需一次，以后不用再设）；
之后双击 `scripts\push_now.bat` 一键上传两个仓库。

**token 从哪来**：GitHub → `https://github.com/settings/tokens/new` →
Note 随便填、Expiration 选 `No expiration` → 勾最上面那个大框 **`repo`**（子项会自动全选）
→ 拉到底点 `Generate token` → 复制 `ghp_` 开头那一串（离开页面就再也看不到）。

`set_token.bat` 会用 API `/user` **先验证再落盘**，无效 token 不会写进文件。
这一步不能省：公开仓库匿名也能 `git ls-remote` 成功，**验不出 token 真假**，
必须走 API 看 HTTP 码（无效 token = 401）。验错就是「本地归档删了、远端也没上去」的数据事故。

`push_github.bat [check|code|data]` 是内核，给服务调用。token 读 `%ROOT%\.git-token`
或 `%GITHUB_TOKEN%`，都没有则干净退出（退出码 2）**绝不卡在交互式密码提示上** ——
服务里跑的命令一旦卡在提示符上就是永久挂起。推送用带 token 的临时 URL，
不写进 `.git/config`。退出码：`0` 成功 / `1` 推送失败 / `2` 无凭据 / `3` 无 git；
月度任务据此跳过删本地归档。

**实测**：2026-09 归档 1,336,768 行 / 479 合约 / 17.4 MB gzip / 27 秒；
月度预演 12.3 秒（报出 12 个待归档月份、回收站 7,647 文件 / 385.5 MB）；
年度清理 DROP 0 个分区（库里最老数据 2025-10，回补尚未满 1 年，符合预期）。

### 9. 历史仓位补录（2026-10-01 新增）

本地 `trade` 表原来只记**程序自己下的单**，实测 OKX 账户上有 100+ 个已平仓仓位、
8000+ 笔成交，本地却只有 8 行 —— 历史面板自然「没东西看」。

现在每 **10 分钟**拉一次 `/api/v5/account/positions-history`（最近 3 个月）写进 `trade`：

* 幂等三级匹配：`pos_id` 命中 → 更新；合约 + 开仓时间（±15 分钟）命中 → 认领
  （保留引擎自己写的 reason，如「止盈 +1.02%」）；都不命中 → 新增。
* 前端历史窗口同步从 3 天放宽到 **30 天**，和保留策略对齐。
* 同步完日志里会打一行：`[POSHIST] 仓位历史已同步：新增 x / 认领 y / 更新 z；
  本地共 N 个已平仓仓位…覆盖 起 ~ 止`。

### 10. 后台独立运行（不用开网页）

整套引擎都跑在 Windows 服务里，**网页只是查看窗口**，关掉浏览器什么都不断：

| 组件 | 载体 | 不开网页时 |
| --- | --- | --- |
| 止盈巡检（≥+1% 立刻平） | OKXWeb 服务内协程，每 3 秒 | 照跑 |
| 信号扫描 + 自动下单 | OKXWeb 服务内协程，每 60 秒 | 照跑 |
| K 线回补（Light/Full 两遍） | 同上 | 照跑 |
| 历史信号回算 | 同上，每 30 秒增量 | 照跑 |
| 网页界面 | 同进程里的 HTTP :8090 | 没人访问也无所谓 |

* 服务跑在 **Session 0**（`tasklist` 里会话名显示 `Services`），没有 cmd 黑窗口。
* 崩溃自动重启（5s / 5s / 30s），开机自启。
* 日志写 `logs/okxbot.log`，与有没有人开网页无关。
* 想让引擎完全停：`net stop OKXWeb`（或 `scripts\stop_all.bat`）；只停交易不停网页：`configs\okx_strategy.json` 里 `enabled: false`。

### 11. MySQL 口令不再进仓库（2026-10-01）

口令原先硬编码在 **4 个 `.go` 源文件 + 3 个批处理**里，而仓库是 public 的。
现在源码 / 脚本里**不含任何明文口令**，运行时按下面顺序解析：

| # | 来源 | 位置 |
| --- | --- | --- |
| 1 | 环境变量 | `OKX_MYSQL_PASS` / `OKX_MYSQL_USER` / `OKX_MYSQL_DSN`（**平时不该设**，见下） |
| 2 | 密钥文件 | `<项目根>\.mysql-pass`（一行口令，**已在 .gitignore**）← **唯一推荐来源** |
| 3 | 空 | 连不上时报错会直接告诉你去哪配 |

```bat
:: 设置 / 轮换（双击也行）——换完自动重启 OKXWeb，不需要 MySQL root
scripts\set_db_pass.bat --gen
```

**为什么环境变量优先、但服务实际读文件**：Windows 服务的环境变量是 SCM 在启动时
整份拷贝的，`setx /M` 之后已注册的服务往往要重启整机才看得到；`.mysql-pass` 没有这个坑。

> ⚠️ **别用 `setx` 把口令写进环境变量**（`set_db_pass.bat` 早期版本默认这么干，已改）：
> 不带 `/M` 的 `setx` 写的是**用户级**变量，明文落在 `HKCU\Environment`，
> 任何以本用户身份跑的进程 `printenv` 一下就能拿到 —— 等于把口令从 git 仓库
> 挪进注册表，白改。而且它对 OKXWeb **完全无效**（服务跑 LocalSystem，读不到用户级变量）。
> 现在**默认只写 `.mysql-pass`**；要写环境变量得显式加 `--env`。清理命令：
>
> ```powershell
> [Environment]::SetEnvironmentVariable('OKX_MYSQL_PASS',$null,'User')
> Remove-ItemProperty -Path 'HKCU:\Environment' -Name OKX_MYSQL_PASS -ErrorAction SilentlyContinue
> ```

> ⚠️ **改代码不等于堵住泄漏。** 历史提交里那把旧口令此前一直有效 ——
> 所以本轮做了**真实轮换**，旧口令现在返回 `Access denied`。
> 只改代码不换口令，是最容易交付、也最没用的那种「安全修复」。

自检：`bin\okxweb.exe -init-only` 的横幅会打 `口令来源 : 密钥文件 …`（**只报来源不报口令**）。

---

## 十二、性能诊断与第一轮优化（2026-10-01）

### 火焰图（新增，只绑回环）

```bat
bin\okxweb.exe -pprof-addr 127.0.0.1:8091     :: 默认就开；-pprof-addr "" 关掉
go tool pprof -http=:9999 "http://127.0.0.1:8091/debug/pprof/profile?seconds=30"
```

**独立端口 8091，且强制校验必须是回环地址** —— 8090 被 Apache 反代到公网 80，
pprof 挂上去等于把 goroutine 栈和堆快照送出门。传 `0.0.0.0:8091` 会直接拒绝启动。

### 这一轮修掉的四个真实缺陷

| 问题 | 修法 | 实测 |
|---|---|---|
| 实时扫描一轮 **62~65 秒**（每合约 2~3 次 OKX HTTP） | 改读本地 MySQL，落后才回退网络 | **25~27 s（约 2.4×）**，日志 `本地库 80 / 网络回退 0` |
| `ComputeSignal` 单次分配 **258 KB**（每秒 7.2 次 GC） | `sync.Pool` 复用指标缓冲 | **376 B（−99.85%）** |
| 逐根扫描是 **O(n²)** | `ComputeSeries()` 指标算一遍 + `At(idx)` O(1) | 440.6 ms → **1.215 ms（363×）** |
| `refreshLatest` 串行网络循环，最大 **753 秒** | 拆「筛到期任务」+「并发抓取」 | 见架构说明 §21.3（地板 = OKX 限频 20 次/2 秒） |

> ⚠️ **指标计算口径被逐位冻结**：`indicator_series.go` 是把原公式逐行照搬，
> 只改「写到哪里」。递归指标（ATR/RSI/EMA/TD9）依赖窗口起始位置，差一个元素
> 买卖点就会漂。`indicator_series_test.go` 用「9 种长度 × 全部下标逐字段比对」
> 加一个专门的「长→短池化污染」测试把这条钉死。

### 第二轮：把剩下两个大项也修掉（2026-10-01 夜）

| 问题 | 修法 | 实测 |
|---|---|---|
| `live.exitPass` 平均 **16.4 s** / 最大 34.7 s | 拆出「等锁」与「干活」两个打点 → **证伪了「锁竞争」假设**（等锁仅 3 ms）；真凶 `runExits.boll` 每个仓位 2 次 OKX HTTP 抢限频闸门 → 改读本地库 | 平均 **556.6 ms**（29×），逐轮中位 **227 ms**；`bollFromDB=570/570` 全部走本地 |
| 历史信号回填逐根滑窗（**O(根数 × 700)**） | `ComputeSeries` 整段算一次 | **255,528,000 → 1,819,600 ns/op（140×）**，内存 −96% |
| 回填每轮读**整个合约历史**（175 合约 ≈ 600 万行） | 按水位线分段读，最多 `sigReadBars=2000` 根 + 左侧缺口探针 | `signal.recalc` 逐轮中位 **183 ms**（原 28.7 s） |
| 整轮扫描 **25~27 秒** | 上面两条修完后只剩本地索引查询 | `[15m] … 用时` **→ 526ms / 581ms / 634ms / 644ms / 837ms** |

> ⚠️ **换实现必须先把差量出来，不能靠「我看着差不多」。** 滑窗法与整段法的递归指标
> 种子位置不同（`i-699` vs `0`），浮点会飘。实测 3 个主力合约 × 3000 根**真实** K 线、
> 8400 个下标：浮点最大相对偏差 `6.6e-04`（只出现在 `Fri`，与理论 `(95/96)^700` 吻合），
> 而 **mask / score / Td / Ready 不一致的根数 = 0**。
> 缩短读取窗口后又补了一次线上取证（8 个合约、全量 35060 根 vs 窗口 2000 根）：
> 尾部 40 个下标**离散字段零差异**。
>
> 最阴的一个坑：`addon.go` 的 `closedWindow` **直接读 `Candle.Confirm` 字段**。
> 把 `Confirm` 恒为 false 的转换函数喂给它，窗口恒空 → **加仓静默永久失效**，
> 不报错、不崩、日志干净。所以 `loadCandlesLocal`（带 `Confirm`）与
> `loadCandles`（不带）**故意并存**，`klines_confirm_test.go` 专门守这条。

### 关于「要不要改成 C++」

**不要。** 完整实测见 [`docs/性能评估-Go与C++对比.md`](docs/性能评估-Go与C++对比.md)。
一句话：纯计算只占实时扫描整轮墙钟的 **0.06%**（80 候选 × 196.5 µs ÷ 25.6 s；
按 `signal.recalc` 单次调用口径也只有 0.21%），单次网络往返（116~201 ms）是
单次信号计算（0.196 ms）的 **~700 倍**。换语言的上限收益 **不到 0.2%**，
而本机连 C++ 编译器都没有（`gcc/g++/cl/clang` 全缺，`CGO_ENABLED=0`）。

第二轮修复把两个大项的**分母**各砍了 1~2 个数量级（见上面那张表），
所以「计算占墙钟」这个百分比会变大 —— 但那是墙钟被砍的结果，不是计算变贵。
现在剩下的时间几乎全是 OKX 的网络往返与限频（`exit.acct` 平均 285 ms、
`feed.syncTickers` 平均 1061 ms）。**换语言对这部分收益是 0。**

**瓶颈是架构，不是语言。**

