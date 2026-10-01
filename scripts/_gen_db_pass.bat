@echo off
rem ===========================================================================
rem  OKX 全合约量化终端 —— 生成随机 MySQL 口令（内部 helper，不要单独双击）
rem  ---------------------------------------------------------------------------
rem  用法：先 set "ROOT=<项目根>"，再  call "%ROOT%\scripts\_gen_db_pass.bat"
rem    成功 → 退出码 0，%MYSQL_PASS% 是新口令，且已写进 <项目根>\.mysql-pass
rem    失败 → 退出码 1，%MYSQL_PASS% 为空
rem
rem  ★ 本文件同样**故意不加 setlocal**（变量要留给调用方）。
rem
rem  ★ 口令经由临时文件传递，不走 stdout 捕获 ★
rem    powershell.exe 启动失败时会往 stdout 打一屏横幅，而 for /f 会把它
rem    当命令输出逐行 set 进变量 —— 于是"随机口令"变成一句广告词。
rem    改成「写文件」后，「没有文件」就是明确的失败信号。实测踩过。
rem ===========================================================================

set "MYSQL_PASS="

set "OPN=%TEMP%\okx_newpass_%RANDOM%%RANDOM%.tmp"
del "%OPN%" >nul 2>&1

powershell -NoProfile -ExecutionPolicy Bypass -File "%ROOT%\scripts\_newpass.ps1" -OutFile "%OPN%" >nul 2>&1

if not exist "%OPN%" (
    echo       [错误] 生成随机口令失败（PowerShell 没跑起来或执行策略被拦）。
    echo              可以改成手工指定：scripts\set_db_pass.bat --pass 你的口令
    exit /b 1
)

set /p MYSQL_PASS=<"%OPN%"
del "%OPN%" >nul 2>&1

if not defined MYSQL_PASS (
    echo       [错误] 生成的口令是空的。
    exit /b 1
)

>"%ROOT%\.mysql-pass" echo %MYSQL_PASS%
if not exist "%ROOT%\.mysql-pass" (
    echo       [错误] 写 "%ROOT%\.mysql-pass" 失败（目录只读？）。
    exit /b 1
)

exit /b 0
