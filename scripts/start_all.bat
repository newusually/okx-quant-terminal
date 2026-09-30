@echo off
chcp 936 >nul 2>&1
title OKX 终端 - 启动
setlocal EnableExtensions

rem ===========================================================================
rem  OKX 全合约量化终端 —— 一键启动
rem  ---------------------------------------------------------------------------
rem  顺序拉起：MySQL 服务 -> Go 网页服务(127.0.0.1:8090) -> Apache(0.0.0.0:80)
rem  三个组件都是后台常驻，关掉这个窗口不会把它们带走。
rem  用法：双击本文件，或命令行 scripts\start_all.bat
rem
rem  注意：findstr 一律走 System32 绝对路径，避免装了 Git/Cygwin 时被同名
rem        GNU 工具顶掉；等待用 ping 而不是 timeout，重定向 stdin 时也不会报错。
rem ===========================================================================

set "SYS=%SystemRoot%\System32"
set "ROOT=%~dp0.."
pushd "%ROOT%"
set "ROOT=%CD%"
popd
cd /d "%ROOT%"

echo.
echo ==============================================================
echo  OKX 全合约量化终端 · 启动
echo  项目目录：%ROOT%
echo ==============================================================
echo.

rem ---- 1. MySQL ------------------------------------------------------------
echo [1/4] 检查 MySQL 服务 OKXMySQL ...
sc query OKXMySQL >nul 2>&1
if errorlevel 1 (
    echo       [错误] 服务没注册。先跑 scripts\install_services.bat
) else (
    sc query OKXMySQL | "%SYS%\findstr.exe" /I /C:"RUNNING" >nul
    if errorlevel 1 (
        net start OKXMySQL >nul 2>&1
        if errorlevel 1 (
            echo       [错误] 启动失败，看 mysql\data\*.err
        ) else (
            echo       已启动。
        )
    ) else (
        echo       已在运行。
    )
    rem 等真的能连上再往下走
    "%ROOT%\mysql\bin\mysqladmin.exe" -uokx -pOkxQuant2026 -h127.0.0.1 --connect-timeout=5 ping >nul 2>&1
    if errorlevel 1 (
        echo       [警告] 连不上 127.0.0.1:3306
    ) else (
        echo       连接测试通过。
    )
)
echo.

rem ---- 2. Go 网页服务 ------------------------------------------------------
echo [2/4] 启动 Go 网页服务 127.0.0.1:8090 ...
tasklist /FI "IMAGENAME eq okxweb.exe" /NH 2>nul | "%SYS%\findstr.exe" /I /C:"okxweb.exe" >nul
if not errorlevel 1 (
    echo       已在运行，跳过。
) else (
    if not exist "%ROOT%\bin\okxweb.exe" (
        echo       [错误] 找不到 bin\okxweb.exe，先编译：
        echo              scripts\build.bat
    ) else (
        rem -days 30 保证回补至少一个月；-workers 6 适配 2 核机器
        start "OKX Web 8090" /min "%ROOT%\bin\okxweb.exe" -addr 127.0.0.1:8090 -days 30 -workers 6
        echo       已启动（最小化窗口，日志写在 logs\）。
    )
)
echo.

rem ---- 3. Apache ----------------------------------------------------------
echo [3/4] 检查 Apache 服务 OKXApache ...
sc query OKXApache >nul 2>&1
if errorlevel 1 (
    echo       [错误] 服务没注册。先跑 scripts\install_services.bat
) else (
    sc query OKXApache | "%SYS%\findstr.exe" /I /C:"RUNNING" >nul
    if errorlevel 1 (
        net start OKXApache >nul 2>&1
        if errorlevel 1 (
            echo       [错误] 启动失败，看 apache\logs\error.log
        ) else (
            echo       已启动。
        )
    ) else (
        echo       已在运行。
    )
)
echo.

rem ---- 4. 验证 ------------------------------------------------------------
echo [4/4] 等待服务就绪 ...
rem 用 ping 代替 timeout：ping 不吃 stdin，被重定向时也不会报错
"%SYS%\ping.exe" -n 9 127.0.0.1 >nul 2>&1
powershell -NoProfile -Command "try{$r=Invoke-WebRequest -Uri 'http://127.0.0.1:8090/api/state' -UseBasicParsing -TimeoutSec 10; Write-Host ('      Go 服务 OK  HTTP ' + $r.StatusCode)}catch{Write-Host '      [警告] Go 服务还没起来，稍等再刷新网页'}"
powershell -NoProfile -Command "try{$r=Invoke-WebRequest -Uri 'http://127.0.0.1/' -UseBasicParsing -TimeoutSec 10; Write-Host ('      Apache OK   HTTP ' + $r.StatusCode)}catch{Write-Host '      [警告] 80 端口不通，检查 Apache 服务'}"

echo.
echo ==============================================================
echo  完成。浏览器打开：http://localhost/
echo  --------------------------------------------------------------
echo  查看服务：net start ^| findstr /I OKX
echo  停止服务：scripts\stop_all.bat
echo ==============================================================
echo.
pause
endlocal
