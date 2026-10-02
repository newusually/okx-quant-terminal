@echo off
chcp 936 >nul
setlocal enabledelayedexpansion
REM ===========================================================================
REM  check_services.bat -- 三个服务的健康巡检（排障用，也可挂计划任务）
REM
REM  用法：
REM    scripts\check_services.bat          看一眼，有问题就打印怎么修
REM    scripts\check_services.bat fix      有服务没跑就按序拉起来
REM    scripts\check_services.bat mail     额外验一次验证码邮件链路
REM
REM  退出码：0 = 三服务全在跑且 8090 有响应   1 = 有异常
REM
REM  ★ 为什么需要这个脚本（2026-10-02 的真实教训）★
REM    用户反馈「网页点发送验证码收不到邮件」。排查发现：
REM    ① 服务不是「配错了 SMTP」，而是**进程根本没在跑**
REM       （80/8090/3306 全无监听）。网页按钮点下去请求都到不了后端。
REM    ② 服务由 SCM 拉起时**没有控制台**，stdout 被系统丢弃 ——
REM       日志里既没有「已就绪」也没有「未配置」，
REM       翻完 1.6 万行也看不出服务到底活没活。
REM    这个脚本就是给「看不到控制台」补的一双眼睛。
REM
REM  ★ 判据为什么是端口而不是 net start 的列表 ★
REM    Windows 服务有「已启动但进程已死」的状态残留（SCM 谎报），
REM    net start 列出来不代表真在服务。端口监听是进程活着的硬证据。
REM ===========================================================================

set "ROOT=%~dp0.."
for %%I in ("%ROOT%") do set "ROOT=%%~fI"
set "FINDSTR=%SystemRoot%\System32\findstr.exe"
set "CURL=%SystemRoot%\System32\curl.exe"

set "BAD=0"
echo == 端口监听（硬证据）==
call :chkport 3306 "MySQL"
call :chkport 8090 "Web(okxweb)"
call :chkport 80   "Apache"

echo.
echo == 接口响应 ==
set "HC="
for /f %%C in ('%CURL% -s -o nul -w "%%{http_code}" --max-time 5 http://127.0.0.1:8090/api/admin/session 2^>nul') do set "HC=%%C"
if "!HC!"=="200" goto :api_ok
echo   8090 /api/admin/session  ^>^> [X] !HC! ^(expected 200^)
set "BAD=1"
goto :api_done
:api_ok
echo   8090 /api/admin/session  ^>^> [OK] 200
:api_done

echo.
echo == SMTP 就绪情况（服务没控制台，只能读日志）==
set "LOG=%ROOT%\logs\okxbot.log"
if not exist "!LOG!" goto :nolog
%FINDSTR% /C:"[MAIL] " "!LOG!" >nul 2>&1
if errorlevel 1 goto :nomail
echo   [OK] 日志里有过 [MAIL] 就绪记录。
echo        查看最后一条（日志是 UTF-8，不在 cmd 里回显原文以免花屏）：
echo          findstr /C:"[MAIL] " "!LOG!"
goto :mail_done
:nomail
echo   [X] 日志里找不到 SMTP 就绪记录 -- 管理台登录会不可用
echo       检查 configs\.smtp-pass 是否存在（内容：邮箱:授权码）
set "BAD=1"
goto :mail_done
:nolog
echo   [X] 日志不存在：!LOG!
set "BAD=1"
:mail_done

REM ---- 可选：真发一封验证码验链路 ----
if /i not "%~1"=="mail" goto :skip_mail
echo.
echo == 实发验证码（会占用 60 秒冷却）==
set "RESP="
for /f "delims=" %%R in ('%CURL% -s --max-time 20 -X POST http://127.0.0.1:8090/api/admin/send_code -H "Content-Type: application/json" -d "{\"email\":\"493076373@qq.com\"}" 2^>nul') do set "RESP=%%R"
echo   resp = !RESP!
:skip_mail

REM ---- 可选：有异常就拉起来 ----
if /i not "%~1"=="fix" goto :skip_fix
if not "!BAD!"=="1" goto :skip_fix
echo.
echo == 按序拉起三个服务 ==
echo   铁律：MySQL 先起（Web 依赖它），Apache 最后（反代 Web）
net start OKXMySQL >nul 2>&1
if errorlevel 1 (echo   [!] OKXMySQL 返回非 0，可能已在跑) else (echo   [OK] OKXMySQL)
net start OKXWeb >nul 2>&1
if errorlevel 1 (echo   [!] OKXWeb 返回非 0，可能已在跑) else (echo   [OK] OKXWeb)
net start OKXApache >nul 2>&1
if errorlevel 1 (echo   [!] OKXApache 返回非 0，可能已在跑) else (echo   [OK] OKXApache)
echo   等 8 秒让 okxweb 完成初始化...
ping -n 9 127.0.0.1 >nul
echo   复检端口 8090：
netstat -ano | %FINDSTR% ":8090" | %FINDSTR% "LISTENING"
:skip_fix

echo.
if "!BAD!"=="1" goto :bad_end
echo [OK] 三服务健康，接口有响应。
endlocal
exit /b 0
:bad_end
echo [X] 有异常 -- 加 fix 参数可自动拉起：scripts\check_services.bat fix
endlocal
exit /b 1

REM ---------------------------------------------------------------------------
REM  :chkport  端口号 显示名
REM
REM  ★ 为什么用 goto 平铺而不是 if(...)else(...) 块 ★
REM    cmd 解析 if 块时先按圆括号配对再执行。块内 echo 的参数里一旦出现
REM    未转义的半角右括号，就会提前闭合整个块 —— 实测第一个端口印完就中断，
REM    后面两个端口根本没检查，症状只是一句「此时不应有 ...」。
REM    两段平铺 + goto 彻底绕开块解析。
REM ---------------------------------------------------------------------------
:chkport
set "P=%~1"
set "N=%~2"
netstat -ano | %FINDSTR% ":%P% " | %FINDSTR% "LISTENING" >nul 2>&1
if errorlevel 1 goto :chkport_bad
echo   %N% [port %P%]  ^>^> [OK] listening
exit /b 0
:chkport_bad
echo   %N% [port %P%]  ^>^> [X] NOT listening
set "BAD=1"
exit /b 0
