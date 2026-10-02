/* ==========================================================================
   admin.js —— 管理台前端（★ 2026-10-02 八期）
   --------------------------------------------------------------------------
   职责：
     1. 把手势（K 线图上连点 6 次左键 + 1 次右键）变成「弹出管理对话框」；
     2. 对话框里：登录（邮箱验证码）→ 条件勾选 + 金额输入 → 保存 / 取消 / 关闭；
     3. 保存走 POST /api/admin/config，服务端原子写回 JSON 并立刻热重载。

   ★ 几个刻意的设计决定 ★

   a. 表单是**动态构造**的，不写在 index.html 里。
      写死在 HTML 里的话，任何人打开首页 F12 就能看到"有个管理表单"，
      隐藏入口就白做了。现在只有手势触发过之后，DOM 里才会出现这些元素。

   b. 「条件勾选」的真实语义 = **开关**，不是「勾了才生效」的直觉理解。
      项目的 JSON 里没有 `use_score` 这类字段，为避免再引入一套需要同步维护的
      开关状态（且改了要同步后端判定），采用**取反表达**：
        · 勾选「分数门槛」= 启用该条件 → 保存时写真实阈值
        · 取消勾选        = 关闭该条件 → 保存时写「关闭值」
          - 买入分数门槛：关闭值 0（SignalQualified 里 threshold<=0 直接拒绝 → 等价停买）
          - 买入涨跌幅门槛：关闭值为空指针 → 用 0（= 显式关闭，只按分数判）
          - 加仓跌幅门槛：0（该条件不参与判定）
      这是本项目**真实存在**的语义，不是前端自造 —— 页面上的文案也照实写明，
      避免用户以为"取消勾选 = 这条不看了"却实际上把策略关停了。

   c. 所有 admin 接口都带 credentials: 'same-origin'，让 HttpOnly cookie 自动带上。
      不用 localStorage 存 token —— 那样 XSS 一偷就走。

   d. 保存成功后**回显后端返回的真实生效值**，而不是"已保存"三个字。
      用户真正想知道的是"现在跑的是不是我要的"，那就直接把生效值摊给他看。
   ========================================================================== */

(function () {
  'use strict';

  // —— 手势参数 ——
  var GESTURE_LEFT = 6;    // 需要连点的左键次数
  var GESTURE_WINDOW = 2600; // 整段手势的时间窗（毫秒），超时重算
  var GESTURE_GAP = 900;   // 两次点击之间超过这么久也算重来

  var state = {
    leftCount: 0,
    lastAt: 0,
    dialog: null,     // 当前打开的浮层
    loggedIn: false,
    cfg: null,        // 最近一次读到的配置
    saving: false,
  };

  // ------------------------------------------------------------------
  // 小工具
  // ------------------------------------------------------------------
  function el(tag, attrs, kids) {
    var n = document.createElement(tag);
    if (attrs) for (var k in attrs) {
      if (k === 'class') n.className = attrs[k];
      else if (k === 'html') n.innerHTML = attrs[k];
      else if (k === 'text') n.textContent = attrs[k];
      else if (k.indexOf('on') === 0) n.addEventListener(k.slice(2), attrs[k]);
      else if (attrs[k] != null) n.setAttribute(k, attrs[k]);
    }
    (kids || []).forEach(function (c) { if (c) n.appendChild(c); });
    return n;
  }

  function api(path, opt) {
    opt = opt || {};
    opt.credentials = 'same-origin';           // ★ 带上 HttpOnly 会话 cookie
    opt.headers = Object.assign(
      { 'Content-Type': 'application/json' }, opt.headers || {});
    if (opt.body && typeof opt.body !== 'string') opt.body = JSON.stringify(opt.body);
    return fetch(path, opt).then(function (r) {
      return r.json().catch(function () { return { ok: false, error: 'HTTP ' + r.status }; })
        .then(function (j) { j.__status = r.status; return j; });
    });
  }

  function num(v) {
    var n = parseFloat(v);
    return isFinite(n) ? n : NaN;
  }

  // 授权邮箱的**打码形式**由服务端下发（GET /api/admin/session 与 send_code 回显）。
  // 这里只是打开对话框、还没拿到服务端应答之前的占位文案；
  // 真实完整地址**任何请求都不下发**（见后端 adminEmailMasked 注释）。
  var MAIL_PLACEHOLDER = '（授权邮箱）';

  // ------------------------------------------------------------------
  // 浮层：统一的关闭 / 销毁
  // ------------------------------------------------------------------
  function closeDialog() {
    if (state.dialog) {
      state.dialog.remove();
      state.dialog = null;
    }
    document.removeEventListener('keydown', onEsc, true);
  }
  function onEsc(e) {
    if (e.key === 'Escape') { e.stopPropagation(); closeDialog(); }
  }
  function mount(mask) {
    closeDialog();
    state.dialog = mask;
    document.body.appendChild(mask);
    document.addEventListener('keydown', onEsc, true);
  }
  function mkMask() {
    // 点遮罩**不**关闭：这个框里可能在填金额，误点一下就白填了。
    // 关闭只能走「取消 / 关闭 / Esc」三个明确入口。
    return el('div', { class: 'adm-mask' });
  }

  // ==================================================================
  // ① 登录对话框
  // ==================================================================
  function openLogin(onOk) {
    var codeInput, emailText, stageTag, msgBox, sendBtn, loginBtn;
    var sent = false;
    var mail = '';   // 服务端下发的**打码**授权邮箱，拿到后回填到界面上

    // 授权邮箱一律以「未获取到邮箱」开头，避免被人从源码里读到任何地址线索
    function showMail() { emailText.textContent = mail || MAIL_PLACEHOLDER; }

    function setMsg(txt, bad) {
      msgBox.textContent = txt || '';
      msgBox.className = 'adm-result' + (txt ? (bad ? ' bad' : ' ok') : '');
      msgBox.style.display = txt ? '' : 'none';
    }

    function sendCode() {
      sendBtn.disabled = true;
      setMsg('');
      // ★★ 不要传 email！（2026-10-02 实翻车修复）★★
      //
      //   这里原来传的是 `email: emailText.textContent` —— 也就是**界面上显示的文字**。
      //   但界面上那个 span 在拿到服务端应答之前显示的是占位文案「（授权邮箱）」，
      //   所以真实发出去的是 email="（授权邮箱）"。
      //   服务端一看不在白名单，按「不回显白名单」的安全设计**静默忽略** ——
      //   于是用户点了发送、界面提示"已发送"，而邮件系统里一封信都没有。
      //   这个 bug 极其隐蔽：前后端各自的行为都是"正确"的，
      //   错在**前端把展示文案当成了数据**。
      //
      //   正确做法：前端**根本不需要知道邮箱**。
      //   服务端 send_code 在 email 为空时就用硬编码的授权邮箱（见
      //   internal/handler/api_admin_config.go：「email == "" → AllowedEmail()」）。
      //   前端越不知道那个地址，越不可能泄漏它 —— 这与安全设计的方向一致。
      api('/api/admin/send_code', { method: 'POST', body: {} })
        .then(function (j) {
          if (!j.ok) { setMsg(j.error || '发送失败', true); return; }
          sent = true;
          if (j.to) { mail = j.to; showMail(); }
          stageTag.textContent = '已发送';
          stageTag.className = 'adm-stage done';
          codeInput.focus();
          setMsg('验证码已发至 ' + (j.to || '授权邮箱') + '，' +
            Math.round((j.ttlSec || 300) / 60) + ' 分钟内有效。', false);
          // 60 秒后允许重发
          var left = 60;
          var t = setInterval(function () {
            left--;
            if (left <= 0) { clearInterval(t); sendBtn.disabled = false; sendBtn.textContent = '重新发送'; return; }
            sendBtn.textContent = '重发(' + left + 's)';
          }, 1000);
        })
        .catch(function (e) { setMsg('请求失败：' + e.message, true); sendBtn.disabled = false; });
    }

    function doLogin() {
      var code = (codeInput.value || '').trim();
      if (!code) { setMsg('请输入验证码', true); codeInput.focus(); return; }
      loginBtn.disabled = true;
      setMsg('验证中…');
      api('/api/admin/login', {
        method: 'POST',
        body: { email: emailText.textContent, code: code },
      }).then(function (j) {
        loginBtn.disabled = false;
        if (!j.ok) { setMsg(j.error || '登录失败', true); codeInput.select(); return; }
        state.loggedIn = true;
        closeDialog();
        onOk();
      }).catch(function (e) {
        loginBtn.disabled = false;
        setMsg('请求失败：' + e.message, true);
      });
    }

    emailText = el('span', { text: '未获取到邮箱' });
    stageTag = el('span', { class: 'adm-stage pending', text: '未发送' });
    codeInput = el('input', {
      class: 'code', type: 'text', inputmode: 'numeric', maxlength: '6',
      autocomplete: 'one-time-code', placeholder: '______',
      onkeydown: function (e) { if (e.key === 'Enter') doLogin(); },
    });
    msgBox = el('div', { class: 'adm-result' });
    msgBox.style.display = 'none';
    sendBtn = el('button', { class: 'btn', text: '发送验证码', onclick: sendCode });
    loginBtn = el('button', { class: 'btn', text: '验证并登录', onclick: doLogin });

    var mask = mkMask();
    var dlg = el('div', { class: 'adm-dlg' }, [
      el('div', { class: 'adm-hd' }, [
        el('h3', { text: '管理员验证' }),
        el('span', { class: 'adm-badge', text: '仅限本人' }),
        el('button', { class: 'adm-x', text: '✕', title: '关闭', onclick: closeDialog }),
      ]),
      el('div', { class: 'adm-body' }, [
        el('div', { class: 'adm-login-note' }, [
          el('div', { html: '验证码将发送到授权邮箱：<code>' + MAIL_PLACEHOLDER + '</code>' }),
          el('div', { html: '只有该邮箱能收到 —— 换任何其它邮箱都不会发出邮件。' }),
          el('div', { html: '验证码 5 分钟内有效，用过即作废；会话保持 24 小时。' }),
        ]),
        el('div', { class: 'adm-field' }, [
          el('span', { text: '邮箱' }), emailText, stageTag,
        ]),
        el('div', { class: 'adm-field' }, [
          el('span', { text: '验证码' }), codeInput,
        ]),
        msgBox,
      ]),
      el('div', { class: 'adm-ft' }, [
        el('span', { class: 'adm-hint', text: '登录后才能修改交易条件' }),
        el('button', { class: 'btn ghost', text: '取消', onclick: closeDialog }),
        sendBtn, loginBtn,
      ]),
    ]);
    mask.appendChild(dlg);
    mount(mask);

    // 打开即问一次服务端「授权邮箱是什么」（打码形式），并探会话是否还在
    api('/api/admin/session').then(function (j) {
      if (j && j.loggedIn) { state.loggedIn = true; closeDialog(); onOk(); return; }
      // 打码邮箱：4****373@qq.com —— 只用于自证「是我的那个邮箱」，不是完整地址
      if (j && j.email) { mail = j.email; showMail(); }
    });
    codeInput.focus();
  }

  // ==================================================================
  // ② 条件配置对话框
  // ==================================================================

  // 一条「可勾选条件」行。setVal/getVal 用于读回输入框数字。
  function condRow(opts) {
    // opts: {id, title, desc, unit, type:'number'|'select', options, step}
    var input;
    if (opts.type === 'select') {
      input = el('select', {}, (opts.options || []).map(function (o) {
        return el('option', { value: o.v, text: o.t });
      }));
    } else {
      input = el('input', {
        type: 'number', step: opts.step || 'any',
        placeholder: opts.placeholder || '',
      });
    }
    var cb = el('input', { type: 'checkbox' });
    var row = el('div', { class: 'adm-cond', id: opts.id }, [
      el('label', {}, [
        cb,
        el('span', {}, [
          el('span', { text: opts.title }),
          opts.desc ? el('span', { class: 'adm-desc', text: opts.desc }) : null,
        ]),
      ]),
      input,
      opts.unit ? el('span', { class: 'adm-unit', text: opts.unit }) : null,
    ]);
    function sync() {
      input.disabled = !cb.checked;
      row.classList.toggle('on', cb.checked);
    }
    cb.addEventListener('change', sync);
    row.__cb = cb;
    row.__input = input;
    row.__sync = sync;
    return row;
  }

  function openConfig() {
    var cfg = state.cfg || {};
    var b = cfg.buy || {};
    var a = cfg.addon || {};

    // ---- 买入条件行 ----
    var rowScore = condRow({
      id: 'buyScore', title: '分数门槛',
      desc: '8 个因子里命中多少项才买（判定：score ≥ 门槛）。取消勾选将把门槛写为 0，等于停买。',
      type: 'number', step: '1',
    });
    rowScore.__input.value = b.score_threshold != null ? b.score_threshold : 3;
    rowScore.__cb.checked = (b.score_threshold || 0) > 0;
    rowScore.__sync();

    var rowRise = condRow({
      id: 'buyRise', title: 'K 线涨跌幅门槛',
      desc: '触发那根 K 线自身的涨跌幅。负数 = 必须真跌（如 -0.7 = 跌超 0.7%），正数 = 必须真涨。',
      type: 'number', step: '0.1', unit: '%',
    });
    var bRise = (b.min_bar_rise_pct == null) ? 0 : b.min_bar_rise_pct;
    rowRise.__input.value = bRise;
    rowRise.__cb.checked = bRise !== 0;
    rowRise.__sync();

    var rowBuyAmt = condRow({
      id: 'buyAmt', title: '每笔买入金额',
      desc: '单笔投入的保证金（USDT）。',
      type: 'number', step: '0.01', unit: 'U',
    });
    rowBuyAmt.__input.value = b.margin_usdt != null ? b.margin_usdt : 0.1;
    rowBuyAmt.__cb.checked = true;
    rowBuyAmt.__sync();

    var rowBuyCap = condRow({
      id: 'buyCap', title: '单笔金额硬上限',
      desc: 'min_one 放大时的封顶。取消勾选将写为 0（不封顶，谨慎）。',
      type: 'number', step: '0.01', unit: 'U',
    });
    rowBuyCap.__input.value = b.max_margin_usdt != null ? b.max_margin_usdt : 1;
    rowBuyCap.__cb.checked = (b.max_margin_usdt || 0) > 0;
    rowBuyCap.__sync();

    var rowBuyOn = condRow({
      id: 'buyOn', title: '买入总开关',
      desc: '关掉后整个策略停止开新仓（不影响已有持仓的出场）。',
      type: 'select',
      options: [{ v: '1', t: '开启' }, { v: '0', t: '关闭' }],
    });
    rowBuyOn.__input.value = (b.enabled === false) ? '0' : '1';
    rowBuyOn.__cb.checked = true;
    rowBuyOn.__sync();

    // ---- 加仓模式 ----
    var mode = a.mode === 'price' ? 'price' : 'resonance';
    var modeBox = el('div', { class: 'adm-modes' });
    var modes = [
      { v: 'resonance', t: '共振模式', d: '分数 + 该根涨幅' },
      { v: 'price', t: '价格模式', d: '跌破买价 + 该根涨幅' },
    ];
    modes.forEach(function (m) {
      var card = el('div', {
        class: 'adm-mode' + (mode === m.v ? ' on' : ''),
        onclick: function () {
          mode = m.v;
          Array.prototype.forEach.call(modeBox.children, function (c) {
            c.classList.toggle('on', c.__v === mode);
          });
          syncAddonRows();
        },
      });
      card.__v = m.v;
      card.appendChild(el('b', { text: m.t }));
      card.appendChild(el('span', { text: m.d }));
      modeBox.appendChild(card);
    });

    // ---- 加仓条件行（两套，按模式显隐）----
    var rowAddonScore = condRow({
      id: 'addonScore', title: '加仓分数门槛',
      desc: '判定为「score > 门槛」（严格大于）。写 0 = 跟随买入的分数门槛。',
      type: 'number', step: '1',
    });
    rowAddonScore.__input.value = a.score_threshold != null ? a.score_threshold : 2;
    rowAddonScore.__cb.checked = (a.score_threshold || 0) > 0;
    rowAddonScore.__sync();

    var rowAddonRise = condRow({
      id: 'addonRise', title: '加仓 K 线涨幅门槛',
      desc: '触发那根自身涨幅须严格大于该值。',
      type: 'number', step: '0.1', unit: '%',
    });
    rowAddonRise.__input.value = a.bar_rise_pct != null ? a.bar_rise_pct : 0.7;
    rowAddonRise.__cb.checked = (a.bar_rise_pct || 0) > 0;
    rowAddonRise.__sync();

    var rowDrop = condRow({
      id: 'addonDrop', title: '跌破买价幅度',
      desc: '收盘价比买入价低超过该幅度才加仓（价格模式）。',
      type: 'number', step: '0.1', unit: '%',
    });
    rowDrop.__input.value = a.drop_pct != null ? a.drop_pct : 1;
    rowDrop.__cb.checked = (a.drop_pct || 0) > 0;
    rowDrop.__sync();

    var rowPriceRise = condRow({
      id: 'addonPriceRise', title: '该根涨幅门槛',
      desc: '价格模式下触发那根的涨幅须严格大于该值。',
      type: 'number', step: '0.1', unit: '%',
    });
    rowPriceRise.__input.value = a.price_rise_pct != null ? a.price_rise_pct : 1;
    rowPriceRise.__cb.checked = (a.price_rise_pct || 0) > 0;
    rowPriceRise.__sync();

    var rowAddonAmt = condRow({
      id: 'addonAmt', title: '每次加仓金额',
      desc: '直接指定加仓金额（U）。写 0 = 改用「按比例」的加仓比例。',
      type: 'number', step: '0.01', unit: 'U',
    });
    rowAddonAmt.__input.value = a.margin_usdt != null ? a.margin_usdt : 0;
    rowAddonAmt.__cb.checked = true;
    rowAddonAmt.__sync();

    var rowAddonRatio = condRow({
      id: 'addonRatio', title: '加仓比例',
      desc: '加仓额 = 原持仓保证金 × 该比例。仅当「每次加仓金额」为 0 时生效。',
      type: 'number', step: '0.01', unit: '倍',
    });
    rowAddonRatio.__input.value = a.ratio != null ? a.ratio : 0.3333333333;
    rowAddonRatio.__cb.checked = true;
    rowAddonRatio.__sync();

    var rowAddonOn = condRow({
      id: 'addonOn', title: '加仓总开关',
      type: 'select',
      options: [{ v: '1', t: '开启' }, { v: '0', t: '关闭' }],
    });
    rowAddonOn.__input.value = (a.enabled === false) ? '0' : '1';
    rowAddonOn.__cb.checked = true;
    rowAddonOn.__sync();

    var riseBox = el('div', {}, [rowAddonScore, rowAddonRise]);
    var priceBox = el('div', {}, [rowDrop, rowPriceRise]);
    function syncAddonRows() {
      riseBox.style.display = mode === 'resonance' ? '' : 'none';
      priceBox.style.display = mode === 'price' ? '' : 'none';
    }
    syncAddonRows();

    // ---- 结果条 ----
    var resultBox = el('div', { class: 'adm-result' });
    resultBox.style.display = 'none';
    function setResult(html, cls) {
      resultBox.innerHTML = html;
      resultBox.className = 'adm-result ' + (cls || '');
      resultBox.style.display = '';
    }

    // ---- 收集并保存 ----
    var saveBtn, cancelBtn;
    function collect() {
      var p = {};
      // 买入
      p.buy_enabled = rowBuyOn.__input.value === '1';
      p.buy_use_score = rowScore.__cb.checked;
      p.score_threshold = rowScore.__cb.checked ? Math.round(num(rowScore.__input.value)) : 0;
      if (isNaN(p.score_threshold)) return { error: '分数门槛不是数字' };
      var rise = rowRise.__cb.checked ? num(rowRise.__input.value) : 0;
      if (isNaN(rise)) return { error: '买入涨跌幅门槛不是数字' };
      p.min_bar_rise_pct = rise;
      var buyAmt = num(rowBuyAmt.__input.value);
      if (isNaN(buyAmt) || buyAmt < 0) return { error: '买入金额不是合法数字' };
      p.buy_margin_usdt = buyAmt;
      var cap = rowBuyCap.__cb.checked ? num(rowBuyCap.__input.value) : 0;
      if (isNaN(cap) || cap < 0) return { error: '单笔上限不是合法数字' };
      p.max_margin_usdt = cap;

      // 加仓
      p.addon_mode = mode;
      p.addon_enabled = rowAddonOn.__input.value === '1';
      if (mode === 'resonance') {
        var as = rowAddonScore.__cb.checked ? Math.round(num(rowAddonScore.__input.value)) : 0;
        if (isNaN(as)) return { error: '加仓分数门槛不是数字' };
        p.addon_score = as;
        var ar = rowAddonRise.__cb.checked ? num(rowAddonRise.__input.value) : 0;
        if (isNaN(ar)) return { error: '加仓涨幅门槛不是数字' };
        p.addon_bar_rise = ar;
      } else {
        var dp = rowDrop.__cb.checked ? num(rowDrop.__input.value) : 0;
        if (isNaN(dp)) return { error: '跌破幅度不是数字' };
        p.addon_drop_pct = dp;
        var pr = rowPriceRise.__cb.checked ? num(rowPriceRise.__input.value) : 0;
        if (isNaN(pr)) return { error: '价格模式涨幅门槛不是数字' };
        p.addon_rise_pct = pr;
      }
      var aAmt = num(rowAddonAmt.__input.value);
      if (isNaN(aAmt) || aAmt < 0) return { error: '加仓金额不是合法数字' };
      p.addon_margin_usdt = aAmt;
      var ratio = num(rowAddonRatio.__input.value);
      if (isNaN(ratio) || ratio <= 0) return { error: '加仓比例必须大于 0' };
      p.addon_ratio = ratio;
      return { patch: p };
    }

    function doSave() {
      if (state.saving) return;
      var got = collect();
      if (got.error) { setResult('✗ ' + got.error, 'bad'); return; }
      state.saving = true;
      saveBtn.disabled = true;
      saveBtn.textContent = '保存中…';
      setResult('正在写入配置并热重载…');
      api('/api/admin/config', { method: 'POST', body: got.patch }).then(function (j) {
        state.saving = false;
        saveBtn.disabled = false;
        saveBtn.textContent = '保存并生效';
        if (j.__status === 401 || j.needLogin) {
          setResult('✗ 会话已过期，请关闭后重新通过手势进入并验证邮箱。', 'bad');
          state.loggedIn = false;
          return;
        }
        if (!j.ok) { setResult('✗ ' + (j.error || '保存失败'), 'bad'); return; }
        state.cfg = Object.assign({}, state.cfg, j.after ? { buy: j.after } : {});
        var lines = (j.changed || []).map(function (s) { return '· ' + s; }).join('<br>');
        setResult('<b>✓ 已保存并立刻热重载生效</b>（无需重启）<br>' + lines +
          '<br><span style="color:var(--muted)">配置路径：' + (cfg.path || '') + '</span>', 'ok');
        // 顶栏的策略文案也跟着刷新一次（不用等下一轮轮询）
        if (typeof window.__refreshAdminState === 'function') window.__refreshAdminState();
      }).catch(function (e) {
        state.saving = false;
        saveBtn.disabled = false;
        saveBtn.textContent = '保存并生效';
        setResult('✗ 请求失败：' + e.message, 'bad');
      });
    }

    saveBtn = el('button', { class: 'btn', text: '保存并生效', onclick: doSave });
    cancelBtn = el('button', {
      class: 'btn ghost', text: '取消',
      onclick: function () { closeDialog(); },   // ★ 取消 = 不保存，直接关
    });

    var mask = mkMask();
    var dlg = el('div', { class: 'adm-dlg wide' }, [
      el('div', { class: 'adm-hd' }, [
        el('h3', { text: '交易条件配置' }),
        el('span', { class: 'adm-badge', text: '管理员' }),
        el('button', { class: 'adm-x', text: '✕', title: '关闭（不保存）', onclick: closeDialog }),
      ]),
      el('div', { class: 'adm-body' }, [
        el('div', { class: 'adm-sec' }, [
          el('h4', { text: '买入条件' }),
          rowBuyOn, rowScore, rowRise, rowBuyAmt, rowBuyCap,
        ]),
        el('div', { class: 'adm-sec' }, [
          el('h4', { text: '加仓条件' }),
          modeBox, rowAddonOn,
          riseBox, priceBox,
          rowAddonAmt, rowAddonRatio,
        ]),
        el('div', { class: 'adm-sec' }, [
          el('h4', { text: '当前出场（只读）' }),
          el('div', { class: 'adm-login-note', html:
            '止盈 <code>' + (cfg.exit && cfg.exit.take_profit_pct) + '%</code> · ' +
            '止损 <code>' + (cfg.exit && cfg.exit.stop_loss_pct) + '%</code> · ' +
            '超时 <code>' + (cfg.exit && Math.round((cfg.exit.max_hold_minutes || 0) / 60)) + ' 小时</code>' +
            '<br>出场参数本页不改（改动点太多，容易漏同步）。需要调整请直接改配置文件。' }),
        ]),
        resultBox,
      ]),
      el('div', { class: 'adm-ft' }, [
        el('span', { class: 'adm-hint', text: '保存 → 写入配置文件 → 立刻热重载，不重启进程' }),
        cancelBtn, saveBtn,
      ]),
    ]);
    mask.appendChild(dlg);
    mount(mask);
  }

  // ==================================================================
  // ③ 入口：手势 → 拉配置 → 弹框
  // ==================================================================
  function enter() {
    api('/api/admin/config').then(function (j) {
      if (j.__status === 401 || j.needLogin) {
        openLogin(function () {
          api('/api/admin/config').then(function (k) {
            state.cfg = k;
            openConfig();
          });
        });
        return;
      }
      if (!j.ok) { console.warn('[admin] 读取配置失败：', j.error); return; }
      state.cfg = j;
      openConfig();
    }).catch(function (e) { console.warn('[admin] 请求失败：', e); });
  }

  // ------------------------------------------------------------------
  // 手势识别：连点 N 次左键 + 1 次右键
  //
  // ★ 为什么用 contextmenu 事件而不是 mousedown(button=2)：
  //   右键的 mousedown 在很多浏览器/宿主里会被拦或走别的路径，
  //   而 contextmenu 是每个浏览器都一定会发的 —— 只要 preventDefault 掉
  //   默认菜单，就能稳定拿到这一次"右键"。
  //   preventDefault **只在手势流程中**做（已经点了若干次左键），
  //   平时右键照常弹浏览器菜单，不影响用户。
  // ------------------------------------------------------------------
  function tick(which) {
    var now = Date.now();
    if (now - state.lastAt > GESTURE_GAP) state.leftCount = 0;
    state.lastAt = now;

    if (which === 'left') {
      state.leftCount++;
      if (state.leftCount >= GESTURE_LEFT) {
        state.leftCount = 0;
        state.awaitRight = true;
        state.awaitUntil = now + GESTURE_WINDOW;
      }
      return false;
    }

    // 右键
    if (state.awaitRight && now <= state.awaitUntil) {
      state.awaitRight = false;
      state.leftCount = 0;
      enter();
      return true;
    }
    state.leftCount = 0;
    state.awaitRight = false;
    return false;
  }

  // ------------------------------------------------------------------
  // 绑定到 K 线图（轮询等它出现 —— 首页初始化是异步的）
  // ------------------------------------------------------------------
  function bindChart() {
    var tries = 0;
    var t = setInterval(function () {
      var box = document.querySelector('.chart-box') || document.getElementById('chart');
      if (!box) {
        if (++tries > 60) clearInterval(t);  // 约 30 秒还找不到就不找了
        return;
      }
      clearInterval(t);
      box.addEventListener('mouseup', function (e) {
        // 只认左键（0）。中键/右键不走这里。
        if (e.button !== 0) return;
        tick('left');
      }, true);
      box.addEventListener('contextmenu', function (e) {
        if (tick('right')) {
          e.preventDefault();
          e.stopPropagation();
        }
      }, true);
      // 数字标记：不给任何视觉提示（隐藏入口的题中之义）
    }, 500);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', bindChart);
  } else {
    bindChart();
  }

  // 暴露给控制台排错用（不含任何密钥，只是让本人能手动检查）
  window.__okxAdmin = {
    enter: enter,
    state: state,
    _gesture: tick,
  };
})();
