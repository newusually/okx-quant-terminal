package handler

// api_admin_config.go —— 管理员配置接口（★ 2026-10-02 八期）
//
// 这套接口能**改动真实下单口径**（买入条件 / 加仓条件 / 金额），所以：
//   · 全部挂在 /api/admin/ 前缀下，由 Server.wrap 统一拦鉴权（见 server.go）；
//   · 除了「发验证码 / 登录」这两个入口，其余一律要求有效会话；
//   · 保存走 service.StrategyWriter 的原子定点替换，写完靠既有热插拔生效。
//
// ★ 为什么配置读接口也要鉴权 ★
//   /api/state 已经暴露了全部策略参数（本来就给大屏看的）。但管理台要读的是
//   「可以编辑的完整视图 + 每个条件的开关状态」，把「原始 JSON 文本」也给出去
//   方便排错 —— 那里面可能有别的东西（如 AI key 的引用）。
//   宁严不松：管理台自己的读接口也要求登录。

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"finally-main/internal/rbac"
	"finally-main/internal/service"
)

// readJSONBody 读一个 JSON 请求体并解成 map。
//
// 限流：最多 64KB。这套接口的入参全是几个阈值数字，正常不会超过 1KB；
// 不设上限的话，一个坏客户端 POST 一个几百 MB 的 body 就能把这台
// 2 核 1.97G 的机器打趴。
func readJSONBody(r *http.Request) (map[string]any, error) {
	if r.Body == nil {
		return map[string]any{}, nil
	}
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		if err == io.EOF {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("请求体不是合法 JSON：%w", err)
	}
	// json.Number 统一转成 float64，让 service.ParsePatch 的 toFloat 只有一种类型要认
	out := make(map[string]any, len(m))
	for k, v := range m {
		if n, ok := v.(json.Number); ok {
			if f, err := n.Float64(); err == nil {
				out[k] = f
				continue
			}
			out[k] = n.String()
			continue
		}
		out[k] = v
	}
	return out, nil
}

// tokenFrom 取出会话 token：优先 cookie（浏览器），其次 Authorization / token 参数
// （方便用 curl 排错）。
func tokenFrom(r *http.Request) string {
	if c, err := r.Cookie(rbac.CookieName); err == nil && c.Value != "" {
		return c.Value
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	if t := r.URL.Query().Get("token"); t != "" {
		return t
	}
	return ""
}

// adminEmailMasked 把授权邮箱打码成 4****373@qq.com 形式。
//
// ★ 为什么打码而不是直接回显 ★
//   发码接口是**公开**的（要登录才能用就没法登录了）。如果它对任意请求都回显
//   「验证码已发送到 493076373@qq.com」，等于把管理员账号白送给任何扫端口的人。
//   所以只回显打码后的形式，足够本人确认「是我的邮箱」，不够让攻击者拿走。
func adminEmailMasked() string {
	e := rbac.AllowedEmail()
	at := strings.IndexByte(e, '@')
	if at <= 1 {
		return "***"
	}
	local := e[:at]
	if len(local) <= 4 {
		return local[:1] + "***" + e[at:]
	}
	return local[:3] + "****" + local[len(local)-3:] + e[at:]
}

// clientIP 取请求来源 IP（用于日志与冷却）。X-Forwarded-For 优先 ——
// 本服务前面挂着 Apache 反代，直连地址永远会是 127.0.0.1。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xr := r.Header.Get("X-Real-IP"); xr != "" {
		return strings.TrimSpace(xr)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------------------------------------------------------------------------
// ① 发验证码
// ---------------------------------------------------------------------------

// handleAdminSendCode POST /api/admin/send_code
//
// 入参：{"email": "..."}（可选，默认就用授权邮箱）
//
// ★ 返回语义是「统一」的：无论邮箱在不在白名单，只要没被冷却拦住，
//   一律返回 {"ok":true,"to":"4****373@qq.com"}。不回显「这个邮箱不授权」，
//   避免被用来枚举管理员邮箱。
func (s *Server) handleAdminSendCode(w http.ResponseWriter, r *http.Request) (any, error) {
	if r.Method != http.MethodPost {
		return nil, fmt.Errorf("只接受 POST")
	}
	if s.auth == nil {
		return nil, fmt.Errorf("鉴权组件未初始化")
	}
	body, err := readJSONBody(r)
	if err != nil {
		return nil, err
	}
	email := strings.TrimSpace(fmt.Sprint(body["email"]))
	if email == "" {
		email = rbac.AllowedEmail()
	}

	ip := clientIP(r)
	if err := s.auth.SendCode(email, ip); err != nil {
		// 冷却 / 邮件服务未配置 这两类是**真实错误**，要如实告诉调用方
		// （本人需要知道「邮件没发出去」才能去配 SMTP），但它们不泄露白名单信息。
		if err == rbac.ErrTooOften {
			return map[string]any{
				"ok":    false,
				"error": "发送太频繁，请 1 分钟后再试",
			}, nil
		}
		if err == rbac.ErrNoMailer {
			return map[string]any{
				"ok":    false,
				"error": "邮件服务未配置（服务端缺少 SMTP 授权码），无法发送验证码",
			}, nil
		}
		return map[string]any{"ok": false, "error": err.Error()}, nil
	}

	s.logf("管理员验证码请求：来源 %s → %s", ip, adminEmailMasked())
	return map[string]any{
		"ok": true,
		// 打码回显：本人能认出是自己的邮箱，攻击者拿不到完整地址
		"to":        adminEmailMasked(),
		"ttlSec":    int(rbac.CodeTTL.Seconds()),
		"serverNow": time.Now().UnixMilli(),
	}, nil
}

// ---------------------------------------------------------------------------
// ② 登录 / 登出 / 会话
// ---------------------------------------------------------------------------

// handleAdminLogin POST /api/admin/login
//
// 入参：{"email":"...","code":"123456"}
// 成功：下发 HttpOnly cookie，body 里也回一份 token（给非浏览器客户端用）。
func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) (any, error) {
	if r.Method != http.MethodPost {
		return nil, fmt.Errorf("只接受 POST")
	}
	if s.auth == nil {
		return nil, fmt.Errorf("鉴权组件未初始化")
	}
	body, err := readJSONBody(r)
	if err != nil {
		return nil, err
	}
	email := strings.TrimSpace(fmt.Sprint(body["email"]))
	code := strings.TrimSpace(fmt.Sprint(body["code"]))
	if email == "" {
		email = rbac.AllowedEmail()
	}
	if code == "" {
		return map[string]any{"ok": false, "error": "请输入验证码"}, nil
	}

	ip := clientIP(r)
	sess, err := s.auth.Login(email, code, ip, r.UserAgent())
	if err != nil {
		// ★ 不区分「验证码错」与「邮箱不授权」—— 都是「验证码错误或已失效」
		msg := "验证码错误或已失效"
		if err == rbac.ErrTooManyTry {
			msg = "验证码错误次数过多，请重新获取"
		}
		s.logf("管理员登录失败（来源 %s）：%v", ip, err)
		return map[string]any{"ok": false, "error": msg}, nil
	}

	// HttpOnly + SameSite=Strict：
	//   HttpOnly  → JS 读不到，XSS 也偷不走
	//   SameSite=Strict → 别的站点发起的请求带不上这个 cookie（CSRF 防护）
	//   Secure 不设：本服务跑在内网 HTTP（Apache 反代），设了 Secure 浏览器就不会回传
	// Path 覆盖整个 /api/admin/，避免被别的路径带上
	http.SetCookie(w, &http.Cookie{
		Name:     rbac.CookieName,
		Value:    sess.Token,
		Path:     "/api/admin/",
		MaxAge:   int(rbac.SessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	return map[string]any{
		"ok":        true,
		"token":     sess.Token,
		"email":     adminEmailMasked(),
		"expireTs":  sess.Expire.UnixMilli(),
		"ttlSec":    int(rbac.SessionTTL.Seconds()),
		"serverNow": time.Now().UnixMilli(),
	}, nil
}

// handleAdminLogout POST /api/admin/logout
func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) (any, error) {
	if s.auth == nil {
		return nil, fmt.Errorf("鉴权组件未初始化")
	}
	tok := tokenFrom(r)
	ok := s.auth.Logout(tok)
	// 顺手把 cookie 也清掉（浏览器端立刻失效）
	http.SetCookie(w, &http.Cookie{
		Name: rbac.CookieName, Value: "", Path: "/api/admin/",
		MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	return map[string]any{"ok": true, "cleared": ok}, nil
}

// handleAdminSession GET /api/admin/session
//
// 前端打开对话框时先探一下「我这会话还在不在」，在就不用重新登录。
func (s *Server) handleAdminSession(w http.ResponseWriter, r *http.Request) (any, error) {
	// ★ 未登录也带上打码邮箱：登录对话框要显示「验证码会发到哪儿」，
	//   否则前端只能写死一个占位符，用户没法确认是不是自己那个邮箱。
	//   打码形式（4****373@qq.com）既能自证身份，又不把完整地址交给任何访客。
	email := adminEmailMasked()
	if s.auth == nil {
		return map[string]any{"ok": true, "loggedIn": false, "email": email}, nil
	}
	sess := s.auth.Verify(tokenFrom(r))
	if sess == nil {
		return map[string]any{"ok": true, "loggedIn": false, "email": email}, nil
	}
	return map[string]any{
		"ok":       true,
		"loggedIn": true,
		"email":    email,
		"madeTs":   sess.MadeAt.UnixMilli(),
		"expireTs": sess.Expire.UnixMilli(),
	}, nil
}

// handleAdminSessionsAll GET /api/admin/sessions —— 列出活跃会话（自查用）
func (s *Server) handleAdminSessionsAll(w http.ResponseWriter, r *http.Request) (any, error) {
	if s.auth == nil {
		return map[string]any{"ok": true, "list": []any{}}, nil
	}
	list := s.auth.Sessions()
	out := make([]map[string]any, 0, len(list))
	for _, x := range list {
		out = append(out, map[string]any{
			"madeTs":   x.MadeAt.UnixMilli(),
			"expireTs": x.Expire.UnixMilli(),
			"ip":       x.IP,
			"ua":       x.UA,
			"current":  x.Token == tokenFrom(r),
		})
	}
	return map[string]any{"ok": true, "count": len(out), "list": out}, nil
}

// ---------------------------------------------------------------------------
// ③ 读可编辑配置（管理台表单回填用）
// ---------------------------------------------------------------------------

// handleAdminGetConfig GET /api/admin/config
//
// 返回「表单需要的全部字段 + 每个条件的开关状态」。
// 与 /api/state 的区别：这里是**可编辑视图**，字段名与保存接口一一对应。
func (s *Server) handleAdminGetConfig(w http.ResponseWriter, r *http.Request) (any, error) {
	cfg := s.cfg()
	if cfg == nil {
		return nil, fmt.Errorf("策略配置不可用")
	}

	return map[string]any{
		"ok": true,
		// —— 买入条件 ——
		"buy": map[string]any{
			"enabled":          cfg.Enabled,
			"score_threshold":  cfg.ScoreThreshold,
			"min_bar_rise_pct": cfg.MinBarRisePct(),
			"margin_usdt":      cfg.Entry.MarginUSDT,
			"max_margin_usdt":  cfg.Entry.MaxMarginUSDT,
			"leverage":         cfg.Entry.Leverage,
			"margin_policy":    cfg.Entry.MarginPolicy,
		},
		// —— 加仓条件 ——
		"addon": map[string]any{
			"enabled":          cfg.Addon.Enabled,
			"mode":             cfg.Addon.Mode,
			"score_threshold":  cfg.Addon.ScoreThres,
			"bar_rise_pct":     cfg.Addon.BarRisePct,
			"drop_pct":         cfg.Addon.DropPct,
			"price_rise_pct":   cfg.Addon.PriceRise,
			"margin_usdt":      cfg.Addon.MarginUSDT,
			"ratio":            cfg.Addon.Ratio,
			"max_times":        cfg.Addon.MaxTimes,
		},
		// —— 出场（只读展示，八期不改；改了要同步的地方太多）——
		"exit": map[string]any{
			"take_profit_pct":  cfg.Exit.TakeProfitPct,
			"stop_loss_pct":    cfg.Exit.StopLossPct,
			"max_hold_minutes": cfg.Exit.MaxHoldMinutes,
			"boll_upper_exit":  cfg.Exit.BollUpperExit,
		},
		// —— 其他只读信息 ——
		"bar":         cfg.Bar,
		"dry_run":     cfg.DryRun,
		"path":        s.strategy.Path(),
		"serverNow":   time.Now().UnixMilli(),
		"startTs":     s.startAt.UnixMilli(),
	}, nil
}

// ---------------------------------------------------------------------------
// ④ 保存配置
// ---------------------------------------------------------------------------

// handleAdminSaveOrGetConfig 按方法分流同一个路径：
// GET /api/admin/config → 读；POST → 保存。
//
// 合成一个入口是为了让「读」和「写」共用同一份鉴权与同一份字段口径 ——
// 两处各写一遍字段名，迟早会出现「表单填的是 A、保存写的是 B」。
func (s *Server) handleAdminSaveOrGetConfig(w http.ResponseWriter, r *http.Request) (any, error) {
	if r.Method == http.MethodPost {
		return s.handleAdminSaveConfig(w, r)
	}
	return s.handleAdminGetConfig(w, r)
}

// handleAdminSaveConfig POST /api/admin/config
//
// 入参：service.Patch 的字段子集（只传要改的）。
// 流程：鉴权（wrap 里做）→ 解析校验 → 原子写回 JSON → 立刻 Force 热重载 → 回显新值。
//
// ★ 为什么写完立刻 Force ★
//   strategyStore 的 Watch 是 2 秒轮询，这里主动调一次 Force 把它提前到「立刻」，
//   并且**当场返回写入后的真实值** —— 用户点保存后看到的就是生效值，
//   不用自己去刷新对比（那个「改了到底有没有用」的问题就不再需要问了）。
func (s *Server) handleAdminSaveConfig(w http.ResponseWriter, r *http.Request) (any, error) {
	if r.Method != http.MethodPost {
		return nil, fmt.Errorf("只接受 POST")
	}
	if s.writer == nil {
		return nil, fmt.Errorf("配置写入组件未初始化")
	}
	body, err := readJSONBody(r)
	if err != nil {
		return nil, err
	}
	patch, err := service.ParsePatch(body)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}, nil
	}

	before := s.snapshotEditable()
	changed, err := s.writer.Apply(patch)
	if err != nil {
		s.logf("✗ 配置保存失败：%v", err)
		return map[string]any{"ok": false, "error": err.Error()}, nil
	}

	// 立刻热重载（不等 Watch 的 2 秒轮询）
	if _, _, ferr := s.strategy.Force(); ferr != nil {
		// 文件已经写进去了，但重读失败 —— 这种情况要如实说，
		// 因为用户看到「保存成功」会以为已经生效。
		s.logf("⚠ 配置已写盘但热重载失败：%v", ferr)
		return map[string]any{
			"ok":      false,
			"error":   "配置已写入文件，但热重载失败：" + ferr.Error(),
			"changed": changed,
		}, nil
	}

	after := s.snapshotEditable()
	s.logf("★ 管理员保存配置成功（%s），立刻热重载生效", strings.Join(changed, "；"))
	// 让它也立刻跟一遍准入过滤（改金额 / 上限会影响哪些合约买得起）。
	// ★ 为什么用「通过 Server 上的钩子回调」而不是直接调 applyUniverseFilter：
	//   那个函数在 cmd 包里（依赖 DB + BackfillManager），handler 不该反向依赖它。
	//   cmd 启动时把回调塞进来，没塞（比如测试）就跳过 —— 准入会在下次周期任务里补上。
	if s.onConfigSaved != nil {
		s.onConfigSaved()
	}

	return map[string]any{
		"ok":        true,
		"changed":   changed,
		"before":    before,
		"after":     after,
		"serverNow": time.Now().UnixMilli(),
		"applied":   true, // 已热重载，无需重启
	}, nil
}

// snapshotEditable 取一份「管理台可编辑字段」的快照，用于保存前后对比回显。
func (s *Server) snapshotEditable() map[string]any {
	cfg := s.cfg()
	if cfg == nil {
		return map[string]any{}
	}
	return map[string]any{
		"buy_enabled":       cfg.Enabled,
		"score_threshold":   cfg.ScoreThreshold,
		"min_bar_rise_pct":  cfg.MinBarRisePct(),
		"buy_margin_usdt":   cfg.Entry.MarginUSDT,
		"max_margin_usdt":   cfg.Entry.MaxMarginUSDT,
		"addon_enabled":     cfg.Addon.Enabled,
		"addon_mode":        cfg.Addon.Mode,
		"addon_score":       cfg.Addon.ScoreThres,
		"addon_bar_rise":    cfg.Addon.BarRisePct,
		"addon_drop_pct":    cfg.Addon.DropPct,
		"addon_price_rise":  cfg.Addon.PriceRise,
		"addon_margin_usdt": cfg.Addon.MarginUSDT,
		"addon_ratio":       cfg.Addon.Ratio,
		"addon_max_times":   cfg.Addon.MaxTimes,
	}
}

