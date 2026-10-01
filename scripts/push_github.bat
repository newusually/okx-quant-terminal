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

if /i "%~1"=="check" goto :check

REM ---- 代码仓 ----
if /i "%~1"=="" goto :pushcode
if /i "%~1"=="code" goto :pushcode
goto :skipcode

:pushcode
call :pushone "%ROOT%" "%CODE_REPO%" "code"
:skipcode

REM ---- 归档仓 ----
if /i "%~1"=="" goto :pushdata
if /i "%~1"=="data" goto :pushdata
goto :done

:pushdata
if exist "%ROOT%\archive\.git" (
  call :pushone "%ROOT%\archive" "%DATA_REPO%" "data"
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

for /f "tokens=2" %%B in ('git rev-parse --abbrev-ref HEAD 2^>nul') do set "BRANCH=%%B"
if not defined BRANCH set "BRANCH=master"

git add -A
git diff --cached --quiet
if errorlevel 1 (
  git -c user.name="okx-quant" -c user.email="okx-quant@localhost" commit -q -m "auto(%TAG%): %DATE% %TIME%"
  if errorlevel 1 (echo [X] %TAG% 提交失败 & exit /b 1)
  echo [OK] %TAG% 已提交
) else (
  echo [--] %TAG% 无新增改动
)

git push "https://x-access-token:%TOKEN%@%REPO%" HEAD:%BRANCH% 2>&1 | %SystemRoot%\System32\findstr.exe /v "x-access-token" 
if errorlevel 1 (
  echo [X] %TAG% 推送失败
  exit /b 1
)
echo [OK] %TAG% 已推送到 %REPO% ^(%BRANCH%^)
exit /b 0
