@echo off
chcp 65001 >nul
setlocal EnableDelayedExpansion

rem ===========================================================================
rem  OKX 全合约量化终端 —— 一次性的服务注册 / 环境安装
rem  ---------------------------------------------------------------------------
rem  做三件事（都已做过的话可以重复跑，幂等）：
rem    1. 把 MySQL 注册成 Windows 服务 OKXMySQL（自动启动）
rem    2. 把 Apache 注册成 Windows 服务 OKXApache（自动启动）
rem    3. 装 VC++ 2026 运行库（Apache / MySQL 都依赖）
rem  需要管理员权限运行。
rem ===========================================================================

set "ROOT=%~dp0.."
pushd "%ROOT%"
set "ROOT=%CD%"
popd

net session >nul 2>&1
if errorlevel 1 (
    echo.
    echo [错误] 请右键"以管理员身份运行"本脚本。
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
echo [1/5] 检查 VC++ 2026 运行库 ...
if exist "%ROOT%\runtime\vcruntime140.dll" (
    echo       已就绪（runtime\ 下已有运行库）。
) else (
    echo       未找到 runtime\vcruntime140.dll
    echo       请先安装 VS2026 版运行库：https://aka.ms/vs/18/release/vc_redist.x64.exe
)

rem ---- 2. MySQL 数据目录初始化 ---------------------------------------------
echo.
echo [2/5] 检查 MySQL 数据目录 ...
if exist "%ROOT%\mysql\data\mysql" (
    echo       已初始化，跳过。
) else (
    echo       正在初始化（--initialize-insecure）...
    if not exist "%ROOT%\mysql\data" mkdir "%ROOT%\mysql\data"
    "%ROOT%\mysql\bin\mysqld.exe" --defaults-file="%ROOT%\conf\my.ini" --initialize-insecure --console
    if errorlevel 1 (
        echo       [错误] 初始化失败，检查 conf\my.ini 里的路径。
    ) else (
        echo       初始化完成。root 空密码，第一次登录后请改密码：
        echo         mysql -uroot -h127.0.0.1
        echo         ALTER USER 'root'@'localhost' IDENTIFIED BY '新密码';
    )
)

rem ---- 3. 注册 MySQL 服务 --------------------------------------------------
echo.
echo [3/5] 注册 MySQL 服务 (OKXMySQL) ...
sc query OKXMySQL >nul 2>&1
if not errorlevel 1 (
    echo       已存在，只做启动类型校正。
    sc config OKXMySQL start= auto >nul
) else (
    "%ROOT%\mysql\bin\mysqld.exe" --install OKXMySQL --defaults-file="%ROOT%\conf\my.ini"
    sc config OKXMySQL start= auto >nul
)
net start OKXMySQL >nul 2>&1
echo       完成，并已尝试启动。

rem ---- 4. 建库 / 建账号 ----------------------------------------------------
echo.
echo [4/5] 建库 okx + 账号 okx ...
"%ROOT%\mysql\bin\mysql.exe" -uroot -h127.0.0.1 --default-character-set=utf8mb4 -e "CREATE DATABASE IF NOT EXISTS okx DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci; CREATE USER IF NOT EXISTS 'okx'@'127.0.0.1' IDENTIFIED WITH mysql_native_password BY 'OkxQuant2026'; CREATE USER IF NOT EXISTS 'okx'@'localhost' IDENTIFIED WITH mysql_native_password BY 'OkxQuant2026'; GRANT ALL PRIVILEGES ON okx.* TO 'okx'@'127.0.0.1'; GRANT ALL PRIVILEGES ON okx.* TO 'okx'@'localhost'; FLUSH PRIVILEGES;" 2>nul
if errorlevel 1 (
    echo       [提示] root 已设密码，跳过自动建库。手工执行：
    echo         scripts\sql\init_db.sql
) else (
    echo       完成。
)

rem ---- 5. 注册 Apache ------------------------------------------------------
echo.
echo [5/5] 注册 Apache 服务 (OKXApache) ...
if not exist "%ROOT%\apache\bin\httpd.exe" (
    echo       [错误] 找不到 apache\bin\httpd.exe
) else (
    "%ROOT%\apache\bin\httpd.exe" -t
    if errorlevel 1 (
        echo       [错误] httpd.conf 语法有误，先修好再注册。
    ) else (
        sc query OKXApache >nul 2>&1
        if not errorlevel 1 (
            echo       已存在，只做启动类型校正。
            sc config OKXApache start= auto >nul
        ) else (
            "%ROOT%\apache\bin\httpd.exe" -k install -n OKXApache
            sc config OKXApache start= auto >nul
        )
        net start OKXApache >nul 2>&1
        echo       完成，并已尝试启动。
    )
)

echo.
echo ==============================================================
echo  安装结束。接着跑 scripts\start_all.bat 把三个服务一起拉起来。
echo ==============================================================
echo.
pause
