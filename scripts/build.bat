@echo off
chcp 936 >nul 2>&1
title OKX 终端 - 编译
setlocal EnableExtensions

rem ===========================================================================
rem  OKX 全合约量化终端 —— 编译
rem  产出 bin\okxweb.exe（网页+引擎）
rem ===========================================================================

set "SYS=%SystemRoot%\System32"
set "ROOT=%~dp0.."
pushd "%ROOT%"
set "ROOT=%CD%"
popd
cd /d "%ROOT%"

rem 用本地 Go，不去自动下载 toolchain（服务器上没梯子会卡住）
set GOTOOLCHAIN=local
set GOFLAGS=-mod=mod
if "%GOPROXY%"=="" set GOPROXY=https://goproxy.cn,direct

echo.
echo ==============================================================
echo  编译 OKX 终端
echo ==============================================================
echo.

"%SYS%\where.exe" go >nul 2>&1
if errorlevel 1 (
    echo [错误] PATH 里找不到 go.exe，请先装 Go 或把它加进 PATH。
    echo.
    pause
    exit /b 1
)

echo [1/3] go build ./...
go build ./...
if errorlevel 1 goto :fail

echo.
echo [2/3] go vet ./...
go vet ./...
if errorlevel 1 goto :fail

echo.
echo [3/3] 产出可执行文件 ...
if not exist bin mkdir bin
go build -trimpath -ldflags "-s -w" -o bin\okxweb.exe ./cmd/okxweb
if errorlevel 1 goto :fail

echo.
dir /b bin\okxweb.exe
echo.
echo ==============================================================
echo  编译完成。接着 scripts\start_all.bat 启动。
echo ==============================================================
echo.
pause
exit /b 0

:fail
echo.
echo ==============================================================
echo  [失败] 编译不通过，上面的报错先修掉。
echo ==============================================================
echo.
pause
exit /b 1
