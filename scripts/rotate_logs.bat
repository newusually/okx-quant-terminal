@echo off
chcp 936 >nul 2>&1
title OKX 终端 - 日志归档轮转
setlocal EnableExtensions EnableDelayedExpansion

rem ===========================================================================
rem  OKX 全合约量化终端 —— 大日志归档 + 30 天日志清理
rem  ---------------------------------------------------------------------------
rem  和 cleanup_data.bat 的分工：
rem    cleanup_data.bat  管「按月」的大扫除（归档 K 线 + 推 GitHub + 记录表清理
rem                      + 日志清理 + 磁盘守卫 + 清回收站），服务自己会定时跑。
rem    rotate_logs.bat   管「日志文件本身太大」这一件事 —— 可以在两次月度任务
rem                      之间随时手工跑，不涉及任何数据库数据。
rem
rem  为什么需要这个：
rem    logs\mysql-slow.log（MySQL 慢查询）和 logs\apache_access.log（访问日志）
rem    是被**别的进程**一直追加写的，不会自己按时间老化 —— 只靠「删 30 天前的
rem    文件」永远删不到它们。这两个文件一天能长几十 MB。
rem
rem  做法：停掉 MySQL / Apache（它们持有文件句柄，运行中改名会失败）
rem        -> 把超过阈值的日志改名搬进 logs\archive\
rem        -> 重新启动
rem        -> 顺手跑一次 -cleanup，把记录表和 archive 里过期的日志删掉
rem           （日志红线 = store.log_retain_days，默认 30 天；okxbot.log 是
rem            活动日志会被跳过，不会删掉正在写的文件）
rem
rem  用法：scripts\rotate_logs.bat            阈值 20MB
rem        scripts\rotate_logs.bat 50        自定义阈值（MB）
rem
rem  注意：本机 schtasks.exe 被安全策略拦，不能用 Windows 计划任务调度。
rem        日志老化已由服务内月度任务覆盖（第 4 步），这个 bat 只在
rem        「某个日志文件突然涨得很大、等不到月底」时手工跑。
rem ===========================================================================

set "SYS=%SystemRoot%\System32"
set "ROOT=%~dp0.."
pushd "%ROOT%"
set "ROOT=%CD%"
popd
cd /d "%ROOT%"

set "LOGDIR=%ROOT%\logs"
set "ARCH=%LOGDIR%\archive"
set "LIMIT_MB=%~1"
if "%LIMIT_MB%"=="" set "LIMIT_MB=20"
set "LIMIT_BYTES=0"
set /A LIMIT_BYTES=%LIMIT_MB% * 1048576

if not exist "%ARCH%" mkdir "%ARCH%" >nul 2>&1

rem 中文 Windows 的 %DATE% 形如「2026/10/01 周四」——取前三个字段拼 YYYYMMDD
for /f "tokens=1-3 delims=/ " %%a in ("%DATE%") do set "D=%%a%%b%%c"
if "%D%"=="" set "D=unknown"

echo.
echo ==============================================================
echo  日志归档轮转 · 阈值 %LIMIT_MB% MB
echo  %DATE% %TIME%
echo ==============================================================
echo.

rem ---- 1. 停 Apache / MySQL（释放文件句柄）--------------------------------
echo [1/4] 停止 OKXApache / OKXMySQL ...
net stop OKXApache >nul 2>&1
net stop OKXMySQL  >nul 2>&1
rem 用 ping 等 3 秒（不用 timeout：重定向 stdin 时会报错）
ping -n 4 127.0.0.1 >nul

rem ---- 2. 归档超过阈值的大日志 --------------------------------------------
echo [2/4] 归档超过 %LIMIT_MB% MB 的日志 ...
set "MOVED=0"
for %%F in ("%LOGDIR%\mysql-slow.log" "%LOGDIR%\mysql-error.log" ^
            "%LOGDIR%\apache_access.log" "%LOGDIR%\apache_error.log" ^
            "%LOGDIR%\okxweb_run.log" "%LOGDIR%\okxweb_stdout.log") do (
    if exist "%%~fF" (
        for %%S in ("%%~fF") do set "SZ=%%~zS"
        if !SZ! GEQ %LIMIT_BYTES% (
            move /Y "%%~fF" "%ARCH%\%%~nF-!D!%%~xF" >nul 2>&1
            if not errorlevel 1 (
                echo       [归档] %%~nxF  ^(%SZ% 字节^)
                set /A MOVED+=1
            )
        )
    )
)
if "%MOVED%"=="0" echo       （没有超过阈值的日志）

rem ---- 3. 重新启动 --------------------------------------------------------
echo [3/4] 重新启动 OKXMySQL / OKXApache ...
net start OKXMySQL  >nul 2>&1
net start OKXApache >nul 2>&1
echo       服务已拉回。

rem ---- 4. 顺手跑一次清理（会删掉 archive 里过期的日志）---------------------
echo [4/4] 执行记录表 / 日志清理（默认 30 天红线）...
if exist "bin\okxweb.exe" (
    "bin\okxweb.exe" -cleanup
) else (
    echo       [跳过] 找不到 bin\okxweb.exe
)

echo.
echo ==============================================================
echo  完成。
echo ==============================================================
pause
