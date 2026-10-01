@echo off
chcp 936 >nul
setlocal
REM ===========================================================================
REM  push_now.bat —— 一键把代码仓 + K 线归档仓推到 GitHub（双击运行）
REM
REM  这是 push_github.bat 的双击封装：跑完不闪退，会停下来告诉你结果。
REM
REM  区分：
REM    push_github.bat  被服务调用时的入口，输出精简、退出码有语义
REM    push_now.bat     给人双击用的，跑完 pause 住，人话总结
REM ===========================================================================

set "ROOT=%~dp0.."
for %%I in ("%ROOT%") do set "ROOT=%%~fI"
set "TOKENFILE=%ROOT%\.git-token"

echo ============================================================
echo   OKX 量化终端 -- 一键推送到 GitHub
echo ============================================================
echo   仓库根目录 : %ROOT%
echo.

if not exist "%TOKENFILE%" (
  echo [X] 还没设置凭据：找不到 %TOKENFILE%
  echo.
  echo     请先双击  scripts\set_token.bat  粘贴一次 token（只需一次，永久有效）。
  echo.
  pause
  exit /b 2
)

call "%~dp0push_github.bat"
set "RC=%ERRORLEVEL%"

echo.
echo ============================================================
if "%RC%"=="0"  goto :ok
if "%RC%"=="2"  goto :nocred
goto :fail

:ok
echo   全部推送成功。
echo ============================================================
echo.
pause
exit /b 0

:nocred
echo   没有凭据 —— 请先双击 scripts\set_token.bat
echo ============================================================
echo.
pause
exit /b 2

:fail
echo   推送失败（退出码 %RC%）—— 看上面带 [X] 的行。
echo.
echo   常见原因：
echo     token 过期 / 没勾 repo 权限 / 断网 / 仓库名下账号不对
echo ============================================================
echo.
pause
exit /b 1
