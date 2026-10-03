/* ================================================================
 * signalalert.js —— 买入信号「网页声音提醒」
 *
 * 用户口径（2026-10-03）：
 *   「盘中有信号就实时提醒买入，买入条件就是 macd>0 and ref macd<0
 *    refref macd<0」+「不要客户端不要收邮件，就给我网页播放声音」
 *
 * 所以本文件做三件事：
 *   ① 每 15 秒轮询 /api/takersignal?since=<已看到的水位线>
 *      —— 增量语义：返回空数组就等于「没有新信号」，不用传 30 天数据
 *   ② 有新信号 → Web Audio 响铃（不用 mp3：省一次请求、也不怕文件丢失）
 *            → 右上角弹横幅（含判定依据 val/prev/prev2，出了事能对账）
 *            → 标签页标题加 🔔（用户切到别的标签也能看见）
 *   ③ 全部开关状态存 localStorage；声音开关必须由**用户点击**打开
 *      —— 浏览器自动播放策略：没有用户手势，AudioContext 起不来。
 *
 * ------------------------------------------------------------------
 * 三条踩过的坑（本项目老毛病，这里一次性避开）
 * ------------------------------------------------------------------
 * ① 全局名冲突：core.js 已有 fmtVol/fmtPct/fmtTime…，重名会让**整个文件**
 *    SyntaxError 静默不执行。这里所有函数/变量一律 sig 前缀。
 * ② fetch 永不落定（低内存机器实测）→ loading 卡死、轮询全灭且不报错。
 *    对策：AbortController 12 秒超时 + loadingAt 自解（超过 60 秒就当上一轮死了）。
 * ③ 「先记后取」：边界/水位线**只有请求成功了才推进**。这里 seen 只在
 *    成功回调里更新，失败保持原值 —— 下一轮自然重试同一段，不会漏也不会重。
 * ================================================================ */

// 三个口径的中文名（与 service/taker_signal.go 一一对应）
var SIG_RULE_LABEL = { hist: 'MACD柱', dif: 'DIF', dea: 'DEA' };

var sigState = {
  on: false,          // 声音开关（默认关：浏览器不点不出声，索性默认静音）
  rule: 'hist',       // 当前口径
  seen: {},           // rule -> 已提醒到哪个 ts（水位线，只在成功时推进）
  inited: {},         // rule -> 是否已初始化过水位线（首屏不补播历史）
  loading: false,
  loadingAt: 0,
  ctx: null,          // AudioContext（用户点击后创建/resume）
  timer: null,
  toasts: 0,          // 当前横幅数（上限 4，防刷屏）
  titleTimer: null,
};

/* ------------------------------------------------------------------ */
/* 音频：三声上行短音（880 / 1174 / 1568 Hz）                            */
/* ------------------------------------------------------------------ */
/* 为什么用 Web Audio 而不是 <audio src=xxx.mp3>：
 *   ① 少一个静态资源（也不怕用户清缓存 / 路径写错后静默失败）；
 *   ② 三声的节奏和频率都在代码里，改起来是一行数字，不用找音频文件；
 *   ③ AudioContext 的 resume 状态可查，能明确告诉用户「声音还没激活」。   */
function sigEnsureCtx() {
  if (sigState.ctx) return sigState.ctx;
  var AC = window.AudioContext || window.webkitAudioContext;
  if (!AC) return null;
  try { sigState.ctx = new AC(); } catch (e) { return null; }
  return sigState.ctx;
}

// sigResume 尝试激活音频（必须在用户手势的调用栈里才有效）
function sigResume() {
  var ctx = sigEnsureCtx();
  if (!ctx) return false;
  if (ctx.state === 'suspended') { try { ctx.resume(); } catch (e) {} }
  return ctx.state !== 'suspended';
}

// sigBeep 响铃：三声上行，~0.55 秒，够抓耳朵又不至于吵
function sigBeep() {
  if (!sigState.on) return;
  var ctx = sigEnsureCtx();
  if (!ctx) return;
  if (ctx.state === 'suspended') { try { ctx.resume(); } catch (e) {} }
  var t0 = ctx.currentTime;
  var notes = [880, 1174, 1568];
  notes.forEach(function (f, i) {
    var start = t0 + i * 0.18;
    var osc = ctx.createOscillator();
    var gain = ctx.createGain();
    osc.type = 'sine';
    osc.frequency.setValueAtTime(f, start);
    // 包络：快起慢落，避免「咔」的爆音
    gain.gain.setValueAtTime(0.0001, start);
    gain.gain.exponentialRampToValueAtTime(0.32, start + 0.02);
    gain.gain.exponentialRampToValueAtTime(0.0001, start + 0.16);
    osc.connect(gain);
    gain.connect(ctx.destination);
    osc.start(start);
    osc.stop(start + 0.18);
  });
}

/* ------------------------------------------------------------------ */
/* 横幅（右上角）                                                        */
/* ------------------------------------------------------------------ */
function sigFmtTime(ms) {
  var d = new Date(ms);
  var p = function (n) { return (n < 10 ? '0' : '') + n; };
  return p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' + p(d.getHours()) + ':' + p(d.getMinutes());
}

// sigToast 弹一条买入横幅
function sigToast(s) {
  var host = document.getElementById('sigToasts');
  if (!host) return;

  // 上限 4 条：信号密集时防止把整个右上角糊满（老的先走）
  while (host.children.length >= 4) host.removeChild(host.firstChild);

  var fv = function (x) { return (x >= 0 ? '+' : '') + Number(x).toFixed(5); };
  var el = document.createElement('div');
  el.className = 'sig-toast';
  var rise = '';
  if (s.eth_rise_ok) {
    var r = Number(s.eth_rise);
    rise = '<span class="sig-rise ' + (r > 0 ? 'up' : (r < 0 ? 'down' : 'flat')) + '">' +
      '该根 ETH ' + (r >= 0 ? '+' : '') + r.toFixed(2) + '%</span>';
  }
  el.innerHTML =
    '<div class="sig-head"><b>🔔 买入信号</b>' +
      '<span class="sig-rule">' + (SIG_RULE_LABEL[sigState.rule] || sigState.rule) + '</span>' +
      '<span class="sig-time">' + sigFmtTime(s.ts) + ' 收盘</span>' +
      '<button class="sig-x" title="关闭">×</button></div>' +
    '<div class="sig-body">当前 ' + fv(s.val) + ' &gt; 0　前一根 ' + fv(s.prev) +
      ' &lt; 0　前两根 ' + fv(s.prev2) + ' &lt; 0</div>' +
    '<div class="sig-foot"><span class="muted">买卖比 ' + Number(s.ratio).toFixed(3) + '</span>' + rise + '</div>';

  el.querySelector('.sig-x').addEventListener('click', function () { el.remove(); });
  host.appendChild(el);
  // 90 秒自动收走（信号是「立刻要你的注意」，不是常驻信息）
  setTimeout(function () { if (el.parentNode) el.remove(); }, 90000);

  sigFlashTitle();
}

// sigNotice 提示横幅（不是买入信号，只是状态说明）
function sigNotice(msg) {
  var host = document.getElementById('sigToasts');
  if (!host) return;
  while (host.children.length >= 4) host.removeChild(host.firstChild);
  var el = document.createElement('div');
  el.className = 'sig-toast sig-notice';
  el.innerHTML = '<div class="sig-head"><b>ℹ 信号提醒</b>' +
    '<button class="sig-x" title="关闭">×</button></div>' +
    '<div class="sig-body">' + esc(msg) + '</div>';
  el.querySelector('.sig-x').addEventListener('click', function () { el.remove(); });
  host.appendChild(el);
  setTimeout(function () { if (el.parentNode) el.remove(); }, 20000);
}

// sigFlashTitle 标题加 🔔（用户切到别的标签页也能看到）
function sigFlashTitle() {
  var base = document.title.replace(/^🔔\s*/, '');
  document.title = '🔔 ' + base;
  if (sigState.titleTimer) clearTimeout(sigState.titleTimer);
  sigState.titleTimer = setTimeout(function () { document.title = base; }, 60000);
}

/* ------------------------------------------------------------------ */
/* 拉取                                                                */
/* ------------------------------------------------------------------ */
// sigApplySeen 只有成功拿到数据才推进水位线（坑 ③）
function sigApplySeen(rule, list, newest) {
  var mx = sigState.seen[rule] || 0;
  (list || []).forEach(function (s) { if (s.ts > mx) mx = s.ts; });
  if (newest && newest > mx) mx = newest;
  sigState.seen[rule] = mx;
}

// sigPoll 拉一次信号
//
//   init=true  → 不带 since，取最新 50 条**只用来初始化水位线**，
//                不响铃不弹窗（否则一打开网页就把 30 天前的信号全播一遍）
//   init=false → 带 since，只取比水位线新的；有就响铃 + 弹窗
function sigPoll(init) {
  var now = Date.now();
  if (sigState.loading) {
    if (now - (sigState.loadingAt || 0) < 60000) return Promise.resolve();
    console.warn('sigPoll 上一轮疑似卡死（低内存机器 fetch 不落定的老毛病），强制重试');
  }
  sigState.loading = true;
  sigState.loadingAt = now;

  var rule = sigState.rule;
  var ctl = typeof AbortController !== 'undefined' ? new AbortController() : null;
  var timer = ctl ? setTimeout(function () { ctl.abort(); }, 12000) : null;

  var url = '/api/takersignal?rule=' + encodeURIComponent(rule) + '&limit=50';
  var seen = sigState.seen[rule] || 0;
  if (!init && seen > 0) url += '&since=' + seen;

  return api(url, { signal: ctl && ctl.signal }).then(function (j) {
    var list = (j && j.signals) || [];
    var newest = (j && j.newest) || 0;

    if (init || !sigState.inited[rule]) {
      sigApplySeen(rule, list, newest);
      sigState.inited[rule] = true;
      sigHint();
      return;
    }

    // 口径可能在中途被改过（用户点下拉的瞬间有请求在飞）——丢弃过期结果
    if (j && j.rule && j.rule !== sigState.rule) return;

    if (!list.length) return;
    // 一次多条（例如页面睡了 20 分钟）也逐条弹，但声音只响一次 ——
    // 连响 N 次只会让人把提醒关掉。
    list.forEach(function (s) { sigToast(s); });
    sigBeep();
    sigApplySeen(rule, list, newest);
    sigHint();
  }).catch(function (e) {
    // ★ 失败**不推进** seen：下一轮拿同一段重试，不漏不重
    console.warn('sigPoll 失败', e && e.message ? e.message : e);
    sigHint(e && e.message ? String(e.message) : '接口异常');
  }).then(function () {
    if (timer) clearTimeout(timer);
    sigState.loading = false;
  });
}

/* ------------------------------------------------------------------ */
/* 开关 / 状态提示                                                       */
/* ------------------------------------------------------------------ */
function sigHint(msg) {
  var btn = document.getElementById('sigBtn');
  if (!btn) return;
  var label = SIG_RULE_LABEL[sigState.rule] || sigState.rule;
  if (msg) { btn.textContent = '⚠ 信号提醒'; btn.title = '轮询失败：' + msg; return; }
  btn.textContent = (sigState.on ? '🔔' : '🔕') + ' 信号提醒';
  var seen = sigState.seen[sigState.rule] || 0;
  btn.title = (sigState.on ? '已开启' : '已关闭（点一下开启）') +
    '：' + label + ' > 0 且前两根 < 0 时响铃 + 弹横幅' +
    (seen ? '；已提醒到 ' + sigFmtTime(seen) : '');
}

// sigSetOn 切换声音开关（必须由点击进来，AudioContext 才起得来）
function sigSetOn(on) {
  sigState.on = !!on;
  try { localStorage.setItem('sigSound', sigState.on ? '1' : '0'); } catch (e) {}
  if (sigState.on) {
    if (sigResume()) {
      sigBeep(); // 开启时响一声，让用户确认「真的会响」
    } else {
      sigNotice('浏览器还没放行音频：请再点一下页面任意位置，或检查该标签页是否被静音。');
    }
  }
  sigHint();
}

// sigSetRule 切换口径 → 该口径重新初始化水位线（不补播历史）
function sigSetRule(rule) {
  sigState.rule = rule;
  try { localStorage.setItem('sigRule', rule); } catch (e) {}
  delete sigState.inited[rule];
  delete sigState.seen[rule];
  sigHint();
  sigPoll(true);
}

/* ------------------------------------------------------------------ */
/* 启动                                                                */
/* ------------------------------------------------------------------ */
function sigBoot() {
  var btn = document.getElementById('sigBtn');
  var sel = document.getElementById('sigRule');

  // 恢复上次的选择（声音默认关：宁愿用户点一下，也不要悄悄不出声）
  try {
    if (localStorage.getItem('sigSound') === '1') sigState.on = true;
    var r = localStorage.getItem('sigRule');
    if (r && SIG_RULE_LABEL[r]) sigState.rule = r;
  } catch (e) {}
  if (sel) sel.value = sigState.rule;

  if (btn) {
    btn.addEventListener('click', function () { sigSetOn(!sigState.on); });
  }
  if (sel) {
    sel.addEventListener('change', function () { sigSetRule(sel.value); });
  }

  // 若上次是开着的，浏览器这次仍然需要一次手势才能出声：
  // 挂一个一次性监听，用户第一次点页面任意位置就补上激活。
  if (sigState.on) {
    var once = function () {
      sigResume();
      document.removeEventListener('click', once);
      sigHint();
    };
    document.addEventListener('click', once);
  }

  sigHint();
  // 首屏错峰：boot 瞬间连接池紧张（低内存机器实测），延后 3 秒再拉
  setTimeout(function () { sigPoll(true); }, 3000);
  sigState.timer = setInterval(function () { sigPoll(false); }, 15000);
  // 后台标签页会被浏览器节流，回到前台立刻补一次
  document.addEventListener('visibilitychange', function () {
    if (!document.hidden) sigPoll(false);
  });
}
