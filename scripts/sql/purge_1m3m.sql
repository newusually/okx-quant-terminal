-- purge_1m3m.sql —— 彻底删除 1m / 3m K 线，回收磁盘
--
-- 为什么用「新建表 + 搬数据 + 换名」而不是 DELETE：
--   kline 按 ts 分区（周分区 × 25 个），而 bar 不是分区键，
--   `DELETE WHERE bar IN ('1m','3m')` 无法做分区裁剪 —— 每批都要全表扫，
--   1416 万行分批删会跑几小时，而且期间事务/undo/binlog 把盘再吃一遍。
--   重建整表只需要一次全表扫 + 一次顺序写，而且 DROP 旧表立刻把空间还给系统。
--
-- 保留：5m / 15m / 1H / 4H（用户口径：1m、3m 不要了，占掉全表 70% 的行）
--
-- ⚠️ 执行前必须先停 OKXWeb，否则服务侧的 PurgeBadKlines / RefreshCoverage
--    会持有 kline 的 MDL，RENAME TABLE 会永久等待。
-- ⚠️ 本文件只放 DDL/DML，不放「清理前」统计 —— 那是全表扫，白等几分钟；
--    验证 SELECT 也不放这里：mysql 客户端遇错即中止，验证语句报错会让人
--    误判清理失败。清理前后行数另跑 ad-hoc 查询。

SET SESSION innodb_lock_wait_timeout = 1800;
SET SESSION lock_wait_timeout = 1800;

-- 1) 新表结构（LIKE 会连分区定义一起复制）
--    DROP IF EXISTS 放最前，保证脚本可重复执行（上次失败留下的半成品先清掉）
DROP TABLE IF EXISTS kline_new;
CREATE TABLE kline_new LIKE kline;

-- 2) 搬数据：只搬要保留的 4 个周期。
--    不加 ORDER BY —— 源表按主键 (inst_id,bar,ts) 扫描，顺序写新表最快；
--    加排序反而要多开一份临时空间。
INSERT INTO kline_new (inst_id,bar,ts,o,h,l,c,v)
  SELECT inst_id,bar,ts,o,h,l,c,v FROM kline WHERE bar IN ('5m','15m','1H','4H');

-- 3) 换名 + 删旧表（DROP 立即回收十几 GB 中的绝大部分）
RENAME TABLE kline TO kline_1m3m_old, kline_new TO kline;
DROP TABLE kline_1m3m_old;

-- 4) 统计信息刷新，让优化器立刻按新行数选执行计划
ANALYZE TABLE kline;
