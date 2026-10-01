package conf

// secret.go —— MySQL 凭据解析（2026-10-01 新增）
//
// 为什么要有这个文件：
//
//	口令原来直接写在 4 个 .go 源文件 + 3 个批处理里，而仓库是 public 的 ——
//	等于把口令连同代码一起公开。虽然 MySQL 只监听 127.0.0.1，但「只监听本机」
//	挡的是外网，挡不住本机的任何进程；这个风险不该靠「恰好没人翻仓库」来成立。
//
// 现在**入库文件里不再出现任何明文口令**，运行时按下面的优先级解析：
//
//	1. 环境变量  OKX_MYSQL_PASS（口令）/ OKX_MYSQL_USER（用户）
//	             OKX_MYSQL_DSN（整串 DSN，填了就忽略上面两个）
//	2. 密钥文件  <项目根>\.mysql-pass（一行口令，已在 .gitignore 里）
//	3. 空串      —— 连不上时错误信息会明确说去哪配（见 MySQLHint）
//
// 为什么环境变量优先：12-factor 口径，部署时注入、不落盘、进程内可见。
//
// 为什么还留着密钥文件：Windows 服务的环境变量是 SCM 在启动时**整份拷贝**的，
// `setx /M` 之后已注册的服务往往要等重启整机才看得到 —— 文件没有这个坑。
// scripts\set_db_pass.bat 会同时写「密钥文件 + 环境变量」，两者不会打架。
//
// 相关入口：repo.DefaultMySQLConfig()（Go 侧）/
//
//	scripts\set_db_pass.bat（设置与轮换）/ scripts\start_all.bat（探测连通性）。

import (
	"os"
	"path/filepath"
	"strings"
)

const (
	// EnvMySQLUser MySQL 用户名环境变量
	EnvMySQLUser = "OKX_MYSQL_USER"
	// EnvMySQLPass MySQL 口令环境变量
	EnvMySQLPass = "OKX_MYSQL_PASS"
	// EnvMySQLDSN 完整 DSN 环境变量
	EnvMySQLDSN = "OKX_MYSQL_DSN"

	// SecretFileMySQL 本地密钥文件名（项目根下，第一行写口令）
	SecretFileMySQL = ".mysql-pass"

	// DefaultMySQLUser 用户名兜底
	DefaultMySQLUser = "okx"
	// DefaultMySQLDatabase 库名兜底
	DefaultMySQLDatabase = "okx"
)

// RootDir 项目根目录（含 go.mod 的那一层）
func RootDir() string { return projectRoot() }

// SecretPath 项目根下的密钥文件路径
func SecretPath(name string) string { return filepath.Join(RootDir(), name) }

// readSecretLine 读密钥文件的第一行并去掉首尾空白。
//
// 顺手容忍 UTF-8 BOM 和 CRLF：用记事本存出来经常带这两样。
// 带着 BOM 的口令会直接认证失败，而现象是「密码明明没错却连不上」——
// 这种问题排查起来最费时间，不如在这里吃掉。
func readSecretLine(name string) string {
	raw, err := os.ReadFile(SecretPath(name))
	if err != nil {
		return ""
	}
	s := strings.TrimPrefix(string(raw), "\ufeff")
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// MySQLUser 解析 MySQL 用户名（环境变量 → 默认 okx）
func MySQLUser() string {
	if v := strings.TrimSpace(os.Getenv(EnvMySQLUser)); v != "" {
		return v
	}
	return DefaultMySQLUser
}

// MySQLSecret 解析 MySQL 口令。
//
// 第二个返回值是**来源描述**，只用来在启动横幅 / 诊断命令里说明
// 「口令是从哪拿到的」—— 绝不把口令本身回显出去（日志经常被人截图贴出来）。
func MySQLSecret() (pass string, source string) {
	if v := strings.TrimSpace(os.Getenv(EnvMySQLPass)); v != "" {
		return v, "环境变量 " + EnvMySQLPass
	}
	if v := readSecretLine(SecretFileMySQL); v != "" {
		return v, "密钥文件 " + SecretPath(SecretFileMySQL)
	}
	return "", ""
}

// MySQLDSN 完整 DSN（环境变量直给；填了就整个覆盖 user/pass/host 那几项）
func MySQLDSN() string { return strings.TrimSpace(os.Getenv(EnvMySQLDSN)) }

// MySQLHint 没配到口令时给一句能照着做的提示
func MySQLHint() string {
	return "未配置 MySQL 口令。任选一种（改完重启 okxweb 生效）：\n" +
		"  ① 双击 scripts\\set_db_pass.bat —— 生成/设置口令，并自动写好下面两处\n" +
		"  ② 在 " + SecretPath(SecretFileMySQL) + " 里写一行口令\n" +
		"  ③ 设环境变量 " + EnvMySQLPass
}
