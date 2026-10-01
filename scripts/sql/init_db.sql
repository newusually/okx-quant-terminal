-- ===========================================================================
--  OKX 全合约量化终端 · MySQL 初始化脚本（手工兜底）
--  ---------------------------------------------------------------------------
--  正常情况下不用手工执行，而且**不要用这个脚本管理口令**：
--    · 全新安装：scripts\install_services.bat 自动建库建账号，
--                口令随机生成并写进 <项目根>\.mysql-pass
--    · 改口令  ：scripts\set_db_pass.bat —— 双击即可，且不需要 root
--                （okx 账号可以改自己的口令：SET PASSWORD）
--    · 服务运行：口令按 环境变量 OKX_MYSQL_PASS → <项目根>\.mysql-pass 解析；
--                源码与批处理里**不含任何明文口令**（见 internal/conf/secret.go）
--
--  只有在「root 已设密码、install_services.bat 无法自动建库」时才手工跑：
--      mysql -uroot -p --default-character-set=utf8mb4 < scripts\sql\init_db.sql
--
--  ★ 跑之前必须把下面的 CHANGE_ME 换成真实口令 ★
--    SQL 没法读环境变量，只能靠人替换 —— 这也是更推荐 set_db_pass.bat 的原因。
--    当前该用什么口令，看 <项目根>\.mysql-pass 的第一行。
--
--  历史：2026-10-01 之前这里写的是明文口令（'OkxQuant****'）并随 public 仓库公开。
--        源码侧已全部改成运行时解析，但**只改代码堵不住泄漏** ——
--        历史提交里那把旧口令一直能用。已用 scripts\set_db_pass.bat --gen
--        真实轮换，旧值现在返回 Access denied。
-- ===========================================================================

CREATE DATABASE IF NOT EXISTS okx
  DEFAULT CHARACTER SET utf8mb4
  COLLATE utf8mb4_general_ci;

-- 业务账号：只给 127.0.0.1 / localhost，不开放远程
CREATE USER IF NOT EXISTS 'okx'@'127.0.0.1' IDENTIFIED WITH mysql_native_password BY 'CHANGE_ME';
CREATE USER IF NOT EXISTS 'okx'@'localhost' IDENTIFIED WITH mysql_native_password BY 'CHANGE_ME';

-- 账号已存在时把口令对齐，避免「.mysql-pass 是新的、库里还是旧的」这种劈叉状态
ALTER USER 'okx'@'127.0.0.1' IDENTIFIED WITH mysql_native_password BY 'CHANGE_ME';
ALTER USER 'okx'@'localhost' IDENTIFIED WITH mysql_native_password BY 'CHANGE_ME';

GRANT ALL PRIVILEGES ON okx.* TO 'okx'@'127.0.0.1';
GRANT ALL PRIVILEGES ON okx.* TO 'okx'@'localhost';
FLUSH PRIVILEGES;

USE okx;

-- ---------------------------------------------------------------------------
--  表结构由 Go 侧 repo.schemaStmts 负责创建，这里只做「确认清单」。
--  跑一次下面的语句，应该看到 11 张表：
--    ai_call, backfill_job, equity, inst, kline, meta,
--    pnl_point, runlog, signals, ticker, trade
-- ---------------------------------------------------------------------------
SHOW TABLES;
