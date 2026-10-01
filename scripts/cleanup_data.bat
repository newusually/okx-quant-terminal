@echo off
chcp 936 >nul 2>&1
title OKX 终端 - 月度自动维护（归档 + 清理 + 磁盘守卫）
setlocal EnableExtensions

rem ===========================================================================
rem  OKX 全合约量化终端 —— 月度自动维护（手工触发版）
rem  ---------------------------------------------------------------------------
rem  常驻服务本来就会自动跑：启动后 90 秒起、每 30 分钟 tick 一次，比对 meta 表
rem  里的 maint_last_monthly / maint_last_yearly，不同月/年才真跑 —— 所以
rem  停机重启多少次，一个月也只跑一次，不重不漏。这个 bat 是手工核对用的。
rem
rem  月度任务六步（顺序不可换，第 2 步必须在第 5 步之前）：
rem    1) 归档上月 15m K 线  -> archive\kline-15m-YYYY-MM.partNN.csv.gz
rem    2) 推送 GitHub 数据仓  <- 必须在第 5 步之前！先删后推一旦推失败
rem                              = 本地没了 + 远端也没有 = 数据永久丢失
rem    3) 记录表清理（30 天）：历史仓位 / 交易记录 / 信号 / 权益 / 运行日志 /
rem                            AI 调用 / 盈亏点。trade 只删已平仓的
rem    4) 日志清理（30 天）：logs\ + apache\logs\ 里超过一个月的日志、归档、截图
rem    5) 磁盘守卫：C 盘可用 < 10 GB 时，删掉多余的历史月份归档，只保留当月
rem    6) 清回收站：C:\$Recycle.Bin\<SID>\ 下的垃圾文件
rem
rem  年度任务（一年只跑一次）：删掉早于 kline_retain_days（默认 365 天）
rem  的 K 线。走 ALTER TABLE DROP PARTITION（毫秒级、不产生 binlog），
rem  不是 DELETE 扫千万行。
rem
rem  保留窗口都在 configs\okx_strategy.json 的 store 段：
rem    retain_days=30 / kline_retain_days=365 / log_retain_days=30 /
rem    archive_min_free_gb=10 / disable_recycle_clean=false
rem
rem  用法：
rem    scripts\cleanup_data.bat           月度：预演 + 询问是否真跑
rem    scripts\cleanup_data.bat /yes      月度：直接真跑，不问
rem    scripts\cleanup_data.bat /yearly   年度：预演 + 询问是否真跑
rem    scripts\cleanup_data.bat /yearly /yes
rem ===========================================================================

set "SYS=%SystemRoot%\System32"
set "ROOT=%~dp0.."
pushd "%ROOT%"
set "ROOT=%CD%"
popd
cd /d "%ROOT%"

if not exist "bin\okxweb.exe" (
    echo [错误] 找不到 bin\okxweb.exe
    pause
    exit /b 1
)

if /I "%~1"=="/yearly" goto :YEARLY

echo.
echo ==============================================================
echo  月度自动维护 · 归档 + 记录表 30 天 + 日志 30 天 + 磁盘守卫
echo  项目目录：%ROOT%
echo ==============================================================
echo.

echo [1/2] 预演（只报告会做什么，一行都不删不推）...
echo --------------------------------------------------------------
"bin\okxweb.exe" -maint-dry
echo --------------------------------------------------------------
echo.

if /I "%~1"=="/yes" goto :DOIT

set /P ANS=确认执行真实月度维护吗？输入 Y 回车继续，其它任意键取消：
if /I not "%ANS%"=="Y" (
    echo 已取消，什么都没做。
    pause
    exit /b 0
)

:DOIT
echo.
echo [2/2] 实际执行月度维护（归档 + 推送 + 清理 + 磁盘守卫 + 回收站）...
echo --------------------------------------------------------------
"bin\okxweb.exe" -maint
echo --------------------------------------------------------------
echo.
echo 完成。日志同时写进了 logs\okxbot.log 和数据库 runlog 表。
pause
exit /b 0

:YEARLY
echo.
echo ==============================================================
echo  年度 K 线清理 · 红线 kline_retain_days（默认 365 天）
echo  项目目录：%ROOT%
echo ==============================================================
echo.
echo 内容：DROP 掉上界早于红线的 kline 分区（毫秒级），残余走
echo       DELETE ... LIMIT 5000 兜底分批删。p_old / pmax 永不 DROP。
echo.

rem ★ 年度清理是真 DROP PARTITION，不可逆。必须先走 -maint-yearly-dry。
echo [1/2] 预演年度清理（只报告，不删）...
echo --------------------------------------------------------------
"bin\okxweb.exe" -maint-yearly-dry
echo --------------------------------------------------------------
echo.

if /I "%~2"=="/yes" goto :YDOIT

set /P ANS2=确认执行真实年度清理吗？输入 Y 回车继续，其它任意键取消：
if /I not "%ANS2%"=="Y" (
    echo 已取消，什么都没删。
    pause
    exit /b 0
)

:YDOIT
echo.
echo [2/2] 实际执行年度清理 ...
echo --------------------------------------------------------------
"bin\okxweb.exe" -maint-yearly
echo --------------------------------------------------------------
echo.
echo 完成。
pause
