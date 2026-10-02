package rbac

// smtp.go —— 验证码邮件的发送实现
//
// 用标准库 net/smtp，**不引入任何新依赖** —— 这个项目跑在 2 核 1.97G 的小机器上，
// 为一个登录验证码拉一个第三方邮件 SDK 不划算。
//
// ★ 凭据来源：环境变量 → 工作目录下的 .smtp-pass 文件 → 空。
//   绝不把授权码写进仓库里的任何文件（本项目 .gitignore 已挡 .mysql-pass/.git-token，
//   .smtp-pass 同样不该进版本库 —— 见 scripts 里的部署说明）。
//   和数据库口令一样：口令链只走「环境变量 → 本机文件」，绝不落到配置 JSON 里，
//   因为那份 JSON 是要被推到 GitHub 双仓的。

import (
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SMTPConfig QQ 邮箱 SMTP 参数
type SMTPConfig struct {
	Host string // smtp.qq.com
	Port int    // 465（SSL）
	User string // 发件邮箱，如 493076373@qq.com
	Pass string // ★ 授权码，不是登录密码
}

// LoadSMTPConfig 从环境变量 / 本机文件读 SMTP 配置。
//
//	OKX_SMTP_HOST / OKX_SMTP_PORT / OKX_SMTP_USER / OKX_SMTP_PASS
//	→ 若 USER/PASS 缺失，再试工作目录下的 .smtp-pass（内容：邮箱:授权码）
//
// 返回的 ok=false 表示「没配置」，调用方应把验证码接口报成「服务未配置」
// 而不是静默失败 —— 否则用户会一直等一封永远不来的邮件。
func LoadSMTPConfig(root string) (SMTPConfig, bool) {
	c := SMTPConfig{
		Host: strings.TrimSpace(os.Getenv("OKX_SMTP_HOST")),
		User: strings.TrimSpace(os.Getenv("OKX_SMTP_USER")),
		Pass: strings.TrimSpace(os.Getenv("OKX_SMTP_PASS")),
	}
	if v := strings.TrimSpace(os.Getenv("OKX_SMTP_PORT")); v != "" {
		var p int
		if _, err := fmt.Sscanf(v, "%d", &p); err == nil && p > 0 {
			c.Port = p
		}
	}
	if c.Host == "" {
		c.Host = "smtp.qq.com"
	}
	if c.Port == 0 {
		c.Port = 465
	}

	// 环境变量不齐 → 试本机文件
	if c.User == "" || c.Pass == "" {
		for _, p := range []string{
			filepath.Join(root, ".smtp-pass"),
			".smtp-pass",
			filepath.Join(root, "configs", ".smtp-pass"),
		} {
			raw, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			line := strings.TrimSpace(string(raw))
			// 格式：邮箱:授权码（允许只有授权码，此时发件人默认用授权邮箱）
			u, pw, found := strings.Cut(line, ":")
			if !found {
				pw = line
				u = ""
			}
			if c.User == "" && strings.TrimSpace(u) != "" {
				c.User = strings.TrimSpace(u)
			}
			if c.Pass == "" {
				c.Pass = strings.TrimSpace(pw)
			}
			break
		}
	}
	if c.User == "" {
		c.User = allowedEmail // 默认用授权邮箱自己当发件人
	}
	return c, c.Pass != ""
}

// SMTPMailer 走 SMTP 的实现（465 隐式 TLS，QQ 邮箱要求）
type SMTPMailer struct {
	cfg  SMTPConfig
	logf func(string, ...any)
}

// NewSMTPMailer 建发送器
func NewSMTPMailer(cfg SMTPConfig, logf func(string, ...any)) *SMTPMailer {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &SMTPMailer{cfg: cfg, logf: logf}
}

// Send 发一封纯文本邮件
func (m *SMTPMailer) Send(to, subject, body string) error {
	if m.cfg.Pass == "" {
		return fmt.Errorf("SMTP 凭据未配置（需要 OKX_SMTP_PASS 或 .smtp-pass 文件）")
	}
	addr := net.JoinHostPort(m.cfg.Host, fmt.Sprint(m.cfg.Port))

	// 465 是隐式 TLS：先建 TLS 连接再说话。
	// 注意不能用 smtp.SendMail —— 它只支持 STARTTLS（587），对 465 会卡住。
	d := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := tls.DialWithDialer(d, "tcp", addr, &tls.Config{
		ServerName: m.cfg.Host,
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		return fmt.Errorf("连接 %s 失败：%w", addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))

	cli, err := smtp.NewClient(conn, m.cfg.Host)
	if err != nil {
		return fmt.Errorf("SMTP 握手失败：%w", err)
	}
	defer cli.Close()

	auth := smtp.PlainAuth("", m.cfg.User, m.cfg.Pass, m.cfg.Host)
	if err := cli.Auth(auth); err != nil {
		return fmt.Errorf("SMTP 认证失败（确认用的是授权码而不是登录密码）：%w", err)
	}
	if err := cli.Mail(m.cfg.User); err != nil {
		return fmt.Errorf("发件人被拒：%w", err)
	}
	if err := cli.Rcpt(to); err != nil {
		return fmt.Errorf("收件人被拒：%w", err)
	}
	wc, err := cli.Data()
	if err != nil {
		return fmt.Errorf("开始投递失败：%w", err)
	}
	msg := buildMessage(m.cfg.User, to, subject, body)
	if _, err := wc.Write([]byte(msg)); err != nil {
		_ = wc.Close()
		return fmt.Errorf("写入邮件正文失败：%w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("投递失败：%w", err)
	}
	if err := cli.Quit(); err != nil {
		// Quit 失败不影响投递结果，只记日志
		m.logf("SMTP QUIT 报错（邮件应已投出）：%v", err)
	}
	return nil
}

// buildMessage 拼 RFC5322 报文。纯文本 + UTF-8，中文标头用 Base64 编码避免乱码。
func buildMessage(from, to, subject, body string) string {
	var b strings.Builder
	b.WriteString("From: " + encodeHeader(from) + "\r\n")
	b.WriteString("To: " + encodeHeader(to) + "\r\n")
	b.WriteString("Subject: " + encodeHeader(subject) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)
	return b.String()
}

// encodeHeader 非 ASCII 标头编成 =?UTF-8?B?...?=
func encodeHeader(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			ascii = false
			break
		}
	}
	if ascii {
		return s
	}
	return "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?="
}
