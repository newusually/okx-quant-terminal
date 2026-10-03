# 二十二期：taker 买卖流向面板（实时 + 30 天历史）

> 2026-10-03。用户口径：实时 + 历史 30 天、**非美股 + 非 ETF** 的 taker vol 买卖比总和；
> 面板位置 **K 线图左边、合约列表面板右边**；四列：时间 / 5m takervol 买:卖 / ETH 下根涨跌幅 /
> 单切片涨幅最高合约（名称 + 下根涨幅）。

## 一、最终形态

**面板**（`web/assets/takerflow.js` + `index.html` + `style.css`）：

| 列 | 内容 | 说明 |
|---|---|---|
| 1 | 时间 | 5m 切片，从过去到现在（表格里最新在上） |
| 2 | 买:卖 | 全池（非美股非ETF 24h 成交额前 80）主动买量 ÷ 主动卖量 |
| 3 | ETH→下根 | 该切片**下一根** 5m 的 ETH 涨跌幅（前瞻对齐） |
| 4 | 涨幅王 本根→下根 | 该切片**涨幅最高**的合约 + 当根涨幅 → 下一根涨跌幅 |

- 顶部汇总条：30 天全池总和 买 xxx 亿 / 卖 xxx 亿、比例 N:1（用户明确要「都加起来，总和」）
- **分页**：复用全站 `pgSlice`/`renderPager`，页码 1 2 3 4 5 可见可点、客户端切页（数据一次拉够）
- 「加载全部 30 天」按钮：一次拉 8640 切片（约 0.8MB）

**接口** `GET /api/takerflow?limit=&days=`（20s TTL 缓存）。

## 二、数据构成（5m 真数据 + 1H 前向填充）

- OKX priapi `rubik/public/stat/indicators?takerBuySellVol` 实测上限：**5m 只回溯 5 天（1439 根）**、1H 回溯 60 天
- 近 5 天 = 真 5m（`src='5m'`）；更早 25 天 = 1H 每根拆 12 格前向填充（`src='1Hfill'`，**滞后 1 小时**防未来函数）
- 表：`taker_vol (inst_id, bar, ts)` 主键 + `taker_scan_state` 水位线；首铺 80 合约 66 万行 / 57 秒
- 服务启动自动补缺（`TakerState` 判已铺够就跳过）；实时轮询只补最近 2 小时
- ★ `src` 必须进 upsert 覆盖列表 —— 1Hfill 要能被后来的真 5m **升级**

## 三、第 4 列口径（当日二次变更，最终版）

「涨幅最高」不是「成交量最大」：`QueryTakerAgg` 第二步 JOIN kline 按
`(k.c-k.o)/NULLIF(k.o,0) DESC` 取 `ROW_NUMBER()=1`。直连验证（12:10 切片）：
涨幅第一名 SAND +1.65%（接口返回一致）；若按量排会是 ETH -0.01% —— 口径确实换了。

## 四、当日踩坑（重要，都有实测）

1. **前端函数重名 = 整个脚本静默不执行**：`fmtVol/fmtPct` 与 core.js 撞名 → SyntaxError →
   面板永远「加载中」。对策：taker 面板全部 `tk` 前缀。
2. **HTTP 429**：priapi 限频极紧。`takerFetchPage` 指数退避 1.2s/2.5s/5s + 并发 3，
   并改「有行就写」（部分成功不整批丢）。
3. **★ fetch 卡死 → loading 锁死 → 面板永远「加载中」**（本项目新坑）：
   boot 瞬间十几条并发请求打满连接池（headless/低内存机器必现），fetch 的 promise
   **既不 resolve 也不 reject**，`finally` 永不执行，30 秒轮询全被 `if (loading) return` 吞掉。
   对策三件套：AbortController 12s 超时 + `loadingAt` 时间戳 60s 自解 + 5s/15s 补拉
   （补拉必须同时查 `!loading`，否则被守卫吞掉）。
4. **★ 接口缓存没按窗口判定**：20s 内先小窗口（2880）后大窗口（8640）会静默拿到
   小窗口结果（实测 slices 8640 → 2883）。修复：缓存记 `limit/days`，命中条件加
   `limit<=已算 && days<=已算`（小请求复用大缓存仍允许）。
5. **启动日志必须 `logx.Logf`**：服务模式无控制台，`fmt.Printf`/`m.logf` 直接丢弃（老坑复发）。

## 五、验收证据（CDP headless，全部 PASS）

- 几何：合约列表 x=10~278 → taker x=304~634 → K线 x=660 起 ✓
- 数据：`rows=8640 slices=8641 pool=80`（09-03 12:45 → 10-03 12:40 全覆盖）；
  汇总比 0.98:1 ~ 1.01:1（BTC 1.007 / ETH 1.058 合理）
- 分页：`共 2880 条 · 显示 1-20 · 第 1/144 页`，点 2/3 内容真实切换、页码高亮 ✓
- 第 4 列形态：`WLD +0.67%→-0.04%`、`SAND +4.52%→-0.79%` ✓
- 缓存修复后：大→小→大窗口混用，始终 8640 ✓

截图：`tmp/taker_shot.png`（全景）、`tmp/taker_pager.png`（分页终验）。
验收探针：`tmp/probe-taker.js`（几何）、`tmp/probe-taker2.js`（分页+第4列）。

## 六、文件清单

| 文件 | 变更 |
|---|---|
| `internal/repo/mysql.go` | +taker_vol / taker_scan_state DDL |
| `internal/repo/taker_repo.go` | 新增：Upsert/QueryTakerAgg/水位线/清理 |
| `internal/service/taker.go` | 新增：拉取+退避+1H填充+池口径 `TakerPoolByVolume` |
| `internal/service/backfill.go` | 首铺 30 天 + 实时增量（SetTakerIDs 注入池） |
| `internal/handler/api_takerflow.go` | 新增：/api/takerflow（缓存含窗口判定） |
| `internal/handler/server.go` | 路由 + tk 缓存字段 |
| `cmd/okxweb/main.go` | 注入 taker 候选池回调 |
| `web/assets/takerflow.js` | 新增：面板逻辑（tk 前缀/超时自愈/分页） |
| `web/assets/index.html` `style.css` `panels.js` `main.js` | 面板 DOM/样式/第三条分隔条/启动 |
