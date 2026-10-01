@echo off
chcp 936 >nul 2>&1
title OKX 终端 - 设置 MySQL 口令
setlocal EnableExtensions

rem ===========================================================================
rem  OKX 全合约量化终端 —— MySQL 口令：设置 / 轮换 向导
rem  ---------------------------------------------------------------------------
rem  为什么需要它：
rem    2026-10-01 之前，MySQL 口令（'OkxQuant****'）硬编码在 4 个 .go 源文件 +
rem    3 个批处理里，而仓库是 public —— 等于口令连同代码一起公开。
rem    源码已改成「运行时从环境变量 / 密钥文件读」，但**改代码不会让旧口令失效**：
rem    历史提交里那把口令至今仍然能连上。要真正堵住，必须把它换掉。
rem    本脚本就干这件事。
rem
rem  做四件事（幂等，可反复跑）：
rem    1. 定出新口令（随机生成 / 手工输入 / 沿用当前）
rem    2. 用**旧口令**登录，执行 SET PASSWORD 换成新口令
rem    3. 用**新口令**复验，通过才写 <项目根>\.mysql-pass
rem    4. 设环境变量 OKX_MYSQL_PASS，并重启 OKXWeb 服务
rem
rem  安全设计：**先验证再落盘，且失败可回退**
rem    - 动库之前先确认旧口令真的能连；连不上就一行都不改
rem    - 改完立刻用新口令复验；复验失败会自动把库改回旧口令
rem      并把文件恢复成旧口令 —— 绝不会出现「库改了、文件没改」的锁死状态
rem    - 不需要 MySQL root：okx 账号可以改自己的口令（SET PASSWORD）
rem
rem  用法：
rem    set_db_pass.bat                     交互（双击走这个）
rem    set_db_pass.bat --gen               生成随机口令并轮换（推荐）
rem    set_db_pass.bat --pass <口令>       指定口令轮换
rem    set_db_pass.bat --pass-file <路径>  从文件读口令（自动化用，最稳）
rem    set_db_pass.bat --file-only         只写文件/环境变量，不动 MySQL
rem    set_db_pass.bat --no-env            不设环境变量
rem    set_db_pass.bat --no-restart        不重启服务
rem
rem  注意：--pass 后面的口令会出现在命令行里，别含  & ^ %% " < > |  这些字符。
rem        要自动化就用 --pass-file。
rem ===========================================================================

set "SYS=%SystemRoot%\System32"
set "ROOT=%~dp0.."
pushd "%ROOT%"
set "ROOT=%CD%"
popd
cd /d "%ROOT%"

set "MODE=ask"
set "NEWPASS="
set "NEWFROM="
set "DO_ENV=1"
set "DO_RESTART=1"
set "FILE_ONLY=0"
if not "%~1"=="" set "HADARG=1"

rem ---- 参数解析（一律用 goto，不用括号块 —— 块里 shift 和 %% 展开都会咬人）----
:parse
if "%~1"=="" goto parsed
if /i "%~1"=="--gen" goto opt_gen
if /i "%~1"=="--file-only" goto opt_fileonly
if /i "%~1"=="--no-env" goto opt_noenv
if /i "%~1"=="--no-restart" goto opt_norestart
if /i "%~1"=="--pass" goto opt_pass
if /i "%~1"=="--pass-file" goto opt_passfile
if /i "%~1"=="-h" goto usage
if /i "%~1"=="--help" goto usage
if /i "%~1"=="/?" goto usage
echo [错误] 不认识的参数：%~1
goto usage

:opt_gen
set "MODE=gen"
shift
goto parse

:opt_fileonly
set "FILE_ONLY=1"
shift
goto parse

:opt_noenv
set "DO_ENV=0"
shift
goto parse

:opt_norestart
set "DO_RESTART=0"
shift
goto parse

:opt_pass
set "MODE=set"
set "NEWPASS=%~2"
set "NEWFROM=命令行 --pass"
shift
shift
if "%NEWPASS%"=="" (
    echo [错误] --pass 后面要跟口令。
    goto usage
)
goto parse

:opt_passfile
set "MODE=set"
set "NEWFROM=口令文件"
shift
set "PF=%~1"
shift
if "%PF%"=="" (
    echo [错误] --pass-file 后面要跟文件路径。
    goto usage
)
if not exist "%PF%" (
    echo [错误] 找不到口令文件：%PF%
    exit /b 1
)
set "NEWPASS="
set /p NEWPASS=<"%PF%"
if "%NEWPASS%"=="" (
    echo [错误] 口令文件是空的：%PF%
    exit /b 1
)
goto parse

:parsed

echo.
echo ==============================================================
echo  OKX 终端 · MySQL 口令设置 / 轮换
echo  项目目录：%ROOT%
echo ==============================================================
echo.

rem ---- 读当前口令（环境变量 → .mysql-pass），顺序与 Go 侧一致 ----
call "%ROOT%\scripts\_read_db_pass.bat"
set "OLDPASS=%MYSQL_PASS%"

echo 当前状态：
if defined OLDPASS (
    echo   已配置口令：是（来源：环境变量 OKX_MYSQL_PASS 或 .mysql-pass）
) else (
    echo   已配置口令：否（.mysql-pass 不存在，环境变量也没设）
)
echo   数据库账号：%MYSQL_USER% @ 127.0.0.1:3306
echo   密钥文件  ：%ROOT%\.mysql-pass
echo.

rem ---- 菜单（只有在没给 --gen / --pass 时才问）----
if "%MODE%"=="gen" goto have_pass
if "%MODE%"=="set" goto have_pass

echo   [G] 生成一个随机口令并轮换（推荐）
echo   [M] 手工输入一个新口令
echo   [K] 不轮换，只把当前口令写进 .mysql-pass 与环境变量
echo   [Q] 退出
echo.
set "CH="
set /p "CH=请选择 [G/M/K/Q]: "
if /i "%CH%"=="Q" goto cancel
if /i "%CH%"=="K" goto keep_only
if /i "%CH%"=="G" goto menu_gen
if /i "%CH%"=="M" goto menu_manual
echo   没听懂，按 G 处理。
goto menu_gen

:menu_gen
set "MODE=gen"
goto have_pass

:menu_manual
echo.
echo   直接输入新口令后回车（只含字母数字最省事；含 & ^ %% " 之类会被 cmd 吃掉）
set "NEWPASS="
set /p "NEWPASS=新口令: "
if "%NEWPASS%"=="" (
    echo   [错误] 空的，没有改动。
    goto cancel
)
if "%NEWPASS%"=="%OLDPASS%" (
    echo   [提示] 和当前口令一样，没有改动。
    goto cancel
)
set "MODE=set"
set "NEWFROM=手工输入"
goto have_pass

:keep_only
if not defined OLDPASS (
    echo.
    echo   [错误] 当前没有任何口令可写 —— 请先选 G 或 M 生成一个。
    goto cancel
)
set "NEWPASS=%OLDPASS%"
set "FILE_ONLY=1"
set "NEWFROM=沿用当前口令"
goto have_pass

rem ---- 生成随机口令 ----
rem  ★ 这里**故意不用 if (...) else (...) 块** ★
rem    块内所有 %VAR% 在「整块被读到」时就展开完了，而 call 是块执行时才跑的 ——
rem    `set "NEWPASS=%MYSQL_PASS%"` 会拿到调用前的空值。
rem    这是批处理最经典的静默失效：不报错，只是新口令永远为空。
:have_pass
if not "%MODE%"=="gen" goto have_pass_set

echo [1/4] 生成随机口令 ...
call "%ROOT%\scripts\_gen_db_pass.bat"
if errorlevel 1 goto cancel
set "NEWPASS=%MYSQL_PASS%"
set "NEWFROM=随机生成"
goto have_pass_ok

:have_pass_set
echo [1/4] 使用指定口令（%NEWFROM%）...

:have_pass_ok
if "%NEWPASS%"=="" (
    echo       [错误] 新口令是空的，没有改动。
    goto cancel
)
echo       已确定新口令（%NEWFROM%，不回显）。
echo.

rem ---- 只写文件的分支 ----
if "%FILE_ONLY%"=="1" goto write_only

rem ---- 2/4 动库前先确认旧口令能连 ----
echo [2/4] 校验当前口令能否登录（不能就不动任何东西）...
if not defined OLDPASS (
    echo       [错误] 没有可用的旧口令 —— 无法登录改密。
    echo              先手工把当前口令写进 %ROOT%\.mysql-pass，或者用 root 跑
    echo              scripts\sql\init_db.sql 重建账号。
    goto cancel
)
call :trypass "%OLDPASS%"
if errorlevel 1 (
    echo       [错误] 用当前口令连不上 127.0.0.1:3306。
    echo              要么 MySQL 没起来（scripts\start_all.bat），
    echo              要么 .mysql-pass 里的口令已经不是数据库里的那个。
    echo              一行都没改，先解决这个再跑。
    goto cancel
)
echo       通过。
echo.

if "%NEWPASS%"=="%OLDPASS%" (
    echo       [提示] 新旧口令相同，跳过改库，直接写文件。
    goto write_only
)

rem ---- 3/4 改库 ----
echo [3/4] 在 MySQL 里把口令改成新的 ...
"%ROOT%\mysql\bin\mysql.exe" -u%MYSQL_USER% -p%OLDPASS% -h127.0.0.1 --connect-timeout=5 -e "SET PASSWORD = '%NEWPASS%';" >nul 2>&1

rem 不管上面退出码如何，都用新口令实测一次 —— SET PASSWORD 是单条语句，
rem 要么生效要么没生效，与其猜退出码不如直接验。
call :trypass "%NEWPASS%"
if errorlevel 1 goto new_failed
echo       改密成功，新口令已生效。
echo.

:write_only
echo [4/4] 写密钥文件与环境变量 ...
>"%ROOT%\.mysql-pass" echo %NEWPASS%
if not exist "%ROOT%\.mysql-pass" (
    echo       [错误] 写 %ROOT%\.mysql-pass 失败。
    goto cancel
)
echo       已写 %ROOT%\.mysql-pass（已在 .gitignore 里，不会入库）

if "%DO_ENV%"=="1" (
    setx OKX_MYSQL_PASS "%NEWPASS%" >nul 2>&1
    if errorlevel 1 (
        echo       [警告] 环境变量没设上（setx 被拦？）。不影响服务 ——
        echo              OKXWeb 读的是 .mysql-pass 文件。
    ) else (
        echo       已设环境变量 OKX_MYSQL_PASS（用户级，只对新开的命令行生效）
    )
)

if "%DO_RESTART%"=="1" goto restart
echo.
echo ==============================================================
echo  完成。**记得重启 OKXWeb 服务**（本脚本按 --no-restart 跳过了）：
echo      net stop OKXWeb  ^&  net start OKXWeb
echo ==============================================================
goto done

:restart
echo.
echo       重启 OKXWeb 服务让它用上新口令 ...
sc query OKXWeb >nul 2>&1
if errorlevel 1 (
    echo       [提示] OKXWeb 服务没注册，跳过。跑 scripts\start_all.bat 即可。
    goto done
)
net stop OKXWeb >nul 2>&1
"%SYS%\ping.exe" -n 4 127.0.0.1 >nul 2>&1
net start OKXWeb >nul 2>&1
"%SYS%\ping.exe" -n 9 127.0.0.1 >nul 2>&1

echo.
echo ==============================================================
echo  完成。核对一下服务与网页：
echo      net start ^| %SYS%\findstr.exe /I OKX
echo      curl http://localhost/api/state
echo ==============================================================
goto done

rem ---------------------------------------------------------------------------
rem  :trypass <口令>  ——  能登录返回 0，不能返回 1
rem  口令为空时直接返回 1：`-p` 后面不带值会变成交互式提示，在脚本里就是死等。
rem ---------------------------------------------------------------------------
:trypass
setlocal

set "TP=%~1"
if "%TP%"=="" endlocal & exit /b 1
if not exist "%ROOT%\mysql\bin\mysql.exe" endlocal & exit /b 1

"%ROOT%\mysql\bin\mysql.exe" -u%MYSQL_USER% -p%TP% -h127.0.0.1 --connect-timeout=5 -e "SELECT 1;" >nul 2>&1
if errorlevel 1 endlocal & exit /b 1

endlocal & exit /b 0

rem ---------------------------------------------------------------------------
rem  失败回退：把库改回旧口令，文件恢复成旧口令
rem ---------------------------------------------------------------------------
:new_failed
echo       [警告] 新口令登录失败 —— 大概率 SET PASSWORD 没生效。
call :trypass "%OLDPASS%"
if errorlevel 1 (
    echo       [错误] 旧口令现在也连不上了。**请人工介入**：
    echo              用 root 跑一遍 scripts\sql\init_db.sql 里的建账号语句，
    echo              口令填 %ROOT%\.mysql-pass 里的那个。
    goto cancel
)
echo       旧口令仍然有效，说明库没改成功。
echo       已把 %ROOT%\.mysql-pass 恢复成旧口令，环境未改动。
>"%ROOT%\.mysql-pass" echo %OLDPASS%
goto cancel

:usage
echo.
echo 用法：set_db_pass.bat [选项]
echo   --gen               生成随机口令并轮换（推荐）
echo   --pass ^<口令^>       指定口令（别含  ^&  ^^  "  ^<  ^>  ^| ）
echo   --pass-file ^<路径^>  从文件读口令（自动化用，最稳）
echo   --file-only         只写文件 / 环境变量，不动 MySQL
echo   --no-env            不设环境变量
echo   --no-restart        不重启 OKXWeb
echo   不带参数 = 交互菜单
echo.
if not defined HADARG pause
endlocal
exit /b 1

:cancel
echo.
echo 没有做任何改动。
if not defined HADARG pause
endlocal
exit /b 1

:done
echo.
if not defined HADARG pause
endlocal
exit /b 0
