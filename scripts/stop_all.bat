@echo off
chcp 936 >nul 2>&1
title OKX 终端 - 停止
setlocal EnableExtensions

rem ===========================================================================
rem  OKX 全合约量化终端 —— 一键停止
rem  顺序：Apache(对外入口) -> Go 网页服务 -> MySQL
rem  先断入口再停数据，避免请求打在半关闭的服务上。
rem
rem  注意：findstr 一律走 System32 绝对路径，
rem        避免装了 Git/Cygwin 时被同名 GNU 工具顶掉。
rem ===========================================================================

set "SYS=%SystemRoot%\System32"
set "ROOT=%~dp0.."
pushd "%ROOT%"
set "ROOT=%CD%"
popd
cd /d "%ROOT%"

echo.
echo ==============================================================
echo  OKX 全合约量化终端 · 停止
echo ==============================================================
echo.

echo [1/3] 停止 Apache 服务 OKXApache ...
sc query OKXApache | "%SYS%\findstr.exe" /I /C:"RUNNING" >nul
if errorlevel 1 (
    echo       未在运行。
) else (
    net stop OKXApache >nul 2>&1
    if errorlevel 1 (echo       停止失败。) else (echo       已停止。)
)

echo.
echo [2/3] 停止 Go 网页服务 okxweb.exe ...
taskkill /F /IM okxweb.exe >nul 2>&1
if errorlevel 1 (echo       未在运行。) else (echo       已停止。)

echo.
echo [3/3] 停止 MySQL 服务 OKXMySQL ...
sc query OKXMySQL | "%SYS%\findstr.exe" /I /C:"RUNNING" >nul
if errorlevel 1 (
    echo       未在运行。
) else (
    net stop OKXMySQL >nul 2>&1
    if errorlevel 1 (echo       停止失败。) else (echo       已停止。)
)

echo.
echo ==============================================================
echo  全部停止。
echo ==============================================================
echo.
pause
endlocal
