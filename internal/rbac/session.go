// Package rbac —— 管理员鉴权：邮箱验证码 + 服务端会话
//
// 背景（2026-10-02 八期）：
//   本项目此前**没有任何鉴权**：谁能访问 80 端口，谁就能看全部持仓、改全部参数。
//   八期新增的「修改交易条件」入口更是直接写 configs/okx_strategy.json ——
//   一旦敞开，等于把下单口径交给任何能连上这台机器的人。
//
// 设计取舍（用户已确认）：
//   · 登录方式选**邮箱验证码**，不发 QQ 邮箱 OAuth —— 后者要腾讯开放平台
//     应用 ID/Secret，拿不到就做不出来。验证码发到唯一授权邮箱，
//     安全性等价于「只有邮箱主人能进」，且零外部依赖。
//   · 会话是**服务端**的：cookie 里只放随机 token，所有状态在内存。
//     不放签名 JWT —— 那样「立刻踢掉某个会话」做不到（改配置这种操作需要能踢）。
//
// ★ 安全红线（改这个文件时不要退让）：
//   1. 验证码 5 分钟有效、**用过即焚**、错误 5 次作废；
//   2. 比对用 crypto/subtle.ConstantTimeCompare，不用 ==（防时序侧信道）；
//   3. 会话 cookie HttpOnly + SameSite=Strict，绝不写进 JS 可读的地方；
//   4. token 用 crypto/rand，绝不用 math/rand 或时间戳；
//   5. 只认一个授权邮箱；其它邮箱发验证码一律**回复成功但不发**（不回显白名单）。
package rbac

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// 会话与验证码的时限。改这些值会放宽攻击窗口，谨慎。
const (
	CodeTTL     = 5 * time.Minute  // 验证码有效期
	CodeMaxTry  = 5                // 同一验证码最多试错几次
	SessionTTL  = 24 * time.Hour   // 会话有效期
	SendCoolDwn = 60 * time.Second // 同一 IP/邮箱 发码冷却，防轰炸
	CookieName  = "okx_admin"
)

var (
	ErrBadCode    = errors.New("验证码错误或已失效")
	ErrTooManyTry = errors.New("验证码错误次数过多，请重新获取")
	ErrTooOften   = errors.New("发送太频繁，请稍后再试")
	ErrNoMailer   = errors.New("邮件发送服务未配置，无法发送验证码")
)

// allowedEmail 唯一授权邮箱。硬编码而不是配置 —— 这是「谁能改我的钱」的根，
// 放进 JSON 等于把根也变成可热插拔的。
const allowedEmail = "493076373@qq.com"

// Mailer 邮件发送抽象。真实实现走 SMTP；测试里换成一个收集器。
//
// 把发送做成接口而不是直接调 net/smtp，是为了让「验证码流程」能被单测覆盖 ——
// 否则这个包只能靠手工点页面验证，正是安全代码最不该有的状态。
type Mailer interface {
	Send(to, subject, body string) error
}

// Code 一条待验证的验证码
type Code struct {
	hash   string // sha256(邮箱 + ":" + 码)，不存明文
	email  string
	expire time.Time
	tries  int
	sentAt time.Time
	ip     string
}

// Session 一条已登录会话
type Session struct {
	Token  string
	Email  string
	MadeAt time.Time
	Expire time.Time
	IP     string
	UA     string
}

// Manager 验证码 + 会话的内存持有者（并发安全）
type Manager struct {
	mu       sync.Mutex
	codes    map[string]*Code    // key = 邮箱
	sessions map[string]*Session // key = token
	mailer   Mailer
	logf     func(string, ...any)

	// lastSend 记录每个「邮箱+IP」上次发码时间，用于冷却
	lastSend map[string]time.Time
	// attempts 记录每个 IP 的失败次数（粗粒度防爆破）
	fails map[string]int
}

// NewManager 建一个管理器。mailer 为 nil 时 SendCode 会失败（但会话逻辑仍可用，
// 方便在没有 SMTP 的环境里跑测试）。
func NewManager(mailer Mailer, logf func(string, ...any)) *Manager {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Manager{
		codes:    map[string]*Code{},
		sessions: map[string]*Session{},
		lastSend: map[string]time.Time{},
		fails:    map[string]int{},
		mailer:   mailer,
		logf:     logf,
	}
}

// AllowedEmail 返回唯一授权邮箱（前端展示「验证码将发送到 xxx」用，
// 但 ★ 只有请求方自己知道该邮箱时才展示 ★ —— 见 Handler 的用法）。
func AllowedEmail() string { return allowedEmail }

// IsAllowed 判断邮箱是否在授权名单（大小写不敏感）
func IsAllowed(email string) bool {
	return strings.EqualFold(strings.TrimSpace(email), allowedEmail)
}

// hashCode sha256(邮箱 + ":" + 码)，hex 编码
func hashCode(email, code string) string {
	h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email)) + ":" + code))
	return hex.EncodeToString(h[:])
}

// randomCode 生成 6 位数字验证码
func randomCode() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	n := (uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])) % 1000000
	return fmt.Sprintf("%06d", n), nil
}

// randomToken 生成 32 字节随机 token（hex 64 字符）
func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// SendCode 给授权邮箱发验证码。
//
// ★ 无论邮箱是否在名单内，**返回给调用方的成功/失败语义都一样**（除了冷却与
// 发送失败），避免用接口探测「哪些邮箱被授权」。实际发信只发生在名单内。
func (m *Manager) SendCode(email, ip string) error {
	email = strings.TrimSpace(email)
	now := time.Now()

	m.mu.Lock()
	// 冷却：同一 邮箱+IP 60 秒内只能发一次（无论是否授权，都要挡，否则
	// 未授权邮箱可以被用来反复触发发信逻辑做探测）
	ck := strings.ToLower(email) + "|" + ip
	if t, ok := m.lastSend[ck]; ok && now.Sub(t) < SendCoolDwn {
		m.mu.Unlock()
		return ErrTooOften
	}
	m.lastSend[ck] = now
	allowed := IsAllowed(email)
	m.mu.Unlock()

	if !allowed {
		// 不在名单：静默返回 nil，让对方以为「已发送」。
		// 前端也不会因此拿到任何差别（见 Handler）。
		m.logf("⚠ 管理员登录：收到非授权邮箱的验证码请求（已忽略，不回显）")
		return nil
	}
	if m.mailer == nil {
		return ErrNoMailer
	}

	code, err := randomCode()
	if err != nil {
		return fmt.Errorf("生成验证码失败：%w", err)
	}

	m.mu.Lock()
	m.codes[strings.ToLower(email)] = &Code{
		hash:   hashCode(email, code),
		email:  email,
		expire: now.Add(CodeTTL),
		sentAt: now,
		ip:     ip,
	}
	m.mu.Unlock()

	mailErr := m.mailer.Send(email, "【OKX 量化终端】管理员登录验证码", fmt.Sprintf(
		"你的管理员登录验证码是：%s\n\n%d 分钟内有效，用过即作废。\n"+
			"若非本人操作，说明有人知道了这个邮箱地址并尝试进入交易配置后台 —— "+
			"请忽略本邮件，并检查服务器 80 端口是否暴露在公网。\n\n"+
			"请求来源 IP：%s\n时间：%s\n",
		code, int(CodeTTL.Minutes()), ip, now.Format("2006-01-02 15:04:05")))

	m.mu.Lock()
	if cur, ok := m.codes[strings.ToLower(email)]; ok && cur.hash == hashCode(email, code) {
		if mailErr != nil {
			delete(m.codes, strings.ToLower(email)) // 发失败就别留着这条码
		}
	}
	m.mu.Unlock()

	if mailErr != nil {
		return fmt.Errorf("验证码邮件发送失败：%w", mailErr)
	}
	m.logf("★ 管理员验证码已发送至授权邮箱（%s，5 分钟内有效）", email)
	return nil
}

// Login 校验验证码并签发会话。
//
// 成功后**立刻删掉该验证码**：它是一次性的。这样即使邮件被转发出去，
// 也用不了第二次。
func (m *Manager) Login(email, code, ip, ua string) (*Session, error) {
	email = strings.TrimSpace(email)
	key := strings.ToLower(email)
	now := time.Now()

	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.codes[key]
	if !ok {
		return nil, ErrBadCode
	}
	if now.After(c.expire) {
		delete(m.codes, key)
		return nil, ErrBadCode
	}
	if c.tries >= CodeMaxTry {
		delete(m.codes, key)
		return nil, ErrTooManyTry
	}

	// ★ 恒定时间比较：等长 hex 串，比较耗时与内容无关。
	if subtle.ConstantTimeCompare([]byte(c.hash), []byte(hashCode(email, code))) != 1 {
		c.tries++
		if c.tries >= CodeMaxTry {
			delete(m.codes, key)
			return nil, ErrTooManyTry
		}
		return nil, ErrBadCode
	}
	delete(m.codes, key) // 用过即焚

	tok, err := randomToken()
	if err != nil {
		return nil, fmt.Errorf("签发会话失败：%w", err)
	}
	s := &Session{
		Token: tok, Email: email,
		MadeAt: now, Expire: now.Add(SessionTTL),
		IP: ip, UA: ua,
	}
	m.sessions[tok] = s
	m.sweepLocked(now)
	m.logf("★ 管理员已登录（%s，来自 %s），会话 %d 小时内有效", email, ip, int(SessionTTL.Hours()))
	return s, nil
}

// Verify 校验 token 是否有效，有效则返回会话。
// token 为空 / 不存在 / 已过期 一律返回 nil。
func (m *Manager) Verify(token string) *Session {
	if token == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[token]
	if !ok {
		return nil
	}
	if time.Now().After(s.Expire) {
		delete(m.sessions, token)
		return nil
	}
	return s
}

// Logout 主动注销一个会话
func (m *Manager) Logout(token string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[token]; !ok {
		return false
	}
	delete(m.sessions, token)
	return true
}

// LogoutAll 踢掉所有会话（改密码 / 怀疑泄露时用）
func (m *Manager) LogoutAll() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.sessions)
	m.sessions = map[string]*Session{}
	return n
}

// Sessions 列出当前活跃会话（管理员自查「还有谁在线」）
func (m *Manager) Sessions() []Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	out := make([]Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		if now.After(s.Expire) {
			continue
		}
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MadeAt.After(out[j].MadeAt) })
	return out
}

// sweepLocked 清理过期条目。调用方必须已持锁。
func (m *Manager) sweepLocked(now time.Time) {
	for k, c := range m.codes {
		if now.After(c.expire) {
			delete(m.codes, k)
		}
	}
	for k, s := range m.sessions {
		if now.After(s.Expire) {
			delete(m.sessions, k)
		}
	}
	// lastSend / fails 只保留近 1 小时，避免长期运行把内存吃满
	for k, t := range m.lastSend {
		if now.Sub(t) > time.Hour {
			delete(m.lastSend, k)
		}
	}
}

// Sweep 定期清理（由 cmd 起一个 ticker 调）。
func (m *Manager) Sweep() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked(time.Now())
}
