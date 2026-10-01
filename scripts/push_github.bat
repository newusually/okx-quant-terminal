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
REM  ★ 给人双击用的话请走 scripts\push_now.bat —— 那个跑完会 pause 住
REM    并用人话总结，这个脚本是给服务调用和命令行用的。
REM
REM  凭据从哪来（按优先级）：
REM    1) %ROOT%\.git-token    —— 文件里只放一行 Personal Access Token
REM    2) 环境变量 GITHUB_TOKEN
REM  两者都没有就直接退出（退出码 2），绝不会卡在交互式密码提示上。
REM  ★ 不知道怎么弄 token：双击 scripts\set_token.bat，粘贴一次就行。
REM
REM  账号名（owner）从哪来：
REM    %ROOT%\.git-owner  没有这个文件时退回默认值 newusually。
REM    该文件由 set_token.bat 用 GitHub API 自动写入，所以换了账号也不用改脚本。
REM
REM  退出码约定（月度维护按这个决定要不要做破坏性动作）：
REM    0 = 全部成功      1 = 有仓库推送失败      2 = 没有凭据      3 = 没有 git
REM
REM  ---------------------------------------------------------------------------
REM  ★ 本机踩过的坑，改这个文件前务必先读这两条：
REM
REM  A. git 不在系统 PATH 里
REM     这台机器唯一的 git 是 WorkBuddy 自带的 PortableGit
REM     （%USERPROFILE%\.workbuddy\binaries\PortableGit\versions\1.2.0），
REM     **没有**进系统 PATH。从 bash 里跑 cmd 会继承 bash 的 PATH 所以测不出来，
REM     用户双击时 cmd 直接报「'git' 不是内部或外部命令」，
REM     然后 add / commit / push 全挂，但代码里只看到「提交失败」，看不出根因。
REM     这里改成先探测再把它塞进 PATH。
REM
REM  B. curl / findstr 一律用绝对路径，且**在 for /f 的 in(...) 里不加引号**
REM     本机 PATH 里有 GNU 的 curl / findstr 同名工具，不加绝对路径会调错；
REM     而给命令加引号会触发 cmd 的「行首引号」陷阱 —— 实测
REM     for /f ... in ('"C:\Windows\System32\findstr.exe" ...')
REM     直接报「文件名、目录名或卷标语法不正确。」且结果为空。
REM     %SystemRoot% 展开后不含空格，所以不加引号是安全的。
REM  ---------------------------------------------------------------------------
REM ===========================================================================

set "ROOT=%~dp0.."
REM 归一化：把 scripts\.. 折叠成真实路径，日志里好看也好排查
for %%I in ("%ROOT%") do set "ROOT=%%~fI"

set "TOKENFILE=%ROOT%\.git-token"
set "OWNERFILE=%ROOT%\.git-owner"
set "CURL=%SystemRoot%\System32\curl.exe"
set "FINDSTR=%SystemRoot%\System32\findstr.exe"
set "WHERE=%SystemRoot%\System32\where.exe"

REM ---- 定位 git.exe，并把它的目录塞到 PATH 最前面 ----
REM   顺序：PATH 里现成的 -> 常见安装位置 -> WorkBuddy 自带 PortableGit
set "GITDIR="
for /f "delims=" %%G in ('%WHERE% git 2^>nul') do if not defined GITDIR set "GITDIR=%%~dpG"
if not defined GITDIR if exist "%ProgramFiles%\Git\cmd\git.exe" set "GITDIR=%ProgramFiles%\Git\cmd\"
if not defined GITDIR if exist "%ProgramFiles(x86)%\Git\cmd\git.exe" set "GITDIR=%ProgramFiles(x86)%\Git\cmd\"
if not defined GITDIR if exist "%LOCALAPPDATA%\Programs\Git\cmd\git.exe" set "GITDIR=%LOCALAPPDATA%\Programs\Git\cmd\"
if not defined GITDIR if exist "C:\Git\cmd\git.exe" set "GITDIR=C:\Git\cmd\"
if not defined GITDIR for /d %%D in ("%USERPROFILE%\.workbuddy\binaries\PortableGit\versions\*") do (
  if not defined GITDIR if exist "%%~fD\cmd\git.exe" set "GITDIR=%%~fD\cmd\"
)
if not defined GITDIR for /d %%D in ("%USERPROFILE%\.workbuddy\binaries\PortableGit\versions\*") do (
  if not defined GITDIR if exist "%%~fD\mingw64\bin\git.exe" set "GITDIR=%%~fD\mingw64\bin\"
)
if defined GITDIR set "PATH=%GITDIR%;%PATH%"

REM ★ 下面这段刻意写成 goto 平铺，而不是 if errorlevel 1 ( ... ) 块：
REM   块内任何 echo 只要含**未转义的半角 )**，就会提前闭合整个块，
REM   后面的行全部变成 "xxx was unexpected at this time"。
REM   实测 echo ... %%ProgramFiles(x86)%%\Git\cmd 直接报「此时不应有 %\Git\cmd。」
REM   —— 而这段本来正好是「找不到 git 时」才走的路径，
REM   于是「报错信息本身也报错」，用户看到的就是一句莫名其妙的语法错误。
git --version >nul 2>&1
if not errorlevel 1 goto :gitok
echo [X] 找不到可用的 git.exe。
echo     已经找过这些地方：
echo       - 系统 PATH
echo       - %ProgramFiles%\Git\cmd
echo       - %ProgramFiles(x86)%\Git\cmd
echo       - %LOCALAPPDATA%\Programs\Git\cmd
echo       - C:\Git\cmd
echo       - %USERPROFILE%\.workbuddy\binaries\PortableGit\versions\ 下的各版本\cmd
echo     本机已知可用的是最后那个（WorkBuddy 自带的 PortableGit）。
echo     实在没有就装一个：https://git-scm.com/download/win
exit /b 3
:gitok

REM ---- 解析 owner ----
set "OWNER="
if exist "%OWNERFILE%" for /f "usebackq delims=" %%O in ("%OWNERFILE%") do if not defined OWNER set "OWNER=%%O"
if not defined OWNER set "OWNER=newusually"

set "CODE_REPO=github.com/%OWNER%/okx-quant-terminal.git"
set "DATA_REPO=github.com/%OWNER%/okx-quant-terminal-data.git"

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
  echo     最省事的做法：双击 scripts\set_token.bat 粘贴一次 token。
  echo     ^(手动也行：把 Personal Access Token 勾选 repo 权限后存成一行放进
  echo      %TOKENFILE%
  echo      或者设置环境变量 GITHUB_TOKEN。^)
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

REM ===========================================================================
REM  :check —— 只验凭据，不推
REM
REM  ★ 为什么必须走 GitHub API，不能再用 git ls-remote 验 token：
REM    okx-quant-terminal 是**公开**仓库，匿名请求也能 ls-remote 拿到 HEAD，
REM    URL 里塞一个假 token 照样返回 0 —— 实测假 token 被报成「[OK] 通」。
REM    而 cleanup.go 是拿这个退出码决定要不要删本地 archive/ 的，
REM    验错了就是「本地删了、远端也没上去」的数据事故。
REM    API /user 对无效 token 明确回 401，这才是有区分度的判据。
REM ===========================================================================
:check
echo == 凭据与地址 ==
echo   git        : 已定位（%GITDIR%）
echo   token      : 已读取（不回显内容）
echo   账号 owner : %OWNER%   ^<- 取自 .git-owner，没有该文件时默认 newusually
echo   代码仓     : %CODE_REPO%
echo   归档仓     : %DATA_REPO%
echo   ROOT       : %ROOT%
echo.

set "BAD=0"

echo == (1) token 本身是否有效 ==================================
set "HCU="
for /f %%C in ('%CURL% -s -o nul -w "%%{http_code}" -H "Authorization: Bearer %TOKEN%" https://api.github.com/user') do set "HCU=%%C"
if "%HCU%"=="200" (
  echo   token  ^>^> [OK] 有效，GitHub 认这个凭据
) else (
  if "%HCU%"=="000" (
    echo   token  ^>^> [?] 连不上 api.github.com（网络问题），判不了
    echo            这不代表 token 有问题，可以直接试推送
  ) else (
    if "%HCU%"=="401" (
      echo   token  ^>^> [X] HTTP 401 —— token 无效或已过期
      echo            多半是复制时少了字符，或生成后又被删掉了
    ) else (
      echo   token  ^>^> [X] HTTP %HCU% —— 判不了
      echo            403 通常是 API 限流，隔几分钟再试
    )
    set "BAD=1"
  )
)

echo == (2) 仓库能不能看到 ======================================
if not "%HCU%"=="200" (
  echo   code  ^>^> [-] 跳过（token 没验过，验了也不准）
  echo   data  ^>^> [-] 跳过
  goto :checkdone
)

set "HCC="
for /f %%C in ('%CURL% -s -o nul -w "%%{http_code}" -H "Authorization: Bearer %TOKEN%" https://api.github.com/repos/%OWNER%/okx-quant-terminal') do set "HCC=%%C"
if "!HCC!"=="200" (
  echo   code  ^>^> [OK] 可见：%OWNER%/okx-quant-terminal
) else (
  echo   code  ^>^> [X] HTTP !HCC! —— 看不到这个仓库
  echo            owner 拼错了？账号名是 %OWNER%，可在 .git-owner 里改
  set "BAD=1"
)

set "HCD="
for /f %%C in ('%CURL% -s -o nul -w "%%{http_code}" -H "Authorization: Bearer %TOKEN%" https://api.github.com/repos/%OWNER%/okx-quant-terminal-data') do set "HCD=%%C"
if "!HCD!"=="200" (
  echo   data  ^>^> [OK] 可见：%OWNER%/okx-quant-terminal-data
) else (
  echo   data  ^>^> [X] HTTP !HCD! —— 看不到这个仓库
  echo            这是 K 线归档的数据仓，跟代码仓分开建。
  echo            最快办法：重新双击 scripts\set_token.bat，它会自动建。
  set "BAD=1"
)

:checkdone
echo.
if "!BAD!"=="1" (
  echo [X] 自检没通过 —— 先别推，按上面提示修。
  exit /b 1
)
echo [OK] 自检通过：凭据有效、仓库可见，可以推送。
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

REM 顺手补一个不带 token 的 origin，方便人手工 git fetch / git log。
REM ★ 这里必须看 errorlevel：上一版把它吞进 >nul 之后就无条件打印「已补配」，
REM   结果 git 根本不存在时也报成功 —— 和之前 findstr 那次是同一类假成功。
git remote get-url origin >nul 2>&1
if errorlevel 1 (
  git remote add origin "https://%REPO%" >nul 2>&1
  if errorlevel 1 (
    echo [!] %TAG% 补配 origin 失败（不影响推送，推送走临时 URL）
  ) else (
    echo [..] %TAG% 已补配 origin = https://%REPO% ^(不含 token^)
  )
)

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
  REM ★ 提交信息用方括号而不是圆括号：这一行在 if (...) 块内，
  REM   圆括号会提前闭合块 —— 实测 auto(%TAG%) 直接报
  REM   「) was unexpected at this time」，效果是「代码仓永远提交不上」，
  REM   而症状只表现为一句语法错误，很难联想到是提交信息里的字符。
  git -c user.name="okx-quant" -c user.email="okx-quant@localhost" commit -q -m "auto[%TAG%]: %DASH% %CLK%"
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
%FINDSTR% /v "x-access-token" "%OUT%"
del "%OUT%" >nul 2>&1
if not "!RC!"=="0" (
  echo [X] %TAG% 推送失败（git 退出码 !RC!）
  exit /b 1
)
echo [OK] %TAG% 已推送到 %REPO% ^(%BRANCH%^)
exit /b 0
