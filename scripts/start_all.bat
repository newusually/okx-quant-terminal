@echo off
chcp 65001 >nul
setlocal EnableDelayedExpansion

rem ===========================================================================
rem  OKX 全合约量化终端 —— 一键启动
rem  ---------------------------------------------------------------------------
rem  依次拉起：MySQL 服务 -> Go 网页服务(127.0.0.1:8090) -> Apache(0.0.0.0:80)
rem  三个组件都是后台常驻，关掉这个窗口不会把它们带走。
rem  用法：双击本文件，或命令行 scripts\start_all.bat
rem ===========================================================================

rem 以脚本所在目录的上一级为项目根，避免盘符/用户目录写死
set "ROOT=%~dp0.."
pushd "%ROOT%"
set "ROOT=%CD%"
popd

echo.
echo ==============================================================
echo  OKX 全合约量化终端 · 启动
echo  项目目录：%ROOT%
echo ==============================================================
echo.

rem ---- 1. MySQL ------------------------------------------------------------
echo [1/4] 检查 MySQL 服务 (OKXMySQL) ...
net start OKXMySQL >nul 2>&1
if errorlevel 1 (
    echo       已处于运行状态，跳过。
) else (
    echo       已启动。
)
rem 等服务真的能连上再往下走
"%ROOT%\mysql\bin\mysqladmin.exe" -uokx -pOkxQuant2026 -h127.0.0.1 ping >nul 2>&1
if errorlevel 1 (
    echo       [警告] 连不上 MySQL(127.0.0.1:3306)。请先跑 scripts\install_services.bat
) else (
    echo       连接测试通过。
)

rem ---- 2. Go 网页服务 ------------------------------------------------------
echo.
echo [2/4] 启动 Go 网页服务 (127.0.0.1:8090) ...
tasklist /FI "IMAGENAME eq okxweb.exe" 2>nul | find /I "okxweb.exe" >nul
if not errorlevel 1 (
    echo       已在运行，跳过。
) else (
    if not exist "%ROOT%\bin\okxweb.exe" (
        echo       [错误] 找不到 bin\okxweb.exe
        echo              先编译：go build -o bin\okxweb.exe ./cmd\okxweb
    ) else (
        rem -days 30 保证回补至少一个月；-workers 6 适配 2 核机器
        start "OKX Web" /min "%ROOT%\bin\okxweb.exe" -addr 127.0.0.1:8090 -days 30 -workers 6
        echo       已启动（最小化窗口，日志同时写 logs\）。
    )
)

rem ---- 3. Apache ----------------------------------------------------------
echo.
echo [3/4] 检查 Apache 服务 (OKXApache) ...
net start OKXApache >nul 2>&1
if errorlevel 1 (
    echo       已处于运行状态，跳过。
) else (
    echo       已启动。
)

rem ---- 4. 验证 ------------------------------------------------------------
echo.
echo [4/4] 等待服务就绪 ...
timeout /t 8 /nobreak >nul
powershell -NoProfile -Command "try{ $r=Invoke-WebRequest -Uri 'http://127.0.0.1/api/state' -UseBasicParsing -TimeoutSec 10; if($r.StatusCode -eq 200){ Write-Host '      Go 服务 OK (200)' } else { Write-Host ('      Go 服务异常 HTTP ' + $r.StatusCode) } } catch { Write-Host '      [警告] Go 服务还没起来，多等一会儿再刷新网页' }"
powershell -NoProfile -Command "try{ $r=Invoke-WebRequest -Uri 'http://127.0.0.1/' -UseBasicParsing -TimeoutSec 10; if($r.StatusCode -eq 200){ Write-Host '      Apache 80 端口 OK (200)' } else { Write-Host ('      Apache 异常 HTTP ' + $r.StatusCode) } } catch { Write-Host '      [警告] 80 端口不通，检查 Apache 是否启动' }"

echo.
echo ==============================================================
echo  完成。浏览器打开：http://localhost/
echo  --------------------------------------------------------------
echo  查看服务：net start ^| findstr /I "OKX"
echo  停止服务：scripts\stop_all.bat
echo ==============================================================
echo.
pause
