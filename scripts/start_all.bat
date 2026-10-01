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

rem 口令：环境变量 OKX_MYSQL_PASS → 根目录 .mysql-pass（与 Go 侧解析顺序一致）。
rem ★ 这两行必须留在下面那个 if(...) 块**外面** ★
rem   块内的 %PWARG% 会在「整块被读到」时就展开，而 call 是块执行时才跑的
rem   → 恒为空。批处理最经典的静默失效：不报错，只是探测永远匿名。
call "%ROOT%\scripts\_read_db_pass.bat"
set "PWARG="
if defined MYSQL_PASS set "PWARG=-p%MYSQL_PASS%"

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
    rem 等真的能连上再往下走（口令来自上面的 .mysql-pass / 环境变量）
    if defined MYSQL_PASS (
        echo       口令已配置（%MYSQL_USER%）。
    ) else (
        echo       [警告] 没找到 MySQL 口令：先双击 scripts\set_db_pass.bat
    )
    "%ROOT%\mysql\bin\mysqladmin.exe" -u%MYSQL_USER% -h127.0.0.1 %PWARG% --connect-timeout=5 ping >nul 2>&1
    if errorlevel 1 (
        echo       [警告] 连不上 127.0.0.1:3306
    ) else (
        echo       连接测试通过。
    )
)
echo.

rem ---- 2. Go 网页服务（Windows 服务，无 cmd 黑窗口）------------------------
echo [2/4] 启动 Go 网页服务 127.0.0.1:8090 ...
rem ★ 一律走 Windows 服务：跑在 Session 0，根本不存在控制台窗口，崩溃还会自动重启。
rem   老写法是 `start "OKX Web 8090" /min bin\okxweb.exe ...`，那样会留下一个
rem   最小化的黑窗口（任务栏里一直挂着一个 cmd 图标），用户明确要求去掉。
rem   只有「服务没法用（没注册 + 当前不是管理员）」时才退回隐藏窗口方式。
sc query OKXWeb >nul 2>&1
if errorlevel 1 (
    if exist "%ROOT%\bin\okxweb.exe" (
        echo       服务未注册，正在注册（需要管理员）...
        "%ROOT%\bin\okxweb.exe" -install
    ) else (
        echo       [错误] 找不到 bin\okxweb.exe，先跑 scripts\build.bat
    )
)
sc query OKXWeb >nul 2>&1
if errorlevel 1 (
    echo       [警告] 服务不可用，改为隐藏窗口方式启动（不会出现黑窗口）。
    if exist "%ROOT%\bin\okxweb.exe" (
        if exist "%ROOT%\scripts\hidden_run.vbs" (
            rem window style 0 = 完全隐藏；wscript /B 让脚本自己也不出声
            "%SYS%\wscript.exe" /B /Nologo "%ROOT%\scripts\hidden_run.vbs" "%ROOT%\bin\okxweb.exe" "-addr 127.0.0.1:8090 -days 365 -workers 6"
            echo       已启动（隐藏窗口，日志写在 logs\）。
        ) else (
            rem 最后的兜底：/B 不新建窗口，只是会跟着这个 cmd 一起退出
            start "" /B "%ROOT%\bin\okxweb.exe" -addr 127.0.0.1:8090 -days 365 -workers 6
            echo       已启动（无独立窗口，日志写在 logs\）。
        )
    )
) else (
    sc query OKXWeb | "%SYS%\findstr.exe" /I /C:"RUNNING" >nul
    if errorlevel 1 (
        net start OKXWeb >nul 2>&1
        if errorlevel 1 (
            echo       [错误] 启动失败，看 logs\okxbot.log
        ) else (
            echo       已启动（Windows 服务，无窗口）。
        )
    ) else (
        echo       已在运行（Windows 服务，无窗口）。
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
