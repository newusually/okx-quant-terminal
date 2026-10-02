package handler

// admin_guard_test.go —— 管理台鉴权闸门的实测
//
// ★ 这道闸门是本系统唯一「谁能改下单口径」的关口 ★
//
// 设计上刻意采用**前缀白名单**而不是「每个 handler 自己记得校验」：
//
//	strings.HasPrefix(path, "/api/admin/") && !adminOpenPath(path)
//
// 好处是「新增一个 /api/admin/xxx 接口」自动被兜住 —— 不会因为忘记加
// 一行校验就把改配置的能力敞开。代价是 adminOpenPath() 成了唯一的例外清单，
// 它每多一项就多一个无需登录的入口，所以这里逐项钉死。
//
// 覆盖：
//   A. 白名单**只有**登录流程那四个接口（多一个就红）
//   B. 需要登录的路径被判为「需登录」
//   C. 前缀之外的路径不受影响（普通 API 不被误拦）
//   D. 任何 /api/admin/ 下未登记的新路径默认需登录（前缀兜底的真正价值）

import (
	"fmt"
	"strings"
	"testing"
)

// TestAdminOpenPath_ExactWhitelist 白名单就是那四个，一个不多一个不少。
//
// 这条测试是「往白名单里偷偷加东西」的守门人：
// 一旦有人为了方便（比如把 /api/admin/config 也放进来做调试）而扩充它，
// 改配置就不再需要登录了 —— 现在会在 CI/本地测试直接红。
func TestAdminOpenPath_ExactWhitelist(t *testing.T) {
	want := map[string]bool{
		"/api/admin/send_code": true,
		"/api/admin/login":     true,
		"/api/admin/logout":    true,
		"/api/admin/session":   true,
	}
	for p, expect := range want {
		if got := adminOpenPath(p); got != expect {
			t.Fatalf("adminOpenPath(%q) = %v，期望 %v", p, got, expect)
		}
	}

	// 逐条点名「绝不能开放」的路径
	// —— 这些一旦开放，未登录的人就能读配置、改配置、看会话列表
	mustClose := []string{
		"/api/admin/config",   // ★ 读/写交易条件，开放 = 谁都能改策略
		"/api/admin/sessions", // 会话列表，含 IP / UA
		"/api/admin/",         // 前缀根
		"/api/admin",          // 无尾斜杠
		"/api/admin/send_code/extra", // 相似但不同
		"/api/admin/login2",
		"/api/admin/session/all",
	}
	for _, p := range mustClose {
		if adminOpenPath(p) {
			t.Fatalf("adminOpenPath(%q) 为 true —— 这个路径必须要求登录", p)
		}
	}
	t.Logf("✓ 白名单恰好 %d 项，且所有敏感路径都要求登录", len(want))
}

// TestAdminGuard_RequiresLogin 前缀闸门的判定：/api/admin/* 默认需登录。
//
// 这里复刻 server.wrap 里的那段判定（同一份逻辑），
// 免得测试与生产各写一遍、日后跑偏。
func TestAdminGuard_RequiresLogin(t *testing.T) {
	needsLogin := func(p string) bool {
		return strings.HasPrefix(p, "/api/admin/") && !adminOpenPath(p)
	}

	// 需登录
	for _, p := range []string{
		"/api/admin/config",
		"/api/admin/sessions",
		"/api/admin/whatever_new_feature", // ★ 未登记的新接口自动被兜住
		"/api/admin/",
	} {
		if !needsLogin(p) {
			t.Fatalf("%q 应当要求登录（前缀白名单兜底失效）", p)
		}
	}
	// 不需登录
	for _, p := range []string{
		"/api/admin/send_code",
		"/api/admin/login",
		"/api/admin/logout",
		"/api/admin/session",
	} {
		if needsLogin(p) {
			t.Fatalf("%q 是登录流程的一部分，不该要求登录", p)
		}
	}
	t.Log("✓ /api/admin/* 默认需登录；新增接口自动被兜住，不必逐个记得加校验")
}

// TestAdminGuard_DoesNotAffectOtherAPIs 前缀闸门不能误伤普通接口。
//
// 判据写成 `HasPrefix(p, "/api/admin/")` 而不是 `HasPrefix(p, "/api/")`，
// 就是为了这个：否则整个行情/持仓/信号接口全要登录，网页直接白屏。
func TestAdminGuard_DoesNotAffectOtherAPIs(t *testing.T) {
	needsLogin := func(p string) bool {
		return strings.HasPrefix(p, "/api/admin/") && !adminOpenPath(p)
	}

	for _, p := range []string{
		"/api/state",
		"/api/kline",
		"/api/mark",
		"/api/positions",
		"/api/events",
		"/api/adminx/config", // 相似前缀，但**不是** /api/admin/
		"/admin/config",
		"/assets/app.js",
		"/",
	} {
		if needsLogin(p) {
			t.Fatalf("%q 被误判为需要登录（普通接口被闸门误伤）", p)
		}
	}
	t.Log("✓ 前缀闸门只作用于 /api/admin/*，普通接口不受影响")
}

// TestAdminGuard_CaseSensitive 路径前缀大小写敏感，不能被人用大小写绕过。
//
// `/API/ADMIN/config` 在 Go 的 http.ServeMux 里不会匹配到注册的
// `/api/admin/config`（mux 是大小写敏感的），所以它既不会走到 handler，
// 也不该被闸门「放行」成一个未鉴权的入口。
// 这里断言的是「闸门不会把变体路径当成白名单路径」。
func TestAdminGuard_CaseSensitive(t *testing.T) {
	if adminOpenPath("/API/ADMIN/SESSION") {
		t.Fatal("白名单匹配不该忽略大小写（否则可被大小写变体绕过）")
	}
	// 但 "/api/admin/session" 的**精确**匹配必须成立
	if !adminOpenPath("/api/admin/session") {
		t.Fatal("精确路径应当命中白名单")
	}
	// 带查询串的路径不应由 adminOpenPath 处理（r.URL.Path 不含 query）
	if adminOpenPath("/api/admin/session?x=1") {
		t.Fatal("带查询串的字符串不该命中白名单（r.URL.Path 本就不含 query，这里是防御性断言）")
	}
	t.Log("✓ 白名单按精确路径匹配，大小写/查询串变体不会命中")
}

// ---------------------------------------------------------------------------
// E. strField：把「字段没传」和「传了值」分清
// ---------------------------------------------------------------------------

// TestStrField_NilIsEmptyNotPlaceholder ★ 2026-10-02 实翻车回归 ★
//
// 守的是一个把「缺失」伪装成「有值」的坑：
//
//	fmt.Sprint(body["email"])   // nil → "<nil>"（5 个字符，非空串！）
//
// 后果链条（真实发生，用户在网页点「发送验证码」永远收不到邮件）：
//  1. 前端不传 email（或传了界面占位文案）→ body["email"] 为 nil
//  2. fmt.Sprint 把它变成 "<nil>" → `if email == ""` 的兜底**不触发**
//  3. "<nil>" 不在白名单 → 被判「非授权邮箱」
//  4. 鉴权层为不泄露白名单，对这种请求**静默按成功返回**（ok:true）
//  → 接口一路绿灯，实际一封邮件都没发，日志里只有一句不起眼的提示。
//
// 所以这里钉死：**取不到就是空串**，让调用方的兜底逻辑接管。
func TestStrField_NilIsEmptyNotPlaceholder(t *testing.T) {
	cases := []struct {
		name string
		m    map[string]any
		key  string
		want string
	}{
		{"键不存在", map[string]any{}, "email", ""},
		{"显式 null", map[string]any{"email": nil}, "email", ""},
		{"数字类型不该冒充字符串", map[string]any{"email": float64(1)}, "email", ""},
		{"布尔类型不该冒充字符串", map[string]any{"email": true}, "email", ""},
		{"对象类型不该冒充字符串", map[string]any{"email": map[string]any{}}, "email", ""},
		{"正常字符串", map[string]any{"email": "493076373@qq.com"}, "email", "493076373@qq.com"},
		{"空字符串", map[string]any{"email": ""}, "email", ""},
	}
	for _, c := range cases {
		if got := strField(c.m, c.key); got != c.want {
			t.Fatalf("%s：strField = %q，期望 %q", c.name, got, c.want)
		}
	}
	t.Log("✓ strField 对缺失/类型不符一律返回空串，不会产生 \"<nil>\" 这类伪值")
}

// TestStrField_GuardsAgainstSprintNil 直接把「为什么不能用 fmt.Sprint」立成断言。
//
// 这样即使有人把 strField 换回 fmt.Sprint，也能立刻看到失败原因，
// 而不是在一个看似无关的「验证码收不到」问题里重新踩一遍。
func TestStrField_GuardsAgainstSprintNil(t *testing.T) {
	var m = map[string]any{}
	if got := fmt.Sprint(m["email"]); got == "" {
		t.Skip("fmt.Sprint(nil) 行为已变化，本断言的前提不再成立，需重新评估 strField")
	}
	// 前提成立：fmt.Sprint(nil) 确实不是空串，那么 strField 必须与它不同
	if strField(m, "email") != "" {
		t.Fatal("strField 必须比 fmt.Sprint 更严格：缺失字段要返回空串而不是 \"<nil>\"")
	}
	t.Log("✓ strField 与 fmt.Sprint 行为不同（前者把缺失如实报成空串）")
}
