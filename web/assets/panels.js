/* ================================================================
 * panels.js —— 十五期「Flex 那套高级感」（用户点名要的全套）：
 *   · 三栏浮动面板：按 ⠿ 拖动脱停，拖动中 scale(1.008) 抬起 + 阴影加深
 *   · 磁性吸附 + 对齐辅助线（贴近其他面板 / 容器边缘 14px 自动贴齐）
 *   · 松手落位；停靠按钮用 FLIP 播 0.28s 弹性归位
 *   · 双击 ⠿ 最大化 / 还原（FLIP）
 *   · 「收起」最小化到底部 Dock（macOS 风），点 chip 弹回
 *   · 可拖拽分隔条（Flex 的 HDividedBox 等价物），双击复位
 *   · 主题切换：暗金（默认）/ macOS 浅色，图表背景同步换
 *   · View Transitions：切下方 tab 时 .tables 区块共享元素补间
 *     （补间本体在 tables.js 的 tab 点击里，这里只负责样式配合）
 * ----------------------------------------------------------------
 * 实现纪律（沿用项目惯例）：
 *   · 零依赖零构建；只动 transform / left/top，不触发布局重排；
 *   · Pointer Events 统一鼠标 / 触摸，document 级监听防丢事件；
 *   · 全部状态（浮动位置 / 最小化 / 栏宽 / 主题）localStorage 持久化；
 *   · 面板 z-index 55~60，远低于管理台遮罩(9000)，不会盖住对话框。
 * ================================================================ */

(function () {
  'use strict';

  var SPRING = 'cubic-bezier(.34,1.56,.64,1)';   // 轻微过冲 = 「活」
  var LS_PANELS = 'okxPanels:v1';
  var LS_COLS = 'okxColW:v1';
  var LS_THEME = 'okxTheme';
  var SNAP = 14;                                  // 磁性吸附半径（px）

  function $(id) { return document.getElementById(id); }

  /* ---------------- FLIP 弹性归位 ----------------
   * First-Last-Invert-Play：量首帧 → 变更 → 量末帧 → 反向平移再播到 0。
   * 全程只动 transform，60fps，这是「窗口移动好看」的核心。 */
  function flip(el, mutate) {
    if (!el) return;
    var a = el.getBoundingClientRect();
    mutate();
    var b = el.getBoundingClientRect();
    var dx = a.left - b.left, dy = a.top - b.top;
    if (!dx && !dy) return;
    if (el.animate) {
      el.animate(
        [{ transform: 'translate(' + dx + 'px,' + dy + 'px)' }, { transform: 'translate(0,0)' }],
        { duration: 280, easing: SPRING }
      );
    }
  }

  /* ---------------- 面板注册 ---------------- */
  var NAMES = { side: '合约列表', center: '图表', right: '合约信息' };
  var PANELS = [
    { key: 'side',   el: document.querySelector('.layout > .panel.side:not(.right)') },
    { key: 'center', el: document.querySelector('.layout > .panel.center') },
    { key: 'right',  el: document.querySelector('.layout > .panel.side.right') },
  ].filter(function (p) { return !!p.el; });

  /* ---------------- 吸附辅助线 ---------------- */
  var gv = $('snapGuideV'), gh = $('snapGuideH');
  function guide(x, y) {
    if (x == null) { gv.style.opacity = '0'; }
    else { gv.style.left = x + 'px'; gv.style.opacity = '1'; }
    if (y == null) { gh.style.opacity = '0'; }
    else { gh.style.top = y + 'px'; gh.style.opacity = '1'; }
  }

  /* ---------------- 图表尺寸同步 ----------------
   * 面板 / 栏宽变化后让 lightweight-charts 重算画布。
   * 不依赖 chart.js 的 window resize 监听，直接 applyOptions 最稳。 */
  function resizeCharts() {
    try {
      var el = $('chart');
      if (el && state.chart) state.chart.applyOptions({ width: el.clientWidth, height: el.clientHeight });
      var pe = $('pnlChart');
      if (pe && state.pnlChart) state.pnlChart.applyOptions({ width: pe.clientWidth, height: pe.clientHeight });
    } catch (e) { /* boot 未完成时静默 */ }
  }

  /* ---------------- 浮动 / 停靠 ---------------- */
  function toFloat(p) {
    if (p.el.classList.contains('floating')) return;
    var r = p.el.getBoundingClientRect();
    p.el.classList.add('floating');
    p.el.style.left = r.left + 'px';
    p.el.style.top = r.top + 'px';
    p.el.style.width = r.width + 'px';
    p.el.style.height = r.height + 'px';
    p.fx = r.left; p.fy = r.top; p.fw = r.width; p.fh = r.height;
    p.mode = 'float';
    updateActs(p);
    save();
  }

  function toDock(p) {
    if (!p.el.classList.contains('floating')) return;
    p.maxed = false;
    p.el.classList.remove('maxed');
    flip(p.el, function () {
      p.el.classList.remove('floating');
      p.el.style.left = '';
      p.el.style.top = '';
      p.el.style.width = '';
      p.el.style.height = '';
      p.el.style.transform = '';
    });
    p.mode = 'dock';
    updateActs(p);
    resizeCharts();
    save();
  }

  /* ---------------- 磁性吸附 ----------------
   * 候选目标：视口边缘(留 8~10px) + 其他可见面板的四条边。
   * 吸附成功时在对齐边上闪现 1px 金色辅助线 —— 专业感的最大来源。 */
  function trySnap(p, x, y, w, h) {
    var xs = [{ t: 10, g: null }, { t: window.innerWidth - 10 - w, g: window.innerWidth - 10 }];
    var ys = [{ t: 8, g: null }, { t: window.innerHeight - 48 - h, g: window.innerHeight - 48 }];
    PANELS.forEach(function (o) {
      if (o === p || o.min || o.el.style.display === 'none') return;
      var r = o.el.getBoundingClientRect();
      if (!r.width) return;
      xs.push({ t: r.left, g: r.left }, { t: r.left - w, g: r.left },
              { t: r.right - w, g: r.right }, { t: r.right, g: r.right });
      ys.push({ t: r.top, g: r.top }, { t: r.top - h, g: r.top },
              { t: r.bottom - h, g: r.bottom }, { t: r.bottom, g: r.bottom });
    });
    var gx = null, gy = null, c, i;
    for (i = 0; i < xs.length; i++) { c = xs[i]; if (Math.abs(x - c.t) < SNAP) { x = c.t; gx = c.g; break; } }
    for (i = 0; i < ys.length; i++) { c = ys[i]; if (Math.abs(y - c.t) < SNAP) { y = c.t; gy = c.g; break; } }
    guide(gx, gy);
    return { x: x, y: y };
  }

  /* ---------------- 拖动 ---------------- */
  function startDrag(e, p) {
    if (e.button !== undefined && e.button !== 0) return;
    e.preventDefault();
    toFloat(p);
    var r = p.el.getBoundingClientRect();
    var ox = e.clientX - r.left, oy = e.clientY - r.top;
    p.el.classList.add('dragging');
    var last = { x: r.left, y: r.top };

    var onMove = function (ev) {
      var x = Math.max(8 - r.width + 90, Math.min(ev.clientX - ox, window.innerWidth - 90));
      var y = Math.max(4, Math.min(ev.clientY - oy, window.innerHeight - 48));
      var s = trySnap(p, x, y, r.width, r.height);
      last = { x: s.x, y: s.y };
      // 拖动中只改 transform：平移 + 微微抬起（scale 1.008）
      p.el.style.transform = 'translate(' + (s.x - r.left) + 'px,' + (s.y - r.top) + 'px) scale(1.008)';
    };
    var onUp = function () {
      document.removeEventListener('pointermove', onMove);
      document.removeEventListener('pointerup', onUp);
      document.removeEventListener('pointercancel', onUp);
      p.el.classList.remove('dragging');
      p.el.style.transform = '';          // 落下：去掉 scale 抬升
      p.fx = last.x; p.fy = last.y;
      p.fw = r.width; p.fh = r.height;
      p.el.style.left = last.x + 'px';
      p.el.style.top = last.y + 'px';
      guide(null, null);
      resizeCharts();
      save();
    };
    document.addEventListener('pointermove', onMove);
    document.addEventListener('pointerup', onUp);
    document.addEventListener('pointercancel', onUp);
  }

  /* ---------------- 双击最大化 / 还原（FLIP）---------------- */
  function toggleMax(p) {
    if (!p.el.classList.contains('floating')) toFloat(p);
    var saved = {
      left: p.el.style.left, top: p.el.style.top,
      width: p.el.style.width, height: p.el.style.height
    };
    if (p.maxed) {
      var s = p.savedStyle || saved;
      flip(p.el, function () { Object.assign(p.el.style, s); });
      p.el.classList.remove('maxed');
      p.maxed = false;
    } else {
      p.savedStyle = saved;
      flip(p.el, function () {
        p.el.style.left = '8px';
        p.el.style.top = '8px';
        p.el.style.width = (window.innerWidth - 16) + 'px';
        p.el.style.height = (window.innerHeight - 16) + 'px';
      });
      p.el.classList.add('maxed');
      p.maxed = true;
    }
    resizeCharts();
    save();
  }

  /* ---------------- 最小化到 Dock ---------------- */
  function addChip(p) {
    var dock = $('panelDock');
    var b = document.createElement('button');
    b.className = 'dock-chip';
    b.textContent = NAMES[p.key] || p.key;
    b.onclick = function () { unminimize(p); };
    dock.appendChild(b);
    dock.classList.remove('hidden');
  }
  function removeChip(p) {
    var dock = $('panelDock');
    var name = NAMES[p.key] || p.key;
    Array.prototype.slice.call(dock.children).forEach(function (c) {
      if (c.textContent === name) c.remove();
    });
    if (!dock.children.length) dock.classList.add('hidden');
  }
  function minimize(p, silent) {
    if (p.min) return;
    p.min = true;
    p.wasFloat = p.el.classList.contains('floating');
    p.el.style.display = 'none';
    addChip(p);
    resizeCharts();
    if (!silent) save();
  }
  function unminimize(p) {
    p.min = false;
    p.el.style.display = '';
    if (!p.wasFloat) toDock(p);   // 原本是停靠的 → 收起再展开仍回停靠位
    removeChip(p);
    resizeCharts();
    save();
  }

  /* ---------------- 每个面板的抓手 + 按钮组 ---------------- */
  function updateActs(p) {
    if (p.acts) p.acts.classList.toggle('hidden', !p.el.classList.contains('floating'));
  }
  function setupPanel(p) {
    var host = p.el.querySelector('.panel-head') || p.el.querySelector('.ch-main');
    if (!host) return;
    var grip = document.createElement('span');
    grip.className = 'ph-grip';
    grip.textContent = '⠿';
    grip.title = '按住拖动（松手磁性吸附）· 双击最大化/还原';
    grip.addEventListener('pointerdown', function (e) { startDrag(e, p); });
    grip.addEventListener('dblclick', function () { toggleMax(p); });
    host.prepend(grip);

    var acts = document.createElement('span');
    acts.className = 'ph-acts hidden';
    acts.innerHTML =
      '<button class="ph-act" data-act="dock" title="停靠回原来的格子">停靠</button>' +
      '<button class="ph-act" data-act="min" title="最小化到底部 Dock">收起</button>';
    host.appendChild(acts);
    p.acts = acts;

    p.el.addEventListener('click', function (e) {
      var b = e.target.closest('.ph-act');
      if (!b) return;
      if (b.dataset.act === 'dock') toDock(p);
      else minimize(p);
    });
  }

  /* ---------------- 持久化 ---------------- */
  function save() {
    var d = {};
    PANELS.forEach(function (p) {
      d[p.key] = {
        mode: p.mode || 'dock', x: Math.round(p.fx || 0), y: Math.round(p.fy || 0),
        w: Math.round(p.fw || 0), h: Math.round(p.fh || 0),
        min: !!p.min, wasFloat: !!p.wasFloat
      };
    });
    try { localStorage.setItem(LS_PANELS, JSON.stringify(d)); } catch (e) {}
  }
  function clampIntoView(p) {
    var x = Math.max(8 - p.fw + 90, Math.min(p.fx, window.innerWidth - 90));
    var y = Math.max(4, Math.min(p.fy, window.innerHeight - 48));
    p.el.style.left = x + 'px';
    p.el.style.top = y + 'px';
    p.fx = x; p.fy = y;
  }
  function restore() {
    var d = null;
    try { d = JSON.parse(localStorage.getItem(LS_PANELS) || 'null'); } catch (e) {}
    if (!d) return;
    PANELS.forEach(function (p) {
      var s = d[p.key];
      if (!s) return;
      if (s.min) { p.wasFloat = s.wasFloat; minimize(p, true); return; }
      if (s.mode === 'float' && s.w > 120 && s.h > 120) {
        toFloat(p);
        p.el.style.left = s.x + 'px';
        p.el.style.top = s.y + 'px';
        p.el.style.width = s.w + 'px';
        p.el.style.height = s.h + 'px';
        p.fx = s.x; p.fy = s.y; p.fw = s.w; p.fh = s.h;
        clampIntoView(p);
      }
    });
  }

  /* ---------------- 可拖拽分隔条（HDividedBox 等价物）---------------- */
  function initSplits() {
    // ★ 二十二期：加了第三个分隔条 T（taker 买卖流向面板）。
    //   三者的物理含义不同，拖动方向也不同：
    //     L：「合约列表」右边界 → 往右拖变宽（+d）
    //     T：「买卖流向」右边界   → 往右拖变宽（+d）
    //     R：「合约信息」左边界   → 往右拖变窄（-d）
    //   所以不能只用「side==='L' ? +d : -d」——那样 T 会被判成反向。
    var VAR = { L: '--side-w', T: '--taker-w', R: '--right-w' };
    var DIR = { L: 1, T: 1, R: -1 };
    var DEF = { L: 268, T: 330, R: 268 };
    var col = Object.assign({}, DEF);
    try { Object.assign(col, JSON.parse(localStorage.getItem(LS_COLS) || 'null') || {}); } catch (e) {}
    var apply = function () {
      Object.keys(VAR).forEach(function (k) {
        document.documentElement.style.setProperty(VAR[k], (col[k] || DEF[k]) + 'px');
      });
    };
    apply();
    Array.prototype.slice.call(document.querySelectorAll('.v-split')).forEach(function (sp) {
      var side = sp.dataset.split;   // 'L' | 'T' | 'R'
      if (!VAR[side]) return;
      var startX = 0, startW = 0;
      sp.addEventListener('pointerdown', function (e) {
        e.preventDefault();
        startX = e.clientX;
        startW = col[side] || DEF[side];
        var onMove = function (ev) {
          var d = (ev.clientX - startX) * DIR[side];
          col[side] = Math.max(160, Math.min(startW + d, 560));
          apply();
        };
        var onUp = function () {
          document.removeEventListener('pointermove', onMove);
          document.removeEventListener('pointerup', onUp);
          try { localStorage.setItem(LS_COLS, JSON.stringify(col)); } catch (err) {}
          resizeCharts();
        };
        document.addEventListener('pointermove', onMove);
        document.addEventListener('pointerup', onUp);
      });
      sp.addEventListener('dblclick', function () {
        col[side] = DEF[side];
        apply();
        resizeCharts();
        // 只复位这一条，别把另外两条也重置 —— 老写法整块 removeItem
        // 会让用户双击任一分隔条就把三条宽度全打回默认。
        try { localStorage.setItem(LS_COLS, JSON.stringify(col)); } catch (err) {}
      });
    });
  }

  /* ---------------- 主题切换（暗金 / macOS 浅色）---------------- */
  var CHART_THEME = {
    dark:  { bg: '#0b0e11', tx: '#b7bdc6', grid: 'rgba(48,54,64,.4)' },
    light: { bg: '#ffffff', tx: '#3a3a3e', grid: 'rgba(0,0,0,.08)' },
  };
  function chartTheme(t) {
    var c = CHART_THEME[t] || CHART_THEME.dark;
    var opt = {
      layout: { background: { type: 'solid', color: c.bg }, textColor: c.tx },
      grid: { vertLines: { color: c.grid }, horzLines: { color: c.grid } },
    };
    try { if (state.chart) state.chart.applyOptions(opt); } catch (e) {}
    try { if (state.pnlChart) state.pnlChart.applyOptions(opt); } catch (e) {}
  }
  function initTheme() {
    var btn = $('btnTheme');
    if (!btn) return;
    var apply = function (t) {
      document.documentElement.dataset.theme = t;
      btn.textContent = t === 'light' ? '暗色' : '浅色';
      chartTheme(t);
      try { localStorage.setItem(LS_THEME, t); } catch (e) {}
    };
    var saved = null;
    try { saved = localStorage.getItem(LS_THEME); } catch (e) {}
    if (saved === 'light') apply('light');   // 图表初始就是暗色，只需恢复浅色
    btn.addEventListener('click', function () {
      var next = document.documentElement.dataset.theme === 'light' ? 'dark' : 'light';
      // 主题切换用 View Transitions 全页补间 —— 状态过渡的高级感
      if (document.startViewTransition) document.startViewTransition(function () { apply(next); });
      else apply(next);
    });
  }

  /* ---------------- 启动 ---------------- */
  PANELS.forEach(setupPanel);
  restore();
  initSplits();
  initTheme();
})();
