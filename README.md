# OKX 全合约量化终端

> 纯 Go 后端 + MySQL + Apache 的 OKX 永续合约自动交易与看板系统。
> 478 个 USDT-SWAP 全合约扫描，小资金口径（**0.1 USDT / 笔**），三层架构，无 Python、无 CGO。

---

## 一、这是什么

一套跑在 Windows Server 上的 OKX 永续合约量化终端，三块能力：

| 能力 | 说明 |
|---|---|
| **行情与看板** | 478 个 USDT 永续全合约，1m/3m/5m/15m/1H/4H 六个周期，回补至少一个月；网页端 AJAX + TradingView 风格 K 线（币安配色），下拉框选合约 |
| **合约准入** | 只买「该买」的：排除美股/ETF/商品、刚上线、即将下线、成交额过低、以及 0.1U 买不起的合约 |
| **交易策略** | 8 因子共振买入 → **止盈 +1%** / 布林上轨平仓 / **超时 4 小时平仓**；15m 先跌 0.5% 后转涨 → 加仓原仓位的 1/3，**最多 3 次，加满信号再来直接平仓**（不设止损） |
| **自动交易** | 纯 Go 实时引擎：**3 秒**巡检止盈、**60 秒**扫全市场信号，命中自动买入、够线自动卖出，无需人工盯盘 |

## 二、快速开始

```bat
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
详细说明见 [`docs/架构说明.md`](docs/架构说明.md)，策略口径见 [`docs/策略说明.md`](docs/策略说明.md)。

## 四、目录速览

```
cmd/            okxweb（网页+引擎） okxdaemon（纯引擎） okxbench（压测） anncheck（公告诊断）
internal/
  handler/      接口层：HTTP ↔ 业务调用
  service/      业务层：准入 / 信号 / 交易 / 加仓 / 回补 / 公告
  repo/         数据层：MySQL 仓储 + OKX SDK
  model/        共享实体
  conf/ logx/   配置热加载 / 日志
configs/        策略配置 okx_strategy.json（改完自动热加载）
conf/           MySQL 配置 my.ini
web/assets/     前端（币安风格 K 线 + AJAX）
scripts/        一键脚本
docs/           文档
```

## 五、合约准入规则

按顺序执行，先命中先出局，结论写回 `inst.tradeable` / `inst.exclude_reason`。

| # | 规则 | 默认阈值 | 排除码 |
|---|---|---|---|
| 1 | 不买美股 / ETF / 商品 | `instCategory != 1` | `stock_etf` `commodity` `category` |
| 2 | 不买刚上线的 | 上市 < 30 天 | `new_listing` |
| 3 | 不买要下线的 | OKX 公告中心下线名单（24h 缓存） | `delisting` |
| 4 | 24h 成交额下限 | ≥ 100 万 USDT | `low_volume` |
| 5 | 最小一手买得起 | `minSz×ctVal×ctMult×价÷杠杆 ≤ 0.5U` | `notional` |

实测：**479 → 171 个可交易**（排除 186 美股/ETF、98 成交额不足、10 新上线、8 商品、5 买不起、1 状态异常）。

## 六、资金口径（小资金专用）

```
每笔目标保证金   0.1 USDT
杠杆             20x（逐仓）
单笔硬上限       0.5 USDT（min_one：0.1U 买不起 1 张时放大到刚好 1 张）
加仓             原保证金 × 1/3，最多 2 次
止盈             +2%（ROI +40%）
出场             布林上轨（收盘价 > SMA20 + 2σ）
止损             关闭（合约内有爆仓风险，靠加仓摊薄均价）
```

## 七、技术要点

* **MySQL 高并发**：连接池 64、多行 `INSERT … ON DUPLICATE KEY UPDATE` 批量 500 行、
  `interpolateParams=true`、`innodb_flush_log_at_trx_commit=2`、`innodb_autoinc_lock_mode=2`。
  实测 **478 合约 × 6 周期 × 100 根 = 286,800 行 / 11.4~12.7 秒，22.5k~25.2k 行/秒，零失败**。
* **准入状态防污染**：`tradeable` / `exclude_reason` **不在** `UpsertInstruments` 的更新列里，
  只由准入过滤写，避免 10 分钟一轮的合约同步把结果冲回 0。
* **公告接口**：`/api/v5/support/announcements` 是**公开接口**，必须走不带鉴权头的请求路径
  （走签名路径会 401）。标题含 `postpone/delay/resume/cancel` 视为撤销更早的下线公告。
* **加仓纯函数**：`service.decideAddon()` 不碰网络，7 个场景有单测覆盖
  （`go test ./internal/service/ -run TestAddon -v`）。
* **配置热加载**：`configs/okx_strategy.json` 支持 `//` 与 `/* */` 注释，改完保存即生效，不用重启。

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

* `api.json` 内含真实 API Key，**已在 .gitignore 中排除**，请勿提交。
* `mysql/` `apache/` `runtime/` `bin/` `logs/` 均为本地产物，不入库。
* OKX 密钥只勾选 **读取 + 交易**，永远不要勾提现。
* 首次跑请保持 `configs/okx_strategy.json` 的 `dry_run: true`，只算信号不下单。

## 十、批处理脚本（scripts\*.bat）

| 文件 | 作用 | 权限 |
| --- | --- | --- |
| `scripts\install_services.bat` | 一次性安装：VC 运行库检查 → MySQL 初始化 → 注册 `OKXMySQL` / `OKXApache` 服务 | **需管理员** |
| `scripts\build.bat` | 编译出 `bin\okxweb.exe` | 普通 |
| `scripts\start_all.bat` | 一键启动 MySQL + okxweb(8090) + Apache(80)，并做连通性自检 | 普通 |
| `scripts\stop_all.bat` | 一键停止，顺序 Apache → okxweb → MySQL | 普通 |

**编码约定（重要）**：`.bat` 一律保存为 **ANSI / GBK(cp936) + CRLF**，第二行 `chcp 936`。
文件名全 ASCII，中文只出现在 `echo` 文本里。

原因：cmd.exe 是按**字节偏移**重读批处理文件的。若存成 UTF-8 又在文件里 `chcp 65001`，
代码页切换会让偏移错位，中文字节会把后面的 ASCII 吃掉，典型症状是
`'lse' 不是内部或外部命令`（`else` 被啃掉）、`'026'`（`2026`）、`命令语法不正确`。
存 GBK 时控制台代码页不变，偏移恒定，中文还能正常显示。

`.gitattributes` 已写死 `*.bat text eol=crlf`，防止克隆后变回 LF。

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
* **加仓**：15m 先跌 0.5% 后转涨 → 补原保证金 1/3，**最多 3 次**；加满后信号再来 → 自动平仓。
* **超时**：开仓满 **4 小时（240 分钟）**未止盈 → 自动平仓。**不设止损**（`stop_loss_pct: 0`）。
* 所有口径都在 `configs/okx_strategy.json`（支持注释、热加载），网页「数据服务」面板实时显示。

### 3. 历史信号回算（K 线上的 🚀）

回补只入库 K 线，历史信号由 `[SIG-BF]` 后台协程逐根回算：
与实时扫描**同一套 `ComputeSignal`**（口径一致），score 够阈值的写入 `signals`
表（`INSERT IGNORE` 幂等 + **双向水位线**增量）。启动 20 秒后跑第一轮，之后每 30 秒增量一轮。
图上金色 🚀 = 买入信号/开仓，🍃 = 平仓卖出。

* **回算周期**：`signal_bars` 配置，当前 `["5m","15m","1H","4H"]`
  （1m/3m 已去掉：历史太长、噪音大。K 线图仍可切到这两个周期看，只是不画信号）
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

| 遍 | 内容 | 代价 |
| --- | --- | --- |
| 第一遍 Light | 每个 (合约,周期) 只拉最新 300 根 | 约 2900 次请求 / 几分钟，铺完就能出信号 |
| 第二遍 Full | 再逐个往前翻满 30 天 | 已铺够的走轻量路径直接跳过 |

周期顺序 `5m → 15m → 1H → 4H → 3m → 1m`：要用的周期先满。
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

### 8. 后台独立运行（不用开网页）

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
