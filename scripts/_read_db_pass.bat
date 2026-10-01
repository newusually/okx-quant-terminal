@echo off
rem ===========================================================================
rem  OKX 全合约量化终端 —— 读取 MySQL 口令（内部 helper，不要单独双击）
rem  ---------------------------------------------------------------------------
rem  用法：先 set "ROOT=<项目根>"，再  call "%ROOT%\scripts\_read_db_pass.bat"
rem       返回后可直接用 %MYSQL_USER% 和 %MYSQL_PASS%
rem
rem  ★ 本文件**故意不加 setlocal** ★
rem    加了 setlocal 变量出了这个文件就没了，调用方拿不到 —— 这类
rem    「helper 里 setlocal」的写法是批处理里最常见的静默失效。
rem
rem  优先级与 Go 侧 internal/conf/secret.go 严格一致，顺序不能反：
rem    1. 环境变量 OKX_MYSQL_USER / OKX_MYSQL_PASS
rem    2. 密钥文件 <项目根>\.mysql-pass（第一行）
rem    3. 空
rem  两边顺序不一致会出现「服务连得上、脚本连不上」这种鬼问题。
rem
rem  口令绝不回显。要看配没配，只看「有 / 无」。
rem ===========================================================================

set "MYSQL_USER="
set "MYSQL_PASS="

rem ---- 1. 环境变量优先 ----
if defined OKX_MYSQL_USER set "MYSQL_USER=%OKX_MYSQL_USER%"
if defined OKX_MYSQL_PASS set "MYSQL_PASS=%OKX_MYSQL_PASS%"

rem ---- 2. 密钥文件兜底（set /p 只取第一行，正好）----
if not defined MYSQL_PASS if exist "%ROOT%\.mysql-pass" (
    set /p MYSQL_PASS=<"%ROOT%\.mysql-pass"
)

rem ---- 3. 用户名的兜底默认值。口令没有默认值，没有就是没有 ----
if not defined MYSQL_USER set "MYSQL_USER=okx"

exit /b 0
