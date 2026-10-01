-- slim_to_15m.sql —— 全库瘦身：K 线只保留 15m
--
-- 用户口径（2026-10-01）：「只保留 15 分钟的信号和买卖点，
-- 4H / 1H / 5m 全部删除；检查数据库是否还有其它数据也删除，
-- 只保留应该保留的信号和买卖点、持仓、交易等信息」。
--
-- 本脚本做的三件事：
--   ① kline 重建，只留 bar='15m'（约 594 万 → 137 万行，回收 ~460MB）
--   ② signals / signal_scan_state / backfill_job 删掉非 15m 的
--   ③ ANALYZE 刷新统计
--
-- 保留不动的：trade（持仓 + 历史仓位）、trade_event（买卖点/流水）、
--            equity（权益曲线）、runlog（日志）、inst/ticker（合约与行情快照）、
--            meta、ai_call、pnl_point、kline_bench（后两张是空表，DDL 会自动建）
--
-- ⚠️ 执行前必须停 OKXWeb，否则服务侧的写入会持有 kline 的 MDL，
--    RENAME TABLE 会永久等待。
-- ⚠️ 只放 DDL/DML，不放验证 SELECT（mysql 客户端遇错即中止）。

SET SESSION innodb_lock_wait_timeout = 1800;
SET SESSION lock_wait_timeout = 1800;

-- ---------------------------------------------------------------- ① K 线
-- 为什么又是重建整表：kline 按 ts 分区，bar 不是分区键，
-- `DELETE WHERE bar<>'15m'` 无法分区裁剪，457 万行分批删要跑很久，
-- 而且期间 undo/binlog 会把盘再吃一遍。重建只需一次全表扫 + 一次顺序写。
DROP TABLE IF EXISTS kline_new;
CREATE TABLE kline_new LIKE kline;

INSERT INTO kline_new (inst_id, bar, ts, o, h, l, c, v)
  SELECT inst_id, bar, ts, o, h, l, c, v FROM kline WHERE bar = '15m';

RENAME TABLE kline TO kline_allbars_old, kline_new TO kline;
DROP TABLE kline_allbars_old;

ANALYZE TABLE kline;

-- ---------------------------------------------------------------- ② 信号
-- signals 只有一万多行，直接删，不用重建。
-- 买卖点不在这张表里 —— 买卖点 = trade_event（kind=open/addon/close），保留。
DELETE FROM signals WHERE bar <> '15m';

-- 信号回算的水位线：只留 15m，其余周期下次回算会自己重建
DELETE FROM signal_scan_state WHERE bar <> '15m';

-- 回补队列进度：6 周期时代留下的记录，只留 15m
DELETE FROM backfill_job WHERE bar <> '15m';

ANALYZE TABLE signals;
ANALYZE TABLE signal_scan_state;
ANALYZE TABLE backfill_job;
