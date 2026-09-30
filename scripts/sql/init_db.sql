-- ===========================================================================
--  OKX 全合约量化终端 · MySQL 初始化脚本
--  ---------------------------------------------------------------------------
--  正常情况下不用手工执行：cmd/okxweb 启动时会自动建库建表（repo.Init()）。
--  只有在「root 已设密码、install_services.bat 无法自动建库」时才需要手工跑：
--      mysql -uroot -p --default-character-set=utf8mb4 < scripts\sql\init_db.sql
-- ===========================================================================

CREATE DATABASE IF NOT EXISTS okx
  DEFAULT CHARACTER SET utf8mb4
  COLLATE utf8mb4_general_ci;

-- 业务账号：只给 127.0.0.1 / localhost，不开放远程
CREATE USER IF NOT EXISTS 'okx'@'127.0.0.1' IDENTIFIED WITH mysql_native_password BY 'OkxQuant2026';
CREATE USER IF NOT EXISTS 'okx'@'localhost' IDENTIFIED WITH mysql_native_password BY 'OkxQuant2026';

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
