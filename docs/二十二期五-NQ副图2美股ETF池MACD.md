# 二十二期·五：NQ 图副图 2 —— 美股/ETF 池 takervol 总和 MACD（MACD2）

**日期**：2026-10-03　**状态**：已上线（服务 8090，6/6 探针断言 PASS）

---

## 一、需求原文与拆解

> 「NQ 这个 macd2 数据绑定 为所有美股+ETF数据的每5分钟的takervol总和 还是12 26 60 参数
> 给我把这个macd2副图绑定到NQ的macd副图下面给我呆着 还是数据库保存 绑定NQ主图K线数据
> 移动缩小放大都跟随主图 和副图1的macd一样」

| # | 约束 | 落地 |
|---|------|------|
| 1 | 数据 = **所有**美股+ETF 每 5m 的 takervol 总和 | 池 = `inst_category='3'` 全部 **190 个**合约（不按成交额截断），每 5m `SUM(buy)/SUM(sell)` |
| 2 | 参数还是 12 / 26 / 60 | 与副图 1 **共用同一条 MACD 算路** `macdCalcOn()`（抽出的公共函数） |
| 3 | 副图 2 绑在副图 1 下面 | 独立 priceScale `nqmacd2`（74%~97% 区间），副图 1 上移到 48%~71% |
| 4 | 数据库保存，读取直接走库 | 新表 `taker_macd_us`（8640 行），`/api/nqchart` 纯 SELECT |
| 5 | 缩放平移跟随主图 | lightweight-charts v4.2 结构上只有**单一 timeScale** —— 三层天然同步，无需事件同步代码 |

## 二、关键事实（动手前实测）

- **taker_vol 里原本一条美股数据都没有**：表里 84 个合约全是加密（category=1）。
  所以第一步是把 190 个美股/ETF 合约的 30 天数据拉进来。
- priapi `takerBuySellVol` 对美股合约**正常出数**（SNDK/TSLA/XAU 实测均有）；
  1H 粒度可回溯 **42.9 天**，30 天窗口够用。
- 美股/ETF 合约的 taker 数据是 **24 小时连续**的（OKX 美股永续全天交易），
  所以聚合序列本身无休市缺口，是「按 NQ 时间戳对齐」这一步把它压到纳指交易时段。

## 三、实现

### 数据管道（复用为主）

```
priapi takerBuySellVol
   └→ taker_vol（★ 与加密池共用一张表：两池合约集合不相交，
       主键 (inst_id,bar,ts) 天然隔离，回补/增量/水位线/清理全复用）
        └→ QueryTakerAgg(pool, from, to, withTop=false)   ← 同一条聚合 SQL
             └→ taker_macd_us（聚合总和 + MACD 12/26/60，8640 行）
                  └→ /api/nqchart 的 macd2 字段（按 NQ 柱子 ts 过滤对齐）
                       └→ nqchart.js 副图 2
```

- **首铺**：190/190 合约、1,439,307 行、耗时 2m48s（并发 4 + 429 指数退避）。
- **增量**：独立 goroutine（不占 realtimeLoop），60s 敲一次、跨根才真干活；
  缺口 >3 根自动升级为 30 天整体重拉（防服务停摆后永久缺段）。
- **频率**：190 请求 / 5 分钟 ≈ 0.63 req/s，远低于 429 阈值。

### 防呆设计（本项目踩坑史的直接转化）

| 坑 | 对策 |
|----|------|
| 两个池各写一份 MACD → 迟早漂 | 抽 `macdCalcOn()`，副图 1/2 共用；EMA 实现与 K 线 MACD 同源 |
| 副图 2 后铺好，首屏判「没变化」不重绘 | 前端 lastKey 指纹加入 `macd2.length` 维度 |
| 池子变了只覆盖指标、留下旧总和 | `UpsertTakerMacdUS` 覆盖**全部业务列** |
| 缺口只用 2h 增量补不回来 | Ensure 内置缺口检测：>15 分钟走 30 天整体重拉 |
| 美股池阻塞 realtimeLoop | 独立 goroutine，再慢也伤不到主链路 |
| 全局名冲突（fmtVol 老坑） | 新函数 `nqFmtVol`、新 ID 全带 `nqLg2` 前缀 |

## 四、验收（全部实测，非推断）

| 项 | 结果 |
|----|------|
| Python 独立重算 MACD（从 taker_vol 按 category=3 重新聚合 → 自己实现 EMA） | **与库逐位相同**（DIF/DEA/HIST 差均为 0.00e+00，排除最近 1 天仍在补齐的根后） |
| 副图 2 与 NQ 柱子对齐 | 5544/5544 点，错位 **0** |
| 探针 6 断言（几何/序列/对齐/单 timeScale/缩放生效/图例） | **6/6 PASS**，零 JS 报错 |
| 缩放跟随 | v4.2 无多 pane 概念 → 结构保证；`setVisibleLogicalRange` 收窄到 200 根实测生效 |
| 图例 | 「美股+ETF 买 43.60万 / 卖 33.26万 · DIF · DEA · MACD」，悬浮显示买卖比与合约数 |

## 五、已知边界

1. **最近 2~3 根的合约数偏少**（priapi 出数就慢，与加密池副图同一特性）：
   最新根可能只有 60~130/190 个合约报数，总和偏小；下一轮增量（5 分钟）自动补齐，
   整体重算会自动修正 MACD —— 图上表现为「最右几根会微调」，属数据源特性非 bug。
2. NQ 面板默认 300px 高，塞三层后每层较矮；拖 `h-split[data-split="B"]` 可拉高。
3. 副图 2 的 MACD 数值量级（±0.4）比副图 1（±0.02）大一个数量级 —— 两个
   priceScale 独立，各画各的零轴，不共用刻度。

## 六、改动清单

| 文件 | 内容 |
|------|------|
| `internal/repo/mysql.go` | 新表 `taker_macd_us` DDL |
| `internal/repo/taker_repo.go` | `TakerUSMacdRow` + Upsert/Query/Range；`QueryTakerAgg` 加 `withTop` 参数（两池共用同一段聚合 SQL） |
| `internal/service/taker_panel.go` | MACD 计算抽成公共 `macdCalcOn()` |
| `internal/service/taker_us.go`（新） | `TakerUSPool` / `TakerUSRebuild` / `TakerUSEnsure`（含缺口检测） |
| `internal/service/backfill.go` | `SetTakerUSIDs` + 首铺 + 常驻刷新 goroutine |
| `cmd/okxweb/main.go` | 注入美股池回调（category='3' 全量） |
| `internal/handler/api_nqchart.go` | 返回 `macd2`（同 have 集合对齐）+ usPool + note 更新 |
| `web/assets/nqchart.js` | 副图 2 三序列 + `nqmacd2` priceScale + 图例 + 指纹加维度 |
| `web/assets/index.html` | 图例第三行 + foot 说明 + 注释 |
| `web/assets/style.css` | NQ 图例压缩（10px）+ 副图 2 行分隔线 |

回滚备份：`bin/okxweb.exe.bak_22h`
