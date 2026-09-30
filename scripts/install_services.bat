@echo off
chcp 936 >nul 2>&1
title OKX 终端 - 安装服务
setlocal EnableExtensions

rem ===========================================================================
rem  OKX 全合约量化终端 —— 一次性的服务注册 / 环境安装
rem  ---------------------------------------------------------------------------
rem  做五件事（重复跑也安全，幂等）：
rem    1. 检查 VC++ 运行库
rem    2. 初始化 MySQL 数据目录（仅在缺失时）
rem    3. 把 MySQL 注册成 Windows 服务 OKXMySQL（自动启动）
rem    4. 建库 okx + 账号 okx
rem    5. 把 Apache 注册成 Windows 服务 OKXApache（自动启动）
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
"%ROOT%\mysql\bin\mysql.exe" -uroot -h127.0.0.1 --default-character-set=utf8mb4 -e "CREATE DATABASE IF NOT EXISTS okx DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci; CREATE USER IF NOT EXISTS 'okx'@'127.0.0.1' IDENTIFIED WITH mysql_native_password BY 'OkxQuant2026'; CREATE USER IF NOT EXISTS 'okx'@'localhost' IDENTIFIED WITH mysql_native_password BY 'OkxQuant2026'; GRANT ALL PRIVILEGES ON okx.* TO 'okx'@'127.0.0.1'; GRANT ALL PRIVILEGES ON okx.* TO 'okx'@'localhost'; FLUSH PRIVILEGES;" >nul 2>&1
if errorlevel 1 (
    echo       [提示] root 可能已设密码，自动建库被拒。手工执行一次：
    echo         mysql -uroot -p -h127.0.0.1 --default-character-set=utf8mb4 ^< scripts\sql\init_db.sql
) else (
    echo       完成。
)
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

echo.
echo ==============================================================
echo  安装结束。
echo  接着跑 scripts\start_all.bat 把服务一起拉起来。
echo ==============================================================
echo.
pause
endlocal
