@echo off
chcp 936 >nul 2>&1
title OKX 终端 - 安装服务
setlocal EnableExtensions

rem ===========================================================================
rem  OKX 全合约量化终端 —— 一次性的服务注册 / 环境安装
rem  ---------------------------------------------------------------------------
rem  做六件事（重复跑也安全，幂等）：
rem    1. 检查 VC++ 运行库
rem    2. 初始化 MySQL 数据目录（仅在缺失时）
rem    3. 把 MySQL 注册成 Windows 服务 OKXMySQL（自动启动）
rem    4. 建库 okx + 账号 okx（口令读 .mysql-pass；没有就**现场生成随机口令**，
rem       不再使用写死的默认口令 —— 那个已经随 public 仓库公开了）
rem    5. 把 Apache 注册成 Windows 服务 OKXApache（自动启动）
rem    6. 把 OKXWeb 注册成 Windows 服务（网页 + 引擎，Session 0 无黑窗口）
rem  需要管理员权限运行。
rem
rem  注意：findstr / timeout / where 一律走 System32 绝对路径，
rem        避免装了 Git/Cygwin 时被同名 GNU 工具顶掉。
rem ===========================================================================

set "SYS=%SystemRoot%\System32"
set "ROOT=%~dp0.."
pushd "%ROOT%"
set "ROOT=%CD%"
popd
cd /d "%ROOT%"

net session >nul 2>&1
if errorlevel 1 (
    echo.
    echo [错误] 需要管理员权限。请右键本文件，选"以管理员身份运行"。
    echo.
    pause
    exit /b 1
)

echo.
echo ==============================================================
echo  OKX 终端 · 安装服务
echo  项目目录：%ROOT%
echo ==============================================================
echo.

rem ---- 1. VC 运行库 --------------------------------------------------------
echo [1/6] 检查 VC++ 运行库 ...
if exist "%ROOT%\runtime\vcruntime140.dll" (
    echo       已就绪（runtime\ 下已有运行库）。
) else (
    echo       未找到 runtime\vcruntime140.dll
    echo       请先装 VS2026 版运行库：
    echo       https://aka.ms/vs/18/release/vc_redist.x64.exe
)
echo.

rem ---- 2. MySQL 数据目录初始化 ---------------------------------------------
echo [2/6] 检查 MySQL 数据目录 ...
if exist "%ROOT%\mysql\data\mysql" (
    echo       已初始化，跳过。
) else (
    echo       正在初始化（--initialize-insecure）...
    if not exist "%ROOT%\mysql\data" mkdir "%ROOT%\mysql\data"
    "%ROOT%\mysql\bin\mysqld.exe" --defaults-file="%ROOT%\conf\my.ini" --initialize-insecure --console
    if errorlevel 1 (
        echo       [错误] 初始化失败，检查 conf\my.ini 里的路径。
    ) else (
        echo       初始化完成。root 初始为空密码，首次登录后建议改密码。
    )
)
echo.

rem ---- 3. 注册 MySQL 服务 --------------------------------------------------
echo [3/6] 注册 MySQL 服务 OKXMySQL ...
sc query OKXMySQL >nul 2>&1
if errorlevel 1 (
    echo       服务不存在，正在注册 ...
    "%ROOT%\mysql\bin\mysqld.exe" --install OKXMySQL --defaults-file="%ROOT%\conf\my.ini"
    if errorlevel 1 (
        echo       [错误] 注册失败。
    ) else (
        echo       注册成功。
    )
) else (
    echo       服务已存在，跳过注册。
)
sc config OKXMySQL start= auto >nul 2>&1
sc query OKXMySQL | "%SYS%\findstr.exe" /I /C:"RUNNING" >nul
if errorlevel 1 (
    net start OKXMySQL >nul 2>&1
    if errorlevel 1 (
        echo       [警告] 启动失败，检查 mysql\data\*.err 日志。
    ) else (
        echo       已启动。
    )
) else (
    echo       已在运行。
)
echo.

rem ---- 4. 建库 / 建账号 ----------------------------------------------------
echo [4/6] 建库 okx + 账号 okx ...
rem 口令不再写死在脚本里：
rem   环境变量 OKX_MYSQL_PASS → <项目根>\.mysql-pass → 都没有就**现场生成一个随机口令**。
rem   全新安装不会再用一个人人皆知的默认口令，也不需要人工先设一遍。
rem ★ call 必须在 if(...) 块**外面**：块内 %MYSQL_PASS% 会在整块被读到时就展开，
rem   而 call 是块执行时才跑的 —— 那样拿到的永远是空值。
call "%ROOT%\scripts\_read_db_pass.bat"
if defined MYSQL_PASS goto have_account_pass

echo       没找到口令，正在生成随机口令 ...
call "%ROOT%\scripts\_gen_db_pass.bat"
if errorlevel 1 (
    echo       [错误] 生成口令失败。先双击 scripts\set_db_pass.bat 再来。
    goto accounts_end
)

:have_account_pass
echo       口令已就绪（不回显），存放于 %ROOT%\.mysql-pass
"%ROOT%\mysql\bin\mysql.exe" -uroot -h127.0.0.1 --default-character-set=utf8mb4 -e "CREATE DATABASE IF NOT EXISTS okx DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci; CREATE USER IF NOT EXISTS 'okx'@'127.0.0.1' IDENTIFIED WITH mysql_native_password BY '%MYSQL_PASS%'; ALTER USER 'okx'@'127.0.0.1' IDENTIFIED WITH mysql_native_password BY '%MYSQL_PASS%'; CREATE USER IF NOT EXISTS 'okx'@'localhost' IDENTIFIED WITH mysql_native_password BY '%MYSQL_PASS%'; ALTER USER 'okx'@'localhost' IDENTIFIED WITH mysql_native_password BY '%MYSQL_PASS%'; GRANT ALL PRIVILEGES ON okx.* TO 'okx'@'127.0.0.1'; GRANT ALL PRIVILEGES ON okx.* TO 'okx'@'localhost'; FLUSH PRIVILEGES;" >nul 2>&1
if errorlevel 1 (
    echo       [提示] root 已设密码，自动建库被拒。两个办法：
    echo         1^) 用 root 跑一遍 scripts\sql\init_db.sql（先按文件头注释改掉里面的口令占位符）
    echo         2^) okx 账号已存在的话：双击 scripts\set_db_pass.bat 直接改口令
    echo             —— 改自己的口令不需要 root
) else (
    echo       完成。数据库口令已与 .mysql-pass 对齐。
)

:accounts_end
echo.

rem ---- 5. 注册 Apache ------------------------------------------------------
echo [5/6] 注册 Apache 服务 OKXApache ...
if not exist "%ROOT%\apache\bin\httpd.exe" (
    echo       [错误] 找不到 apache\bin\httpd.exe
) else (
    "%ROOT%\apache\bin\httpd.exe" -t
    if errorlevel 1 (
        echo       [错误] httpd.conf 语法有误，先修好再注册。
    ) else (
        sc query OKXApache >nul 2>&1
        if errorlevel 1 (
            echo       服务不存在，正在注册 ...
            "%ROOT%\apache\bin\httpd.exe" -k install -n OKXApache
            echo       注册成功。
        ) else (
            echo       服务已存在，跳过注册。
        )
        sc config OKXApache start= auto >nul 2>&1
        sc query OKXApache | "%SYS%\findstr.exe" /I /C:"RUNNING" >nul
        if errorlevel 1 (
            net start OKXApache >nul 2>&1
            if errorlevel 1 (
                echo       [警告] 启动失败，检查 apache\logs\error.log。
            ) else (
                echo       已启动。
            )
        ) else (
            echo       已在运行。
        )
    )
)

rem ---- 6. 注册 OKXWeb（Go 网页 + 引擎，无 cmd 黑窗口）-----------------------
echo [6/6] 注册 OKXWeb 服务（网页 + 自动交易引擎）...
if not exist "%ROOT%\bin\okxweb.exe" (
    echo       [警告] 找不到 bin\okxweb.exe，先跑 scripts\build.bat 再回来。
    echo              （现在跳过，不影响 MySQL / Apache）
) else (
    rem okxweb.exe -install 是幂等的：已存在就更新配置，不存在才创建。
    rem 注册后它跑在 Session 0，没有控制台窗口，崩溃由 SCM 自动重启。
    "%ROOT%\bin\okxweb.exe" -install
)
echo.

echo.
echo ==============================================================
echo  安装结束。
echo  接着跑 scripts\start_all.bat 把三个服务一起拉起来。
echo  查看服务：net start ^| findstr /I OKX
echo ==============================================================
echo.
pause
endlocal
