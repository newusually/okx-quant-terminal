@echo off
chcp 65001 >nul
setlocal EnableDelayedExpansion

rem ===========================================================================
rem  OKX 全合约量化终端 —— 一键停止
rem  顺序：Apache(对外入口) -> Go 网页服务 -> MySQL
rem  先断入口再停数据，避免请求打在半关闭的服务上。
rem ===========================================================================

set "ROOT=%~dp0.."
pushd "%ROOT%"
set "ROOT=%CD%"
popd

echo.
echo ==============================================================
echo  OKX 全合约量化终端 · 停止
echo ==============================================================
echo.

echo [1/3] 停止 Apache (OKXApache) ...
net stop OKXApache >nul 2>&1
if errorlevel 1 (echo       未在运行。) else (echo       已停止。)

echo.
echo [2/3] 停止 Go 网页服务 (okxweb.exe) ...
taskkill /F /IM okxweb.exe >nul 2>&1
if errorlevel 1 (echo       未在运行。) else (echo       已停止。)

echo.
echo [3/3] 停止 MySQL (OKXMySQL) ...
net stop OKXMySQL >nul 2>&1
if errorlevel 1 (echo       未在运行。) else (echo       已停止。)

echo.
echo ==============================================================
echo  全部停止。
echo ==============================================================
echo.
pause
