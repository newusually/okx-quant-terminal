/* ==========================================================================
   admin.js —— 管理台前端（★ 2026-10-02 八期；九期扩成全量热插拔配置台；
                                        十期：冷却下线 + 数量限制）
   --------------------------------------------------------------------------
   职责：
     1. 把手势（K 线图上连点 6 次左键 + 1 次右键）变成「弹出管理对话框」；
     2. 对话框里：登录（邮箱验证码）→ 分区填条件 → 保存；
     3. 保存走 POST /api/admin/config，服务端原子写回 JSON 并立刻热重载。

   ★ 几个刻意的设计决定 ★

   a. 表单是**动态构造**的，不写在 index.html 里。
      写死在 HTML 里的话，任何人打开首页 F12 就能看到"有个管理表单"，
      隐藏入口就白做了。现在只有手势触发过之后，DOM 里才会出现这些元素。

   b. ★ 九期核心原则：不造「假的开关」★

      八期的 condRow 是「勾选框 + 输入框」：勾上=启用该条件，
      取消=写一个"关闭值"。这对**真正有开关语义**的字段（分数门槛、
      K 线涨跌幅）是对的 —— 它们的 0 确实表示"关闭"。

      但九期新增的字段里有一批**根本没有"关闭值"**：

        · leverage / addon_ratio   写 0 是非法值（归一化会反压回默认）
        · buy_margin_usdt          写 0 会被归一化反压回 0.1
        · max_margin_usdt          写 0 会被归一化反压回 1.0
        · margin_policy            是枚举，不是开关

      对这些字段套 condRow，就会造出一个**假的开关**：
      用户取消勾选 → 保存 → 页面提示成功 → 回来一看还是原值。
      这正是本项目反复踩的「改了没用」，而且这次是**前端主动制造**的。

      所以：有开关语义 → condRow；没开关语义 → numRow / selRow / comboRow。
      判断标准只有一条：**这个字段写 0，引擎会把它当"关"还是当"错"？**

   c. 四种行类型各管一件事，别混用：
        condRow  勾选框 + 输入   —— 有"关闭值"的条件（写 0 真的等于关）
        numRow   纯输入          —— 没有关闭值的量（写 0 是非法/会被反压）
        selRow   下拉            —— 枚举（模式、口径、开关式的 on/off）
        comboRow 输入 + 预设下拉 —— 想给参考值、但不想限制死（十期加）

      ★ comboRow 的预设下拉**不是字段**：选一下只把值填进输入框，
        保存时永远只读输入框。所以不会出现"下拉显示 30、实际存着 7"。

   d. 所有 admin 接口都带 credentials: 'same-origin'，让 HttpOnly cookie 自动带上。
      不用 localStorage 存 token —— 那样 XSS 一偷就走。

   e. 保存成功后**回显后端返回的真实生效值**（changed 列表 + before/after），
      而不是"已保存"三个字。用户真正想知道的是"现在跑的是不是我要的"。
   ========================================================================== */

(function () {
  'use strict';

  // —— 手势参数 ——
  var GESTURE_LEFT = 6;      // 需要连点的左键次数
  var GESTURE_WINDOW = 2600; // 整段手势的时间窗（毫秒），超时重算
  var GESTURE_GAP = 900;     // 两次点击之间超过这么久也算重来

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
  function intOf(input) { return Math.round(num(input.value)); }

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
      //
      //   正确做法：前端**根本不需要知道邮箱**。
      //   服务端 send_code 在 email 为空时就用硬编码的授权邮箱。
      //   前端越不知道那个地址，越不可能泄漏它 —— 与安全设计方向一致。
      api('/api/admin/send_code', { method: 'POST', body: {} })
        .then(function (j) {
          if (!j.ok) { setMsg(j.error || '发送失败', true); sendBtn.disabled = false; return; }
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
      // ★★ 同样不要传 email！（2026-10-02 实翻车修复，与 sendCode 是同一个坑）★★
      //
      //   原来传的是 `email: emailText.textContent`，而那个 span 在发码成功后
      //   会被回填成服务端下发的**打码**地址「493****373@qq.com」。
      //   于是服务端登录时算的是：
      //       hashCode("493****373@qq.com", 用户输入的码)
      //   而发码时存的是：
      //       hashCode("493076373@qq.com", 真码)
      //   **两者必然不等** → 用户输入正确的 6 位码，却永远得到
      //   「验证码错误或已失效」。更糟的是每次失败都会累加试错次数，
      //   连试 5 次就把真码也作废了。
      //
      //   一个必须记住的推论：**展示用的打码地址不能参与任何比对**。
      api('/api/admin/login', {
        method: 'POST',
        body: { code: code },
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
  // ② 配置对话框
  // ==================================================================

  // —— 三种行类型 ——

  // (1) condRow：有**开关语义**的条件行（勾选 = 启用，取消 = 写关闭值）
  //
  //     只给这样的字段用：它的 0（或空）在引擎里**真的表示"关闭"**。
  //     比如 score_threshold=0 → SignalQualified 直接拒绝 = 停买；
  //         min_bar_rise_pct=0   → 显式关闭"必须真跌"这个条件。
  //     判断标准：**取消勾选后写进去的那个值，引擎认得吗？**
  function condRow(opts) {
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

  // (2) numRow：**纯数字行，没有勾选框**
  //
  //     ★ 用它而不是 condRow 的字段，都是"没有关闭值"的 ★
  //       leverage / ratio 写 0 非法；buy_margin_usdt 写 0 会被归一化
  //       反压回 0.1；margin_policy 是枚举。
  //       给它们套勾选框 = 造一个假开关：取消了保存，回来看还是原值。
  function numRow(opts) {
    var input = el('input', {
      type: 'number', step: opts.step || 'any',
      placeholder: opts.placeholder || '',
    });
    var row = el('div', { class: 'adm-cond adm-plain', id: opts.id }, [
      el('label', {}, [
        el('span', {}, [
          el('span', { text: opts.title }),
          opts.desc ? el('span', { class: 'adm-desc', text: opts.desc }) : null,
        ]),
      ]),
      input,
      opts.unit ? el('span', { class: 'adm-unit', text: opts.unit }) : null,
    ]);
    row.__input = input;
    return row;
  }

  // (3) selRow：二选一/多选一的下拉行
  //
  //     用下拉而不是勾选框：勾选框的"不勾"究竟是"关闭"还是"没填"，
  //     在界面上看不出来；下拉的两个选项是**显式**的两句话。
  function selRow(opts) {
    var sel = el('select', {}, (opts.options || []).map(function (o) {
      return el('option', { value: o.v, text: o.t });
    }));
    var row = el('div', { class: 'adm-cond adm-plain', id: opts.id }, [
      el('label', {}, [
        el('span', {}, [
          el('span', { text: opts.title }),
          opts.desc ? el('span', { class: 'adm-desc', text: opts.desc }) : null,
        ]),
      ]),
      sel,
    ]);
    row.__input = sel;
    return row;
  }

  // (4) comboRow：**既可以选择、也可以直接输入**的数字行
  //
  //     ★ 十期新增。用户原话：「持仓数量 可以选择也可以输入数量」★
  //
  //     两种实现都能满足，选了后者：
  //       · <datalist>：原生"边输边给建议"，但下拉箭头只在聚焦时露出来，
  //         而且各浏览器对 number + datalist 的支持参差不齐（Safari 基本没有）。
  //       · 数字输入框 + 旁边一个窄的「预设」下拉 ← 选这个。
  //         老浏览器也稳，而且**两个入口都看得见** ——
  //         交易参数界面里，「用户没发现有得选」比「多一个控件」糟糕得多。
  //
  //     ★ 一个必须说清的约定：预设下拉**不是一个独立字段** ★
  //       选中一项 → 它把数字写进左边的输入框 → 自己弹回「预设…」占位。
  //       所以：
  //         · 保存时永远只读输入框（collect() 与 numRow 完全一致，不读下拉）；
  //         · 输入框里是自定义值时，下拉显示占位文案而不是假装"已选中"，
  //           不会出现"下拉显示 30、实际存着 7"这种自相矛盾的状态。
  function comboRow(opts) {
    var input = el('input', {
      type: 'number', step: opts.step || '1',
      placeholder: opts.placeholder || '',
    });
    var presetOpts = [{ v: '', t: '预设…' }].concat(opts.presets || []);
    var sel = el('select', { class: 'adm-preset', title: '也可以直接在上面输入' },
      presetOpts.map(function (o) {
        return el('option', { value: o.v, text: o.t });
      }));
    sel.addEventListener('change', function () {
      if (sel.value === '') return;
      input.value = sel.value;
      sel.value = '';                       // 立刻弹回占位项，别让人以为它记住了
      input.classList.remove('flash');       // 给"已经填进去了"一点视觉反馈
      void input.offsetWidth;                // 强制重排，否则重复选同一项不触发动画
      input.classList.add('flash');
    });
    var row = el('div', { class: 'adm-cond adm-plain', id: opts.id }, [
      el('label', {}, [
        el('span', {}, [
          el('span', { text: opts.title }),
          opts.desc ? el('span', { class: 'adm-desc', text: opts.desc }) : null,
        ]),
      ]),
      el('span', { class: 'adm-pair' }, [input, sel]),
      opts.unit ? el('span', { class: 'adm-unit', text: opts.unit }) : null,
    ]);
    row.__input = input;   // ★ 只暴露输入框：collect() / 校验都走它，跟 numRow 一样
    return row;
  }

  // 两列栅格容器（字段多，单列要滚很久）
  function grid(kids) { return el('div', { class: 'adm-grid' }, kids); }

  // 分区容器
  function sec(title, kids) {
    return el('div', { class: 'adm-sec' }, [el('h4', { text: title })].concat(kids));
  }

  function openConfig() {
    var cfg = state.cfg || {};
    var sw = cfg.switch || {};
    var b = cfg.buy || {};
    var g = cfg.gate || {};
    var u = cfg.universe || {};
    var a = cfg.addon || {};
    var ex = cfg.exit || {};
    var rk = cfg.risk || {};
    var lv = cfg.live || {};

    // ---------- ① 总开关 ----------
    var rowBuyOn = selRow({
      id: 'swBuyOn', title: '策略总开关',
      desc: '关掉后停止开新仓（不影响已有持仓的出场）。',
      options: [{ v: '1', t: '开启' }, { v: '0', t: '关闭' }],
    });
    rowBuyOn.__input.value = (sw.buy_enabled === false) ? '0' : '1';

    var rowDryRun = selRow({
      id: 'swDryRun', title: '试运行（只出信号不下单）',
      desc: '改 true 后引擎照常扫信号、写记录，但不发任何真实订单。调试用。',
      options: [{ v: '0', t: '实盘下单' }, { v: '1', t: '只出信号' }],
    });
    rowDryRun.__input.value = (sw.dry_run === true) ? '1' : '0';

    // ---------- ② 买入条件 ----------
    var rowScore = condRow({
      id: 'buyScore', title: '分数门槛',
      desc: '8 个因子里命中多少项才买（判定：score ≥ 门槛，0~8）。取消勾选写 0 = 停买。',
      type: 'number', step: '1',
    });
    rowScore.__input.value = b.score_threshold != null ? b.score_threshold : 4;
    rowScore.__cb.checked = (b.score_threshold || 0) > 0;
    rowScore.__sync();

    var rowRise = condRow({
      id: 'buyRise', title: 'K 线涨跌幅门槛',
      desc: '触发那根 K 线自身的涨跌幅。负数 = 必须真跌（-0.7 = 跌超 0.7%），正数 = 必须真涨。取消勾选 = 关闭该条件。',
      type: 'number', step: '0.1', unit: '%',
    });
    var bRise = (b.min_bar_rise_pct == null) ? 0 : b.min_bar_rise_pct;
    rowRise.__input.value = bRise;
    rowRise.__cb.checked = bRise !== 0;
    rowRise.__sync();

    var rowBuyAmt = numRow({
      id: 'buyAmt', title: '每笔买入保证金',
      desc: '单笔投入的保证金。必须 > 0 —— 写 0 会被服务端归一化反压回默认值。',
      type: 'number', step: '0.01', unit: 'U',
    });
    rowBuyAmt.__input.value = b.buy_margin_usdt != null ? b.buy_margin_usdt : 0.1;

    var rowBuyCap = numRow({
      id: 'buyCap', title: '单笔金额硬上限',
      desc: 'min_one 口径下放大时的封顶。必须 > 0（写 0 会被反压回默认）。',
      type: 'number', step: '0.01', unit: 'U',
    });
    rowBuyCap.__input.value = b.max_margin_usdt != null ? b.max_margin_usdt : 1;

    var rowLeverage = numRow({
      id: 'buyLev', title: '杠杆',
      desc: '合约杠杆倍数。改这个会影响爆仓价，谨慎。',
      type: 'number', step: '1', unit: 'x',
    });
    rowLeverage.__input.value = b.leverage != null ? b.leverage : 20;

    var rowPolicy = selRow({
      id: 'buyPolicy', title: '保证金口径',
      desc: 'min_one：买不起 1 张就放大到刚好 1 张（封顶用上面的上限）。fixed：严格按每笔金额，买不起就跳过。',
      options: [
        { v: 'min_one', t: 'min_one 放大到 1 张' },
        { v: 'fixed', t: 'fixed 买不起就跳过' },
      ],
    });
    rowPolicy.__input.value = b.margin_policy === 'fixed' ? 'fixed' : 'min_one';

    // ---------- ③ 开仓闸门 ----------
    //
    //   ★ 十期：「同合约开仓冷却」整行**删除** ★
    //
    //   用户的九期诉求是「距开仓不足 30 根要能改」，十期改口为
    //   「冷却条件全部删除 改成数量 就是持仓数量」。
    //
    //   所以这里不再有 rowCooldown 这个控件，字段本身也**从 patch 里去掉** ——
    //   前端不再发送 cooldown_bars，配置里的值原样保留（现在是 0）。
    //
    //   ★ 为什么不干脆把后端字段一起删掉？★
    //     留着它写 0 是零风险的：trader.go 的 cooldownBlocked 第一条件就是
    //     `cd <= 0 → 放行`，`0 = 真的完全不做冷却`，跟删掉的行为完全一样；
    //     而删字段要动 conf / service / 写回器三处，任何一处漏改都会让
    //     「配置文件里有个引擎不认的键」这种更难查的状态出现。
    //     注：十期顺手修了 `cd=0` 时的一句判定漏洞，见 cooldownBlocked 的注释。
    //
    //   ★ 数量把关为什么放在 max_concurrent_positions 而不是别的地方★
    //     trader.go 的判定是 `len(openPos)+opened >= N` 就 skip，
    //     也就是"达到 N 就不再开新仓"。填 30 = 最多同时持有 30 个。
    var rowMaxPos = comboRow({
      id: 'gateMaxPos', title: '最多同时持仓',
      desc: '同时持有的合约数上限，达到这个数就不再开新仓（填 30 = 最多同时持有 30 个）。' +
        '填 0 = 不限。可以在右边选预设，也可以直接输入数字。',
      step: '1', unit: '个',
      presets: [
        { v: '0', t: '不限' },
        { v: '5', t: '5' },
        { v: '10', t: '10' },
        { v: '20', t: '20' },
        { v: '30', t: '30' },
        { v: '50', t: '50' },
        { v: '100', t: '100' },
      ],
    });
    rowMaxPos.__input.value = g.max_concurrent_positions != null ? g.max_concurrent_positions : 30;

    var rowDailyMax = numRow({
      id: 'gateDaily', title: '当日开仓上限',
      desc: '每天最多开几笔。填 0 = 不限。',
      type: 'number', step: '1', unit: '笔',
    });
    rowDailyMax.__input.value = g.daily_max_entries != null ? g.daily_max_entries : 0;

    // ---------- ④ 合约准入 ----------
    var rowMaxOrder = numRow({
      id: 'uniMaxOrder', title: '准入单笔上限',
      desc: '「最小一手保证金 ≤ 它」才允许交易这个合约。必须 > 0。',
      type: 'number', step: '0.01', unit: 'U',
    });
    rowMaxOrder.__input.value = u.max_order_margin_usdt != null ? u.max_order_margin_usdt : 1;

    var rowNewDays = numRow({
      id: 'uniNewDays', title: '新上线排除天数',
      desc: '上市不足这么多天的不买（新币插针、没历史 K 线）。填 0 = 不排除。',
      type: 'number', step: '1', unit: '天',
    });
    rowNewDays.__input.value = u.exclude_new_listing_days != null ? u.exclude_new_listing_days : 30;

    var rowDelist = selRow({
      id: 'uniDelist', title: '排除待下线合约',
      desc: '按 OKX 公告中心解析出的下线名单排除。',
      options: [{ v: '1', t: '排除' }, { v: '0', t: '不排除' }],
    });
    rowDelist.__input.value = (u.exclude_delisting === false) ? '0' : '1';

    var rowStockEtf = selRow({
      id: 'uniStockEtf', title: '排除美股 / ETF / 商品',
      desc: '开启后只做加密合约。当前口径是关闭 —— 能不能做只看上面那条「准入单笔上限」。',
      options: [{ v: '1', t: '排除' }, { v: '0', t: '不排除' }],
    });
    rowStockEtf.__input.value = (u.exclude_stock_etf === true) ? '1' : '0';

    var rowMinVol = numRow({
      id: 'uniMinVol', title: '24h 成交额下限',
      desc: '成交额低于它的不做（深度差、滑点吃本金）。填 0 = 不限。',
      type: 'number', step: '10000', unit: 'U',
    });
    rowMinVol.__input.value = u.min_quote_volume_24h != null ? u.min_quote_volume_24h : 1000000;

    var rowTopN = numRow({
      id: 'uniTopN', title: '只算成交额前 N 名',
      desc: '扫描范围收窄到成交额前 N 个合约，省算力。填 0 = 全市场。',
      type: 'number', step: '1', unit: '名',
    });
    rowTopN.__input.value = u.top_n_by_volume != null ? u.top_n_by_volume : 80;

    // ---------- ⑤ 加仓条件 ----------
    var rowAddonOn = selRow({
      id: 'addonOn', title: '加仓总开关',
      desc: '关掉后已持仓不再补仓。',
      options: [{ v: '1', t: '开启' }, { v: '0', t: '关闭' }],
    });
    rowAddonOn.__input.value = (a.enabled === false) ? '0' : '1';

    // 模式卡片（两套判据并存，切模式不丢参数）
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

    var rowAddonScore = condRow({
      id: 'addonScore', title: '加仓分数门槛',
      desc: '判定为「score > 门槛」（严格大于，与买入的 ≥ 刻意不同）。写 0 = 跟随买入的分数门槛。',
      type: 'number', step: '1',
    });
    rowAddonScore.__input.value = a.score_threshold != null ? a.score_threshold : 2;
    rowAddonScore.__cb.checked = (a.score_threshold || 0) > 0;
    rowAddonScore.__sync();

    var rowAddonRise = numRow({
      id: 'addonRise', title: '加仓 K 线涨幅门槛',
      desc: '触发那根自身涨幅须严格大于该值。必须 > 0（写 0 会被反压回默认）。',
      type: 'number', step: '0.1', unit: '%',
    });
    rowAddonRise.__input.value = a.bar_rise_pct != null ? a.bar_rise_pct : 0.7;

    var rowDrop = numRow({
      id: 'addonDrop', title: '跌破买价幅度',
      desc: '收盘价比买入价低超过该幅度才加仓（价格模式）。必须 > 0。',
      type: 'number', step: '0.1', unit: '%',
    });
    rowDrop.__input.value = a.drop_pct != null ? a.drop_pct : 1;

    var rowPriceRise = numRow({
      id: 'addonPriceRise', title: '该根涨幅门槛',
      desc: '价格模式下触发那根的涨幅须严格大于该值。必须 > 0。',
      type: 'number', step: '0.1', unit: '%',
    });
    rowPriceRise.__input.value = a.price_rise_pct != null ? a.price_rise_pct : 1;

    var rowAddonAmt = numRow({
      id: 'addonAmt', title: '每次加仓金额',
      desc: '直接指定加仓金额（U）。填 0 = 改用下面的「加仓比例」。',
      type: 'number', step: '0.01', unit: 'U',
    });
    rowAddonAmt.__input.value = a.margin_usdt != null ? a.margin_usdt : 0;

    var rowAddonRatio = numRow({
      id: 'addonRatio', title: '加仓比例',
      desc: '加仓额 = 原持仓保证金 × 该比例。仅当「每次加仓金额」为 0 时生效，必须 > 0。',
      type: 'number', step: '0.01', unit: '倍',
    });
    rowAddonRatio.__input.value = a.ratio != null ? a.ratio : 0.3333333333;

    var rowAddonMax = numRow({
      id: 'addonMax', title: '加仓次数上限',
      desc: '每个仓位最多加几次。填 0 = 不限。',
      type: 'number', step: '1', unit: '次',
    });
    rowAddonMax.__input.value = a.max_times != null ? a.max_times : 0;

    var rowAddonGap = numRow({
      id: 'addonGap', title: '两次加仓最小间隔',
      desc: '至少隔多少根。默认 1（同一根只加一次）；填 0 = 同一根也能加。',
      type: 'number', step: '1', unit: '根',
    });
    rowAddonGap.__input.value = a.min_gap_bars != null ? a.min_gap_bars : 1;

    var rowRiseBar = selRow({
      id: 'addonRiseBar', title: '加仓判定周期',
      desc: 'auto = 用该仓位自己开仓时的周期（3m/5m）。15m 已下线（二十一期），老仓自动退回 5m。',
      options: [
        { v: 'auto', t: 'auto 跟随仓位周期' },
        { v: '3m', t: '3 分钟' },
        { v: '5m', t: '5 分钟' },
      ],
    });
    rowRiseBar.__input.value = a.rise_bar || 'auto';

    var riseBox = grid([rowAddonScore, rowAddonRise]);
    var priceBox = grid([rowDrop, rowPriceRise]);
    function syncAddonRows() {
      riseBox.style.display = mode === 'resonance' ? '' : 'none';
      priceBox.style.display = mode === 'price' ? '' : 'none';
    }
    syncAddonRows();

    // ---------- ⑥ 出场条件 ----------
    var rowTp = numRow({
      id: 'exitTp', title: '止盈',
      desc: '浮盈到该百分比立刻市价平。实测关掉（填 0）后，超时平仓那一刻的盈亏纯随机、净值反而为负。',
      type: 'number', step: '0.05', unit: '%',
    });
    rowTp.__input.value = ex.take_profit_pct != null ? ex.take_profit_pct : 0.35;

    var rowSl = numRow({
      id: 'exitSl', title: '止损',
      desc: '浮亏到该百分比平仓。300 表示 -300%（物理上到不了）= 等效不设止损。填 0 = 关闭。',
      type: 'number', step: '1', unit: '%',
    });
    rowSl.__input.value = ex.stop_loss_pct != null ? ex.stop_loss_pct : 300;

    var rowHoldMin = numRow({
      id: 'exitHoldMin', title: '超时平仓（分钟）',
      desc: '开仓满这么多分钟自动市价平掉，是没摸到止盈线时的兜底离场。1440 = 24 小时。',
      type: 'number', step: '1', unit: '分钟',
    });
    rowHoldMin.__input.value = ex.max_hold_minutes != null ? ex.max_hold_minutes : 1440;

    var rowHoldBars = numRow({
      id: 'exitHoldBars', title: '超时平仓（根）',
      desc: '按根数算的超时（旧口径）。> 0 时才生效，且会被上面的分钟覆盖。一般留 0。',
      type: 'number', step: '1', unit: '根',
    });
    rowHoldBars.__input.value = ex.max_hold_bars != null ? ex.max_hold_bars : 0;

    var rowBoll = selRow({
      id: 'exitBoll', title: '布林上轨出场',
      desc: '与买入读同一根 K 线，开仓后下一轮巡检就可能反手平掉（实测 SNDK 开仓 15 秒即平、亏 0.17%）。建议保持关闭。',
      options: [{ v: '0', t: '关闭（推荐）' }, { v: '1', t: '开启' }],
    });
    rowBoll.__input.value = (ex.boll_upper_exit === true) ? '1' : '0';

    // ---------- ⑦ 风控 ----------
    var rowAcctStop = numRow({
      id: 'riskAcctStop', title: '权益熔断',
      desc: '账户权益低于该值全停。填 0 = 不启用。',
      type: 'number', step: '0.1', unit: 'U',
    });
    rowAcctStop.__input.value = rk.account_equity_stop != null ? rk.account_equity_stop : 0;

    var rowDailyLoss = numRow({
      id: 'riskDailyLoss', title: '当日亏损停开仓',
      desc: '当日亏损超过权益的该比例就停止开仓。填 0 = 不按此停。',
      type: 'number', step: '1', unit: '%',
    });
    rowDailyLoss.__input.value = rk.daily_loss_stop_pct != null ? rk.daily_loss_stop_pct : 50;

    var rowTotalMargin = numRow({
      id: 'riskTotalMargin', title: '总保证金占比上限',
      desc: '总保证金不超过权益的该比例。填 0 = 不启用。',
      type: 'number', step: '1', unit: '%',
    });
    rowTotalMargin.__input.value = rk.max_total_margin_pct != null ? rk.max_total_margin_pct : 100;

    var rowMinAvail = numRow({
      id: 'riskMinAvail', title: '可用余额下限',
      desc: '可用余额低于该值就停止开仓。填 0 = 不启用。',
      type: 'number', step: '0.01', unit: 'U',
    });
    rowMinAvail.__input.value = rk.min_available_usdt != null ? rk.min_available_usdt : 0.1;

    var rowLossPause = numRow({
      id: 'riskLossPause', title: '连亏暂停',
      desc: '连续亏这么多笔就暂停 2 小时。填 0 = 不暂停。',
      type: 'number', step: '1', unit: '笔',
    });
    rowLossPause.__input.value = rk.consecutive_loss_pause != null ? rk.consecutive_loss_pause : 5;

    var rowApiPause = numRow({
      id: 'riskApiPause', title: 'API 错误暂停',
      desc: '连续这么多次接口错误就暂停。填 0 = 不启用。',
      type: 'number', step: '1', unit: '次',
    });
    rowApiPause.__input.value = rk.pause_on_api_error != null ? rk.pause_on_api_error : 10;

    // ---------- ⑧ 引擎节奏 ----------
    var rowExitSec = numRow({
      id: 'liveExitSec', title: '止盈巡检间隔',
      desc: '每多少秒看一眼持仓浮盈。服务端下限 1 秒 —— 填 0 会回到下限，不会真的停掉巡检。',
      type: 'number', step: '1', unit: '秒',
    });
    rowExitSec.__input.value = lv.live_exit_sec != null ? lv.live_exit_sec : 3;

    var rowEntrySec = numRow({
      id: 'liveEntrySec', title: '买入扫描间隔',
      desc: '每多少秒全市场扫一次买入信号。服务端下限 5 秒。',
      type: 'number', step: '1', unit: '秒',
    });
    rowEntrySec.__input.value = lv.live_entry_sec != null ? lv.live_entry_sec : 60;

    // ---- 结果条 ----
    var resultBox = el('div', { class: 'adm-result' });
    resultBox.style.display = 'none';
    function setResult(html, cls) {
      resultBox.innerHTML = html;
      resultBox.className = 'adm-result ' + (cls || '');
      resultBox.style.display = '';
      resultBox.scrollIntoView({ block: 'nearest' });
    }

    // ---- 收集并保存 ----
    var saveBtn, cancelBtn;
    function collect() {
      var p = {};

      // ① 总开关
      p.buy_enabled = rowBuyOn.__input.value === '1';
      p.dry_run = rowDryRun.__input.value === '1';

      // ② 买入
      var st = rowScore.__cb.checked ? intOf(rowScore.__input) : 0;
      if (isNaN(st) || st < 0 || st > 8) return { error: '分数门槛要在 0~8 之间（8 个因子）' };
      p.score_threshold = st;

      var rise = rowRise.__cb.checked ? num(rowRise.__input.value) : 0;
      if (isNaN(rise)) return { error: '买入 K 线涨跌幅门槛不是数字' };
      p.min_bar_rise_pct = rise;

      var bm = num(rowBuyAmt.__input.value);
      if (isNaN(bm) || bm <= 0) return { error: '每笔买入保证金必须大于 0（写 0 会被服务端反压回默认值，等于没改）' };
      p.buy_margin_usdt = bm;

      var cap = num(rowBuyCap.__input.value);
      if (isNaN(cap) || cap <= 0) return { error: '单笔金额硬上限必须大于 0' };
      p.max_margin_usdt = cap;

      var lev = intOf(rowLeverage.__input);
      if (isNaN(lev) || lev <= 0) return { error: '杠杆必须大于 0' };
      p.leverage = lev;

      p.margin_policy = rowPolicy.__input.value;

      // ③ 开仓闸门
      //
      //   ★ 十期：这里**不再发 cooldown_bars** ★
      //     用户的诉求是「冷却条件全部删除」，所以前端整个不再提这个字段。
      //     Patch 是指针式的：没发的字段 = 不改，配置里的值原样保留。
      //     目前落盘的 cooldown_bars = 0（= 不冷却），引擎侧也有 cd<=0 的显式放行。
      var mp = intOf(rowMaxPos.__input);
      if (isNaN(mp) || mp < 0) return { error: '最多同时持仓要是 0 或正整数（0 = 不限）' };
      p.max_concurrent_positions = mp;

      var dm = intOf(rowDailyMax.__input);
      if (isNaN(dm) || dm < 0) return { error: '当日开仓上限要是 0 或正整数（0 = 不限）' };
      p.daily_max_entries = dm;

      // ④ 准入
      var mo = num(rowMaxOrder.__input.value);
      if (isNaN(mo) || mo <= 0) return { error: '准入单笔上限必须大于 0' };
      p.max_order_margin_usdt = mo;

      var nd = intOf(rowNewDays.__input);
      if (isNaN(nd) || nd < 0) return { error: '新上线排除天数要是 0 或正整数' };
      p.exclude_new_listing_days = nd;

      p.exclude_delisting = rowDelist.__input.value === '1';
      p.exclude_stock_etf = rowStockEtf.__input.value === '1';

      var qv = num(rowMinVol.__input.value);
      if (isNaN(qv) || qv < 0) return { error: '24h 成交额下限要是 0 或正数' };
      p.min_quote_volume_24h = qv;

      var tn = intOf(rowTopN.__input);
      if (isNaN(tn) || tn < 0) return { error: '成交额前 N 名要是 0 或正整数' };
      p.top_n_by_volume = tn;

      // ⑤ 加仓
      p.addon_enabled = rowAddonOn.__input.value === '1';
      p.addon_mode = mode;
      if (mode === 'resonance') {
        var as = rowAddonScore.__cb.checked ? intOf(rowAddonScore.__input) : 0;
        if (isNaN(as) || as < 0) return { error: '加仓分数门槛要是 0 或正整数' };
        p.addon_score = as;
        var ar = num(rowAddonRise.__input.value);
        if (isNaN(ar) || ar <= 0) return { error: '加仓 K 线涨幅门槛必须大于 0' };
        p.addon_bar_rise = ar;
      } else {
        var dp = num(rowDrop.__input.value);
        if (isNaN(dp) || dp <= 0) return { error: '跌破买价幅度必须大于 0' };
        p.addon_drop_pct = dp;
        var pr = num(rowPriceRise.__input.value);
        if (isNaN(pr) || pr <= 0) return { error: '该根涨幅门槛必须大于 0' };
        p.addon_rise_pct = pr;
      }

      var am = num(rowAddonAmt.__input.value);
      if (isNaN(am) || am < 0) return { error: '每次加仓金额要是 0 或正数' };
      p.addon_margin_usdt = am;

      var ra = num(rowAddonRatio.__input.value);
      if (isNaN(ra) || ra <= 0) return { error: '加仓比例必须大于 0' };
      p.addon_ratio = ra;

      var mt = intOf(rowAddonMax.__input);
      if (isNaN(mt) || mt < 0) return { error: '加仓次数上限要是 0 或正整数' };
      p.addon_max_times = mt;

      var mg = intOf(rowAddonGap.__input);
      if (isNaN(mg) || mg < 0) return { error: '两次加仓最小间隔要是 0 或正整数' };
      p.addon_min_gap_bars = mg;

      p.addon_rise_bar = rowRiseBar.__input.value;

      // ⑥ 出场
      var tp = num(rowTp.__input.value);
      if (isNaN(tp) || tp < 0) return { error: '止盈要 ≥ 0' };
      p.take_profit_pct = tp;

      var sl = num(rowSl.__input.value);
      if (isNaN(sl) || sl < 0) return { error: '止损要 ≥ 0' };
      p.stop_loss_pct = sl;

      var hm = intOf(rowHoldMin.__input);
      if (isNaN(hm) || hm < 0) return { error: '超时分钟要是 0 或正整数' };
      p.max_hold_minutes = hm;

      var hb = intOf(rowHoldBars.__input);
      if (isNaN(hb) || hb < 0) return { error: '超时根数要是 0 或正整数' };
      p.max_hold_bars = hb;

      p.boll_upper_exit = rowBoll.__input.value === '1';

      // ⑦ 风控
      var av = num(rowAcctStop.__input.value);
      if (isNaN(av) || av < 0) return { error: '权益熔断要 ≥ 0' };
      p.account_equity_stop = av;

      var dl = num(rowDailyLoss.__input.value);
      if (isNaN(dl) || dl < 0) return { error: '当日亏损停开仓要 ≥ 0' };
      p.daily_loss_stop_pct = dl;

      var tm = num(rowTotalMargin.__input.value);
      if (isNaN(tm) || tm < 0) return { error: '总保证金占比上限要 ≥ 0' };
      p.max_total_margin_pct = tm;

      var ma = num(rowMinAvail.__input.value);
      if (isNaN(ma) || ma < 0) return { error: '可用余额下限要 ≥ 0' };
      p.min_available_usdt = ma;

      var lp = intOf(rowLossPause.__input);
      if (isNaN(lp) || lp < 0) return { error: '连亏暂停笔数要 ≥ 0' };
      p.consecutive_loss_pause = lp;

      var ap = intOf(rowApiPause.__input);
      if (isNaN(ap) || ap < 0) return { error: 'API 错误暂停次数要 ≥ 0' };
      p.pause_on_api_error = ap;

      // ⑧ 引擎
      var xs = intOf(rowExitSec.__input);
      if (isNaN(xs) || xs < 1) return { error: '止盈巡检间隔要 ≥ 1 秒' };
      p.live_exit_sec = xs;

      var es = intOf(rowEntrySec.__input);
      if (isNaN(es) || es < 5) return { error: '买入扫描间隔要 ≥ 5 秒' };
      p.live_entry_sec = es;

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

        // 保存成功 → 重新拉一份配置回来刷新表单基准值。
        // ★ 为什么重新拉而不是拿 j.after 自己拼 ★
        //   after 是**扁平的**字段快照，而表单是按分区渲染的，
        //   手工拼回分区结构容易漏（八期就写错过一次：把 after 当成了 buy 段）。
        //   重新拉一次最省事，也顺便验证「写盘 → 热重载 → 读回」整条链通了。
        api('/api/admin/config').then(function (k) { if (k && k.ok) state.cfg = k; });

        var lines = (j.changed || []).map(function (s) { return '· ' + s; }).join('<br>');
        setResult('<b>✓ 已保存并立刻热重载生效</b>（无需重启）<br>' +
          (lines || '· （无字段变化）') +
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
        sec('① 总开关', grid([rowBuyOn, rowDryRun])),
        sec('② 买入条件', grid([rowScore, rowRise, rowBuyAmt, rowBuyCap, rowLeverage, rowPolicy])),
        sec('③ 开仓闸门（数量限制）', grid([rowMaxPos, rowDailyMax])),
        sec('④ 合约准入', grid([rowMaxOrder, rowNewDays, rowDelist, rowStockEtf, rowMinVol, rowTopN])),
        sec('⑤ 加仓条件', [modeBox, grid([rowAddonOn]), riseBox, priceBox,
          grid([rowAddonAmt, rowAddonRatio, rowAddonMax, rowAddonGap, rowRiseBar])]),
        sec('⑥ 出场条件', grid([rowTp, rowSl, rowHoldMin, rowHoldBars, rowBoll])),
        sec('⑦ 风控', grid([rowAcctStop, rowDailyLoss, rowTotalMargin, rowMinAvail,
          rowLossPause, rowApiPause])),
        sec('⑧ 引擎节奏', grid([rowExitSec, rowEntrySec])),
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
