@echo off
chcp 936 >nul
setlocal enabledelayedexpansion
REM ===========================================================================
REM  set_token.bat —— 傻瓜式设置 GitHub 推送凭据（双击运行即可）
REM
REM  做完四件事：
REM    1) 把粘贴进来的 token 写进  %ROOT%\.git-token   （已被 .gitignore 排除）
REM    2) 用 GitHub API 验证 token 真伪，并问出你的真实账号名，写进
REM       %ROOT%\.git-owner   （推送地址里的 owner 以这个为准，不依赖硬编码）
REM    3) 如果数据仓 okx-quant-terminal-data 还不存在，自动帮你建一个私有仓
REM    4) 跑一次 push_github.bat check，把结论用人话讲清楚
REM
REM  为什么不用「写进 .git/config」或「设环境变量」：
REM    写 .git/config 会让 token 落到仓库文件里，容易误提交；
REM    设环境变量要重启资源管理器才生效，而且这台机器是服务在跑，读不到。
REM    单独一个被 ignore 的文件最省事 —— push 时用临时 URL 携带 token，
REM    不落盘、不进 remote 配置。
REM
REM  ★ 两个本机踩过的坑，改动前请先看：
REM    A. for /f 的 in(...) 里给命令加引号会触发 cmd 的「行首引号」陷阱，
REM       实测 '"C:\Windows\System32\findstr.exe" ...' 报
REM       「文件名、目录名或卷标语法不正确。」且取到空值。绝对路径不加引号。
REM    B. 用 git ls-remote 验 token 是假的判据 —— 公开仓库匿名也能读到 HEAD，
REM       假 token 照样返回 0。必须用 API /user 看 HTTP 码（无效 token = 401）。
REM ===========================================================================

set "ROOT=%~dp0.."
for %%I in ("%ROOT%") do set "ROOT=%%~fI"
set "TOKENFILE=%ROOT%\.git-token"
set "OWNERFILE=%ROOT%\.git-owner"
set "CURL=%SystemRoot%\System32\curl.exe"
set "FINDSTR=%SystemRoot%\System32\findstr.exe"
set "AJSON=%TEMP%\okx_gh_user.json"

echo ============================================================
echo   OKX 量化终端 -- 设置 GitHub 推送凭据
echo ============================================================
echo.
echo 请先在浏览器里做这 5 步（浏览器要已经登录 GitHub）：
echo.
echo   1. 打开这个网址（经典 token 页面，勾一个大框就够，最省事）：
echo.
echo        https://github.com/settings/tokens/new
echo.
echo   2. Note 随便填，比如   okx-quant-upload
echo      Expiration 选        No expiration
echo.
echo   3. 勾选最上面那个大框   [ ] repo
echo      （勾了它，下面 5 个子项会自动全选）
echo.
echo   4. 拉到页面最下面，点绿色的   Generate token
echo.
echo   5. 页面上会出现 ghp_ 开头的一长串字符，
echo      点它右边的复制按钮。（离开这个页面就再也看不到了）
echo.
echo ------------------------------------------------------------
echo   复制好之后，在这个黑窗口里点一下【鼠标右键】就是粘贴，
echo   或者按 Ctrl+V。粘好以后按回车。
echo ------------------------------------------------------------
echo.

set "TOKEN="
set /p "TOKEN=请粘贴 token 然后按回车: "

if not defined TOKEN (
  echo.
  echo [X] 什么都没输入。请重新双击本文件再来一次。
  echo.
  pause
  exit /b 1
)

REM 去掉粘贴时可能带上的引号 / 首尾空格
set "TOKEN=!TOKEN:"=!"
for /f "tokens=* delims= " %%A in ("!TOKEN!") do set "TOKEN=%%A"

if not defined TOKEN (
  echo.
  echo [X] 清洗之后是空的，请重新双击本文件再来一次。
  echo.
  pause
  exit /b 1
)

REM ---- 先用 API 验 token 真伪，验不过就不写文件 ----
REM   不写进去是因为：留一个坏 token 在盘上，推送会静默失败，
REM   而 cleanup.go 又拿推送结果决定要不要删本地归档，风险不划算。
set "HCU="
if exist "%CURL%" (
  for /f %%C in ('%CURL% -s -o "%AJSON%" -w "%%{http_code}" -H "Authorization: Bearer !TOKEN!" https://api.github.com/user') do set "HCU=%%C"
)

if not "!HCU!"=="200" (
  if exist "%AJSON%" del "%AJSON%" >nul 2>&1
  echo.
  if "!HCU!"=="000" (
    echo [X] 连不上 api.github.com，没法验证 token。
    echo     先确认这台机器能上网，然后重新双击本文件。
  ) else (
    echo [X] token 验证失败（HTTP !HCU!）。
    echo     401 = 这串 token 无效或已过期 —— 最常见的原因是复制时少了字符。
  )
  echo.
  echo     没有写入任何东西，请重新双击本文件再来一次。
  echo.
  pause
  exit /b 1
)

REM ---- token 有效：抠出账号名 ----
set "LOGIN="
for /f "tokens=2 delims=:," %%A in ('%FINDSTR% /c:login "%AJSON%"') do if not defined LOGIN set "LOGIN=%%A"
if exist "%AJSON%" del "%AJSON%" >nul 2>&1
if defined LOGIN set "LOGIN=!LOGIN:"=!"
if defined LOGIN for /f "tokens=* delims= " %%B in ("!LOGIN!") do set "LOGIN=%%B"

if not defined LOGIN (
  echo.
  echo [!] token 有效，但没能解析出账号名。
  echo     请把下面这行手工写进 %OWNERFILE%
  echo     内容就是你 GitHub 用户名，另外别忘了 token 也要存：
  echo       echo 你的用户名^> "%OWNERFILE%"
  echo.
)

REM ---- 落盘 ----
>"%TOKENFILE%" echo !TOKEN!
echo.
echo [OK] token 已验证通过，并保存到  %TOKENFILE%
if defined LOGIN (
  >"%OWNERFILE%" echo !LOGIN!
  echo [OK] GitHub 账号识别为 : !LOGIN!
  echo      已写入  %OWNERFILE%
)

REM ---- 数据仓不存在就自动创建私有仓 ----
if defined LOGIN (
  set "HCD="
  for /f %%C in ('%CURL% -s -o nul -w "%%{http_code}" -H "Authorization: Bearer !TOKEN!" https://api.github.com/repos/!LOGIN!/okx-quant-terminal-data') do set "HCD=%%C"
  if "!HCD!"=="200" (
    echo [OK] 数据仓 okx-quant-terminal-data 已存在
  ) else (
    echo.
    echo [..] 数据仓 okx-quant-terminal-data 还不存在，正在自动创建私有仓 ...
    "%CURL%" -s -X POST -H "Authorization: Bearer !TOKEN!" -H "Accept: application/vnd.github+json" "https://api.github.com/user/repos" -d "{\"name\":\"okx-quant-terminal-data\",\"private\":true,\"description\":\"OKX 15m K line monthly archive\"}" -o nul -w "     创建返回码 %%{http_code}\n"
    echo      ^(201 = 建好了，422 = 已存在^)
  )
)

REM ---- 自检 ----
echo.
echo == 最终自检 =================================================
echo.
call "%~dp0push_github.bat" check
set "RC=!ERRORLEVEL!"
echo.
echo ============================================================
if "!RC!"=="0" (
  echo  搞定。凭据存好了，两个仓库都验通了。
  echo.
  echo  下一步 —— 双击  scripts\push_now.bat  即可一键上传。
) else (
  echo  还差一点（退出码 !RC!），请看上面的 [X] 行。
  echo.
  echo  常见情况：
  echo    - token 401：复制不全，重新生成一个再粘一次
  echo    - code 看不到仓库：GitHub 用户名和 %OWNERFILE% 里的不一致
  echo    - data 看不到仓库：上面自动建仓那步失败了（多半是 token 没勾 repo）
)
echo ============================================================
echo.
echo 这个窗口可以直接关掉。token 已经存在文件里了，以后不用再设。
echo.
pause
endlocal
exit /b 0
