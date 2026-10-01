@echo off
chcp 936 >nul
setlocal enabledelayedexpansion
REM ===========================================================================
REM  push_github.bat —— 把代码仓和 K 线归档仓推到 GitHub
REM
REM  用法：
REM    scripts\push_github.bat            两个仓都推
REM    scripts\push_github.bat code       只推代码仓
REM    scripts\push_github.bat data       只推归档仓
REM    scripts\push_github.bat check      只检查凭据与远端，不推
REM
REM  凭据从哪来（按优先级）：
REM    1) %ROOT%\.git-token    —— 文件里只放一行 Personal Access Token
REM    2) 环境变量 GITHUB_TOKEN
REM  两者都没有就直接退出（退出码 2），绝不会卡在交互式密码提示上。
REM
REM  为什么用「带 token 的临时 URL」而不是写进 .git/config：
REM    git push https://x-access-token:TOKEN@github.com/owner/repo.git
REM    这样 token 不会落到本地 git 配置文件里，避免误提交或被别的工具读到。
REM ===========================================================================

set "ROOT=%~dp0.."
REM 归一化：把 scripts\.. 折叠成真实路径，日志里好看也好排查
for %%I in ("%ROOT%") do set "ROOT=%%~fI"

set "CODE_REPO=github.com/newusually/okx-quant-terminal.git"
set "DATA_REPO=github.com/newusually/okx-quant-terminal-data.git"
set "TOKENFILE=%ROOT%\.git-token"

REM ---- 取 token ----
set "TOKEN="
if exist "%TOKENFILE%" (
  for /f "usebackq delims=" %%T in ("%TOKENFILE%") do (
    if not defined TOKEN set "TOKEN=%%T"
  )
)
if not defined TOKEN if defined GITHUB_TOKEN set "TOKEN=%GITHUB_TOKEN%"

if not defined TOKEN (
  echo [X] 找不到 GitHub 凭据。
  echo     请把 Personal Access Token ^(勾选 repo 权限^) 存成一行放进：
  echo       %TOKENFILE%
  echo     或者设置环境变量 GITHUB_TOKEN。
  exit /b 2
)

REM ---- 统一日期时间（%DATE% 在中文 Windows 下是「2026/10/01 周四」）------
REM   直接拼进 git 提交信息会带上斜杠和星期，既难看又无法按字符串排序。
for /f "tokens=1-3 delims=/ " %%a in ("%DATE%") do set "DASH=%%a-%%b-%%c"
if not defined DASH set "DASH=%DATE%"
set "CLK=%TIME:~0,5%"
set "CLK=%CLK: =0%"

if /i "%~1"=="check" goto :check

REM ---- 代码仓 ----
if /i "%~1"=="" goto :pushcode
if /i "%~1"=="code" goto :pushcode
goto :skipcode

:pushcode
call :pushone "%ROOT%" "%CODE_REPO%" "code"
if errorlevel 1 set "FAILED=1"
:skipcode

REM ---- 归档仓 ----
if /i "%~1"=="" goto :pushdata
if /i "%~1"=="data" goto :pushdata
goto :done

:pushdata
REM 归档数据仓是独立仓库（archive\.git），主仓库用 .gitignore 排除了它
if exist "%ROOT%\archive\.git" (
  call :pushone "%ROOT%\archive" "%DATA_REPO%" "data"
  if errorlevel 1 set "FAILED=1"
) else (
  echo [SKIP] 归档仓还没初始化：%ROOT%\archive\.git 不存在
)
goto :done

:check
echo == 凭据检查 ==
echo   token 长度 : 已读取（不回显内容）
echo   代码仓     : %CODE_REPO%
echo   归档仓     : %DATA_REPO%
echo   ROOT       : %ROOT%
echo.
echo == 远端可达性 ==
git ls-remote "https://x-access-token:%TOKEN%@%CODE_REPO%" HEAD >nul 2>&1
if errorlevel 1 (echo   code  ^>^> × 推不通（token 无效 / 无权限 / 仓库不存在）) else (echo   code  ^>^> OK)
git ls-remote "https://x-access-token:%TOKEN%@%DATA_REPO%" HEAD >nul 2>&1
if errorlevel 1 (echo   data  ^>^> × 推不通（多半是数据仓还没建）) else (echo   data  ^>^> OK)
exit /b 0

:done
if defined FAILED (
  echo.
  echo [X] 有仓库推送失败，退出码 1（月度维护会记下 PushErr 并跳过后续破坏性动作）
  endlocal
  exit /b 1
)
endlocal
exit /b 0

REM ---------------------------------------------------------------------------
REM :pushone  目录 / 远端 / 标签
REM ---------------------------------------------------------------------------
:pushone
setlocal
set "DIR=%~1"
set "REPO=%~2"
set "TAG=%~3"
cd /d "%DIR%" || (echo [X] 进不去 %DIR% & exit /b 1)

REM ★ 取当前分支名：必须整行取（delims=）或 tokens=1。
REM   `git rev-parse --abbrev-ref HEAD` 只输出一行分支名（1 个 token），
REM   用 tokens=2 会取到空串 —— 实测 [tokens=2] -> [] / [tokens=1] -> [master]，
REM   也就是说 BRANCH 一直是靠下面那行 hardcode 兜底蒙对的；仓库改名 main
REM   之后就会静默推到错误的分支名。
REM   symbolic-ref --short HEAD 更稳，分离头指针时它会失败，兜底才生效。
for /f "delims=" %%B in ('git symbolic-ref --short HEAD 2^>nul') do set "BRANCH=%%B"
if not defined BRANCH for /f "tokens=1" %%B in ('git rev-parse --abbrev-ref HEAD 2^>nul') do set "BRANCH=%%B"
if not defined BRANCH set "BRANCH=master"

git add -A
git diff --cached --quiet
if errorlevel 1 (
  git -c user.name="okx-quant" -c user.email="okx-quant@localhost" commit -q -m "auto(%TAG%): %DASH% %CLK%"
  if errorlevel 1 (echo [X] %TAG% 提交失败 & exit /b 1)
  echo [OK] %TAG% 已提交
) else (
  echo [--] %TAG% 无新增改动
)

REM ★ 推送成败必须取 git 自己的退出码。
REM   不能写成 `git push ... | findstr ...` 再判 errorlevel ——
REM   管道里的 errorlevel 是 findstr 的，而 `findstr /v` 只要打出任意一行
REM   不含模式的内容就返回 0，推送失败时 git 的错误信息恰好全属于这类，
REM   于是失败会被报成「[OK] 已推送」（实测踩到过）。
REM   所以：先重定向到临时文件 -> 立刻取退出码 -> 再过滤打印 -> 最后删文件。
set "OUT=%TEMP%\okxpush_%TAG%.log"
git push "https://x-access-token:%TOKEN%@%REPO%" HEAD:%BRANCH% > "%OUT%" 2>&1
set "RC=!ERRORLEVEL!"
REM 过滤掉可能含 token 的行再打印（git 出错时有可能回显带 token 的 URL）
%SystemRoot%\System32\findstr.exe /v "x-access-token" "%OUT%"
del "%OUT%" >nul 2>&1
if not "!RC!"=="0" (
  echo [X] %TAG% 推送失败（git 退出码 !RC!）
  exit /b 1
)
echo [OK] %TAG% 已推送到 %REPO% ^(%BRANCH%^)
exit /b 0
