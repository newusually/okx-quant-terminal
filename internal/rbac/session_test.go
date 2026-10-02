package rbac

// session_test.go —— 管理员鉴权链路的实测
//
// ★ 为什么这个包必须有单测 ★
//   这是本系统**唯一**一道「谁能改下单口径」的门。手工点页面只能证明「我能进」，
//   证明不了「别人进不来」。下面每一条测的都是**拒绝**路径：
//   白名单外的邮箱、过期码、重放、猜错、伪造 token。
//   拒绝路径没测过的鉴权，等于没做鉴权。
//
// 覆盖：
//   A. 白名单之外一律不发码，但**对外表现与授权邮箱完全一致**（不回显）
//   B. 验证码一次性：用过即焚，重放要失败
//   C. 验证码过期作废
//   D. 连错 5 次作废（而不是无限猜）
//   E. 会话：签发 / 校验 / 注销 / 过期 / 伪造 token
//   F. 发码冷却：60 秒内第二次要挡
//   G. Mailer 失败时不留残留验证码
//   H. token 必须来自 crypto/rand（长度与字符集）

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMailer 收集发出的邮件，用来从「收件箱」里把验证码抠出来。
//
// 走接口注入而不是直接读 Manager 内部字段：这样测的是**真实链路**
// （Send → code 落库），而不是「我改了内部状态所以它应该成立」。
type fakeMailer struct {
	mu     sync.Mutex
	sent   []fakeMail
	failOn bool
}

type fakeMail struct {
	to, subject, body string
}

func (f *fakeMailer) Send(to, subject, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOn {
		return errors.New("smtp 挂了")
	}
	f.sent = append(f.sent, fakeMail{to, subject, body})
	return nil
}

func (f *fakeMailer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

// lastCode 从最后一封邮件正文里抠出 6 位验证码
func (f *fakeMailer) lastCode(t *testing.T) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		t.Fatal("没有发出任何邮件，抠不出验证码")
	}
	body := f.sent[len(f.sent)-1].body
	// 正文形如「你的管理员登录验证码是：123456」
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "验证码是：") {
			idx := strings.Index(line, "验证码是：")
			code := strings.TrimSpace(line[idx+len("验证码是："):])
			if len(code) == 6 {
				return code
			}
		}
	}
	t.Fatalf("邮件正文里找不到 6 位验证码：\n%s", body)
	return ""
}

// newTestManager 一个带假 mailer 的管理器
func newTestManager() (*Manager, *fakeMailer) {
	fm := &fakeMailer{}
	return NewManager(fm, nil), fm
}

// ---------------------------------------------------------------------------
// A. 白名单：不回显
// ---------------------------------------------------------------------------

// TestAllowedEmailIsHardcoded 授权邮箱就是用户给的那一个。
//
// 这条看着像废话，但它守的是「谁能改我的钱」这个根 —— 有人为了「方便测试」
// 把它改成通配或读环境变量，这里立刻红。
func TestAllowedEmailIsHardcoded(t *testing.T) {
	if AllowedEmail() != "493076373@qq.com" {
		t.Fatalf("授权邮箱被改成了 %q", AllowedEmail())
	}
	if !IsAllowed("493076373@qq.com") {
		t.Fatal("本人邮箱应当被授权")
	}
	// 大小写与空白宽容
	if !IsAllowed("  493076373@QQ.COM  ") {
		t.Fatal("邮箱比对应对大小写与空白宽容")
	}
	t.Log("✓ 授权邮箱硬编码为 493076373@qq.com，大小写/空白宽容")
}

// TestSendCode_NonAllowedIsSilent 非授权邮箱：对外一模一样，但**不发信**。
//
// 这是防「探测哪些邮箱被授权」的关键：如果非授权邮箱拿到错误、授权邮箱拿到成功，
// 攻击者一个请求就能确认白名单。所以这里断言
// ① 返回 nil（与授权邮箱同样的成功语义）② 收件箱里什么都没有。
func TestSendCode_NonAllowedIsSilent(t *testing.T) {
	m, fm := newTestManager()

	err := m.SendCode("attacker@evil.com", "1.2.3.4")
	if err != nil {
		t.Fatalf("非授权邮箱应当对外返回成功（防探测），实际报错：%v", err)
	}
	if fm.count() != 0 {
		t.Fatalf("非授权邮箱不该真发信，实际发了 %d 封", fm.count())
	}
	t.Log("✓ 非授权邮箱：对外返回成功、实际不发信（无法用接口探测白名单）")
}

// TestSendCode_AllowedSends 授权邮箱正常发信，且能从邮件里拿到可用验证码。
func TestSendCode_AllowedSends(t *testing.T) {
	m, fm := newTestManager()

	if err := m.SendCode(AllowedEmail(), "1.2.3.4"); err != nil {
		t.Fatalf("授权邮箱发码应当成功：%v", err)
	}
	if fm.count() != 1 {
		t.Fatalf("应当恰好发 1 封，实际 %d", fm.count())
	}
	code := fm.lastCode(t)
	if _, err := m.Login(AllowedEmail(), code, "1.2.3.4", "UA"); err != nil {
		t.Fatalf("用邮件里的验证码登录失败：%v", err)
	}
	t.Log("✓ 授权邮箱发信 → 邮件里的码可直接登录")
}

// ---------------------------------------------------------------------------
// B. 一次性
// ---------------------------------------------------------------------------

// TestLogin_CodeIsSingleUse 同一个验证码不能用第二次。
//
// 邮件可能被转发、被云邮箱的「安全扫描」抓去点，一次性是基本要求。
func TestLogin_CodeIsSingleUse(t *testing.T) {
	m, fm := newTestManager()
	if err := m.SendCode(AllowedEmail(), "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	code := fm.lastCode(t)

	if _, err := m.Login(AllowedEmail(), code, "1.2.3.4", "UA"); err != nil {
		t.Fatalf("第一次登录应当成功：%v", err)
	}
	// 重放同一个码 —— 必须失败
	if _, err := m.Login(AllowedEmail(), code, "1.2.3.4", "UA"); !errors.Is(err, ErrBadCode) {
		t.Fatalf("验证码重放应当失败（用过即焚），实际 err=%v", err)
	}
	t.Log("✓ 验证码用过即焚，重放被拒")
}

// ---------------------------------------------------------------------------
// C/D. 过期与试错
// ---------------------------------------------------------------------------

// TestLogin_ExpiredCode 过期的码作废。
//
// 直接改 expire 而不是 sleep 5 分钟 —— 单测不能被子测拖成 5 分钟。
// 这样改的是「同一段判定逻辑读的那个字段」，等价性成立。
func TestLogin_ExpiredCode(t *testing.T) {
	m, fm := newTestManager()
	if err := m.SendCode(AllowedEmail(), "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	code := fm.lastCode(t)

	m.mu.Lock()
	m.codes[strings.ToLower(AllowedEmail())].expire = time.Now().Add(-time.Second)
	m.mu.Unlock()

	if _, err := m.Login(AllowedEmail(), code, "1.2.3.4", "UA"); !errors.Is(err, ErrBadCode) {
		t.Fatalf("过期验证码应当被拒，实际 err=%v", err)
	}
	// 过期条目应当被顺手删掉
	m.mu.Lock()
	_, still := m.codes[strings.ToLower(AllowedEmail())]
	m.mu.Unlock()
	if still {
		t.Fatal("过期验证码应当被清理掉")
	}
	t.Log("✓ 过期验证码作废并被清理")
}

// TestLogin_TooManyTries 连错 5 次作废，且第 5 次报「次数过多」。
//
// 6 位数字有 100 万种，不限次数总能撞开。必须钉死。
func TestLogin_TooManyTries(t *testing.T) {
	m, fm := newTestManager()
	if err := m.SendCode(AllowedEmail(), "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	real := fm.lastCode(t)
	wrong := "000000"
	if wrong == real {
		wrong = "111111"
	}

	var last error
	for i := 0; i < CodeMaxTry; i++ {
		_, last = m.Login(AllowedEmail(), wrong, "1.2.3.4", "UA")
	}
	if !errors.Is(last, ErrTooManyTry) {
		t.Fatalf("第 %d 次错码应当报 ErrTooManyTry，实际 %v", CodeMaxTry, last)
	}
	// 作废后即使拿对的码也进不去
	if _, err := m.Login(AllowedEmail(), real, "1.2.3.4", "UA"); !errors.Is(err, ErrBadCode) {
		t.Fatalf("试错超限后应当整条作废（对的码也无效），实际 %v", err)
	}
	t.Logf("✓ 连错 %d 次整条作废（防爆破）", CodeMaxTry)
}

// TestLogin_BadCodeIncrementsTries 错一次不应直接作废，只是计数 +1。
func TestLogin_BadCodeIncrementsTries(t *testing.T) {
	m, fm := newTestManager()
	if err := m.SendCode(AllowedEmail(), "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	real := fm.lastCode(t)
	wrong := "000000"
	if wrong == real {
		wrong = "111111"
	}

	if _, err := m.Login(AllowedEmail(), wrong, "1.2.3.4", "UA"); !errors.Is(err, ErrBadCode) {
		t.Fatalf("错一次应当是 ErrBadCode：%v", err)
	}
	m.mu.Lock()
	tries := m.codes[strings.ToLower(AllowedEmail())].tries
	m.mu.Unlock()
	if tries != 1 {
		t.Fatalf("错一次 tries 应为 1，实际 %d", tries)
	}
	// 对的码仍然能进
	if _, err := m.Login(AllowedEmail(), real, "1.2.3.4", "UA"); err != nil {
		t.Fatalf("错一次后对的码应当仍可用：%v", err)
	}
	t.Log("✓ 错一次只计数，不清空验证码")
}

// TestLogin_NoCodeAtAll 从没申请过验证码就直接登录 → 拒。
func TestLogin_NoCodeAtAll(t *testing.T) {
	m, _ := newTestManager()
	if _, err := m.Login(AllowedEmail(), "123456", "1.2.3.4", "UA"); !errors.Is(err, ErrBadCode) {
		t.Fatalf("没申请过验证码应当被拒，实际 %v", err)
	}
	t.Log("✓ 没有待验证的码时登录被拒")
}

// ---------------------------------------------------------------------------
// E. 会话
// ---------------------------------------------------------------------------

// TestSession_Lifecycle 签发 → 校验 → 注销。
func TestSession_Lifecycle(t *testing.T) {
	m, fm := newTestManager()
	if err := m.SendCode(AllowedEmail(), "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	sess, err := m.Login(AllowedEmail(), fm.lastCode(t), "1.2.3.4", "UA/1.0")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Token == "" {
		t.Fatal("会话 token 不该为空")
	}
	if sess.Email != AllowedEmail() || sess.IP != "1.2.3.4" || sess.UA != "UA/1.0" {
		t.Fatalf("会话记录字段不对：%+v", sess)
	}

	got := m.Verify(sess.Token)
	if got == nil || got.Token != sess.Token {
		t.Fatal("签发的 token 应当能校验通过")
	}

	if !m.Logout(sess.Token) {
		t.Fatal("注销已存在的会话应当返回 true")
	}
	if m.Verify(sess.Token) != nil {
		t.Fatal("注销后 token 必须立刻失效")
	}
	if m.Logout(sess.Token) {
		t.Fatal("重复注销应当返回 false")
	}
	t.Log("✓ 会话 签发→校验→注销 全链路正确")
}

// TestSession_ForgedToken 伪造 token 一律拒绝。
func TestSession_ForgedToken(t *testing.T) {
	m, _ := newTestManager()
	bad := []string{
		"",
		"deadbeef",
		strings.Repeat("a", 64),
		"'; DROP TABLE sessions; --",
	}
	for _, tk := range bad {
		if m.Verify(tk) != nil {
			t.Fatalf("伪造 token %q 不该通过校验", tk)
		}
	}
	t.Log("✓ 伪造/空 token 全部被拒")
}

// TestSession_Expired 过期的会话失效并被清理。
func TestSession_Expired(t *testing.T) {
	m, fm := newTestManager()
	if err := m.SendCode(AllowedEmail(), "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	sess, err := m.Login(AllowedEmail(), fm.lastCode(t), "1.2.3.4", "UA")
	if err != nil {
		t.Fatal(err)
	}

	m.mu.Lock()
	m.sessions[sess.Token].Expire = time.Now().Add(-time.Second)
	m.mu.Unlock()

	if m.Verify(sess.Token) != nil {
		t.Fatal("过期会话必须失效")
	}
	m.mu.Lock()
	_, still := m.sessions[sess.Token]
	m.mu.Unlock()
	if still {
		t.Fatal("过期会话应当被清理掉")
	}
	t.Log("✓ 过期会话失效并被清理")
}

// TestSession_LogoutAll 一键踢掉所有会话（怀疑泄露时的应急手段）。
func TestSession_LogoutAll(t *testing.T) {
	m, fm := newTestManager()

	tokens := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		if err := m.SendCode(AllowedEmail(), "1.2.3.4"); err != nil {
			t.Fatal(err)
		}
		// 绕开冷却：直接清 lastSend（模拟隔了一段时间）
		m.mu.Lock()
		m.lastSend = map[string]time.Time{}
		m.mu.Unlock()
		s, err := m.Login(AllowedEmail(), fm.lastCode(t), "1.2.3.4", "UA")
		if err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, s.Token)
	}

	if n := len(m.Sessions()); n != 3 {
		t.Fatalf("应有 3 个活跃会话，实际 %d", n)
	}
	if n := m.LogoutAll(); n != 3 {
		t.Fatalf("LogoutAll 应返回 3，实际 %d", n)
	}
	for _, tk := range tokens {
		if m.Verify(tk) != nil {
			t.Fatal("LogoutAll 之后旧 token 必须全部失效")
		}
	}
	if len(m.Sessions()) != 0 {
		t.Fatal("LogoutAll 之后不该还有会话")
	}
	t.Log("✓ LogoutAll 一次性踢掉全部会话")
}

// TestSession_Sweep 定期清理会把过期会话与陈旧的冷却记录扫掉。
func TestSession_Sweep(t *testing.T) {
	m, fm := newTestManager()
	if err := m.SendCode(AllowedEmail(), "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	sess, err := m.Login(AllowedEmail(), fm.lastCode(t), "1.2.3.4", "UA")
	if err != nil {
		t.Fatal(err)
	}

	m.mu.Lock()
	m.sessions[sess.Token].Expire = time.Now().Add(-time.Hour)
	for k := range m.lastSend {
		m.lastSend[k] = time.Now().Add(-2 * time.Hour)
	}
	m.mu.Unlock()

	m.Sweep()

	m.mu.Lock()
	nSess := len(m.sessions)
	nSend := len(m.lastSend)
	m.mu.Unlock()
	if nSess != 0 {
		t.Fatalf("Sweep 后过期会话应清空，实际 %d", nSess)
	}
	if nSend != 0 {
		t.Fatalf("Sweep 后 2 小时前的冷却记录应清空，实际 %d", nSend)
	}
	t.Log("✓ Sweep 清理过期会话与陈旧冷却记录（长期运行不涨内存）")
}

// ---------------------------------------------------------------------------
// F. 冷却
// ---------------------------------------------------------------------------

// TestSendCode_Cooldown 同一 邮箱+IP 60 秒内只能发一次。
func TestSendCode_Cooldown(t *testing.T) {
	m, fm := newTestManager()
	if err := m.SendCode(AllowedEmail(), "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	if err := m.SendCode(AllowedEmail(), "1.2.3.4"); !errors.Is(err, ErrTooOften) {
		t.Fatalf("60 秒内第二次发码应当被冷却挡住，实际 %v", err)
	}
	if fm.count() != 1 {
		t.Fatalf("冷却期内不该真发第二封，实际发了 %d 封", fm.count())
	}

	// 换 IP 则不受影响（冷却键是 邮箱+IP）
	if err := m.SendCode(AllowedEmail(), "5.6.7.8"); err != nil {
		t.Fatalf("换 IP 应当可以再发：%v", err)
	}
	// 非授权邮箱同样要挡（否则可被拿来刷发信逻辑）
	if err := m.SendCode("attacker@evil.com", "5.6.7.8"); err != nil {
		t.Fatalf("非授权邮箱首次请求对外仍返回成功：%v", err)
	}
	if err := m.SendCode("attacker@evil.com", "5.6.7.8"); !errors.Is(err, ErrTooOften) {
		t.Fatalf("非授权邮箱也要吃冷却（防被拿去刷），实际 %v", err)
	}
	t.Log("✓ 发码冷却按 邮箱+IP 生效，且对非授权邮箱同样生效")
}

// ---------------------------------------------------------------------------
// G. 发信失败不留残码
// ---------------------------------------------------------------------------

// TestSendCode_MailFailureLeavesNoCode 发信失败时不能留下一条「已知的验证码」。
//
// 否则会出现：SMTP 挂了 → 用户没收到码 → 但内存里躺着一个码。
// 万一这个码被猜到（或日志里泄漏过），就成了后门。
func TestSendCode_MailFailureLeavesNoCode(t *testing.T) {
	m, fm := newTestManager()
	// 先成功发一次，拿到一个真实码
	if err := m.SendCode(AllowedEmail(), "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	oldCode := fm.lastCode(t)

	// 换 IP 再发一次，这次让 SMTP 失败
	fm.failOn = true
	m.mu.Lock()
	m.lastSend = map[string]time.Time{}
	m.mu.Unlock()
	if err := m.SendCode(AllowedEmail(), "9.9.9.9"); err == nil {
		t.Fatal("SMTP 失败时应当返回错误")
	}

	// 失败那次的码必须已经清掉：拿「上一轮的旧码」不该能进
	m.mu.Lock()
	_, has := m.codes[strings.ToLower(AllowedEmail())]
	m.mu.Unlock()
	if has {
		t.Fatal("发信失败后仍残留验证码记录（可能成为后门）")
	}
	if _, err := m.Login(AllowedEmail(), oldCode, "1.2.3.4", "UA"); err == nil {
		t.Fatal("发信失败后旧码不该还能用")
	}
	t.Log("✓ SMTP 失败时清除验证码，不留后门")
}

// TestSendCode_NoMailerConfigured 没配 SMTP 时明确报错（而不是假装发出去了）。
func TestSendCode_NoMailerConfigured(t *testing.T) {
	m := NewManager(nil, nil) // 没有 mailer
	if err := m.SendCode(AllowedEmail(), "1.2.3.4"); !errors.Is(err, ErrNoMailer) {
		t.Fatalf("未配置邮件服务时应当报 ErrNoMailer（不能让用户干等），实际 %v", err)
	}
	// 但非授权邮箱仍然静默成功（不泄露「服务没配」这个信息）
	if err := m.SendCode("x@y.com", "1.2.3.4"); err != nil {
		t.Fatalf("非授权邮箱不该暴露「服务未配置」：%v", err)
	}
	t.Log("✓ 未配置 SMTP：授权邮箱得到明确错误，非授权邮箱仍然静默")
}

// ---------------------------------------------------------------------------
// H. token 质量
// ---------------------------------------------------------------------------

// TestToken_IsRandomHex64 会话 token 必须是 64 位 hex（32 字节随机）。
//
// 用时间戳或 math/rand 生成 token 的话，会话可被预测 = 鉴权形同虚设。
// 这里除了检查形状，还检查「多次生成不重复」。
func TestToken_IsRandomHex64(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		tk, err := randomToken()
		if err != nil {
			t.Fatalf("生成 token 失败：%v", err)
		}
		if len(tk) != 64 {
			t.Fatalf("token 长度应为 64，实际 %d（%q）", len(tk), tk)
		}
		for _, c := range tk {
			if !strings.ContainsRune("0123456789abcdef", c) {
				t.Fatalf("token 含非 hex 字符 %q：%q", c, tk)
			}
		}
		if seen[tk] {
			t.Fatal("token 出现重复 —— 随机源有问题")
		}
		seen[tk] = true
	}
	t.Log("✓ token 为 64 位 hex 且 200 次无重复")
}

// TestRandomCode_IsSixDigits 验证码必须是 6 位数字（含前导 0）。
func TestRandomCode_IsSixDigits(t *testing.T) {
	for i := 0; i < 500; i++ {
		c, err := randomCode()
		if err != nil {
			t.Fatal(err)
		}
		if len(c) != 6 {
			t.Fatalf("验证码应为 6 位，实际 %q", c)
		}
		for _, r := range c {
			if r < '0' || r > '9' {
				t.Fatalf("验证码含非数字字符：%q", c)
			}
		}
	}
	t.Log("✓ 验证码恒为 6 位数字（含前导 0）")
}

// TestHashCode_NotPlaintext 内部存的必须是 hash，不是明文码。
//
// 这条守的是「内存被 dump 也不能直接拿到可用验证码」。
func TestHashCode_NotPlaintext(t *testing.T) {
	m, fm := newTestManager()
	if err := m.SendCode(AllowedEmail(), "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	code := fm.lastCode(t)

	m.mu.Lock()
	c := m.codes[strings.ToLower(AllowedEmail())]
	m.mu.Unlock()
	if c == nil {
		t.Fatal("验证码应当已落库")
	}
	if strings.Contains(c.hash, code) {
		t.Fatalf("存储里含明文验证码 %q（应只存 sha256）", code)
	}
	if c.hash != hashCode(AllowedEmail(), code) {
		t.Fatal("hash 算法与校验路径不一致")
	}
	// 大小写/空白不同的邮箱应得到同一个 hash（与校验路径一致）
	if hashCode("  493076373@QQ.COM ", code) != hashCode(AllowedEmail(), code) {
		t.Fatal("hashCode 未对邮箱做规范化，会导致登录时比对不上")
	}
	t.Log("✓ 只存 sha256 摘要，不存明文验证码；邮箱规范化一致")
}

// ---------------------------------------------------------------------------
// 并发
// ---------------------------------------------------------------------------

// TestConcurrent_LoginSameCode 并发用同一个码登录：只能有 1 个成功。
//
// 用户可能手抖点两下「验证并登录」，或者攻击者并发重放。
// 一次性语义必须在并发下也成立 —— 加锁不严的话这条会红。
func TestConcurrent_LoginSameCode(t *testing.T) {
	m, fm := newTestManager()
	if err := m.SendCode(AllowedEmail(), "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	code := fm.lastCode(t)

	const N = 12
	var wg sync.WaitGroup
	var mu sync.Mutex
	okCount := 0
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.Login(AllowedEmail(), code, "1.2.3.4", "UA"); err == nil {
				mu.Lock()
				okCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if okCount != 1 {
		t.Fatalf("并发用同一个码只能成功 1 次，实际成功 %d 次（一次性语义在并发下破了）", okCount)
	}
	t.Log("✓ 并发重放同一验证码只成功 1 次")
}

// TestConcurrent_SendCodeCooldown 并发发码：冷却下只能真发一封。
func TestConcurrent_SendCodeCooldown(t *testing.T) {
	m, fm := newTestManager()
	const N = 10
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.SendCode(AllowedEmail(), "1.2.3.4")
		}()
	}
	wg.Wait()
	if fm.count() != 1 {
		t.Fatalf("并发发码在冷却下只能真发 1 封，实际 %d 封", fm.count())
	}
	t.Log("✓ 并发发码被冷却挡住，只发 1 封")
}
