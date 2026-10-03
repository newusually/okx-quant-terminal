/* ================================================================
 * tabsdock.js —— 二十二期：下方表格区「选项卡自由拖动 + 拖出成独立面板」
 *
 * 用户口径：
 *   「当前持仓面板要做至少5行长度 不能太短
 *     面板上比如历史持仓 当前持仓这些所有面板的选项卡可以自由换位置移动
 *     也可以单独领出来插入到页面其他任意位置」
 *
 * 两个能力：
 *   ① 选项卡在标签条内**拖动换位**（横向拖 >8px 生效，顺序存 localStorage）
 *   ② 选项卡**向下/向任意方向拖出 >70px** → 整块内容脱离 .tables，
 *      变成一个可拖动、可缩放的浮动面板，出现在页面任意位置；
 *      标签条上的按钮置灰保留（点它 = 把面板叫回来），面板头部也有「停靠回去」。
 *
 * 实现纪律：
 *   · 复用既有 DOM：拖出只是把 .tab-body 节点 appendChild 进浮动面板 ——
 *     appendChild 会保留节点上的全部事件绑定，pager / 过滤按钮零重接。
 *   · 与 panels.js 解耦：这里自带一份 ~40 行的迷你拖拽（面板就 6 个，
 *     不值得抽象出共享层）。
 *   · 权益曲线（pnl）的图表在节点移动后必须手动 applyOptions 对一次尺寸，
 *     否则画布还是旧宽。
 * ================================================================ */

(function () {
  'use strict';

  var LS_KEY = 'okxTabsDock:v1';
  var DRAG_THRESHOLD = 8;      // 横向换位的触发距离
  var DETACH_DISTANCE = 70;    // 拖出成面板的触发距离（相对标签条）

  function $(id) { return document.getElementById(id); }

  var state = {
    order: [],      // tab 顺序（data-tab 名单）
    detached: {},   // name -> {x,y,w,h} 浮动面板位置
  };

  function save() {
    try { localStorage.setItem(LS_KEY, JSON.stringify(state)); } catch (e) {}
  }
  function load() {
    try {
      var d = JSON.parse(localStorage.getItem(LS_KEY) || 'null');
      if (d && Array.isArray(d.order)) state.order = d.order;
      if (d && d.detached) state.detached = d.detached;
    } catch (e) {}
  }

  function tabBtn(name) {
    return document.querySelector('#tabs .tab[data-tab="' + name + '"]');
  }
  function tabBody(name) {
    return $('tab' + name.charAt(0).toUpperCase() + name.slice(1));
  }
  function allNames() {
    return [].map.call(document.querySelectorAll('#tabs .tab'), function (b) { return b.dataset.tab; });
  }

  /* ---------------- 顺序 ---------------- */
  function applyOrder() {
    var bar = $('tabs');
    if (!bar) return;
    // 按 state.order 依次重排（不在名单里的排后面），稳定且幂等
    var names = allNames();
    names.forEach(function (n) {
      if (state.order.indexOf(n) < 0) state.order.push(n);
    });
    state.order = state.order.filter(function (n) { return names.indexOf(n) >= 0; });
    state.order.forEach(function (n) {
      var b = tabBtn(n);
      if (b) bar.appendChild(b);   // appendChild 自带「挪到末尾」语义
    });
  }

  /* ---------------- 浮动面板（拖出的选项卡）---------------- */
  function detachPanel(name) {
    var body = tabBody(name);
    var btn = tabBtn(name);
    if (!body || !btn || body.dataset.detached === '1') return;

    var pos = state.detached[name] || {};
    var w = pos.w || Math.max(560, Math.min(900, Math.round(window.innerWidth * 0.5)));
    var h = pos.h || Math.max(260, Math.min(480, Math.round(window.innerHeight * 0.4)));
    var x = (typeof pos.x === 'number') ? pos.x : Math.round(window.innerWidth * 0.25);
    var y = (typeof pos.y === 'number') ? pos.y : 140;

    var panel = document.createElement('section');
    panel.className = 'panel floating tabdock';
    panel.dataset.dockTab = name;
    panel.style.left = x + 'px';
    panel.style.top = y + 'px';
    panel.style.width = w + 'px';
    panel.style.height = h + 'px';

    var label = btn.textContent.replace(/\s*\d+$/, '').trim() || name;
    panel.innerHTML =
      '<div class="panel-head td-head">' +
      '  <span class="td-grip" title="按住拖动 · 双击复位大小">⠿ ' + label + '</span>' +
      '  <span class="td-acts">' +
      '    <button class="ph-act" data-act="redock" title="放回下方表格区">停靠回去</button>' +
      '  </span>' +
      '</div>';

    panel.appendChild(body);            // ★ 整块内容搬进去（事件全保留）
    body.classList.remove('hidden');
    body.classList.add('td-body');
    document.body.appendChild(panel);

    btn.classList.add('detached');
    btn.title = '已拖出为独立面板 · 点这里把面板叫到最前';
    body.dataset.detached = '1';

    bindPanelDrag(panel, name);
    panel.querySelector('[data-act="redock"]').addEventListener('click', function () {
      redock(name);
    });

    // 权益曲线的图表挪窝后必须手动对尺寸
    if (name === 'pnl' && state.pnlChart) {
      setTimeout(function () {
        var el = $('pnlChart');
        if (el) state.pnlChart.applyOptions({ width: el.clientWidth, height: el.clientHeight });
      }, 60);
    }
  }

  function redock(name) {
    var body = tabBody(name);
    var btn = tabBtn(name);
    var panel = document.querySelector('.tabdock[data-dock-tab="' + name + '"]');
    if (!body || !btn || !panel) return;

    // 记住它原来在 .tables 里的位置：tab-body 们按 tabs 顺序排，
    // 挪回来时插到「下一个还在表格区的 tab-body」前面，没有就追加到末尾。
    var tables = document.querySelector('.tables');
    var next = null;
    var idx = state.order.indexOf(name);
    for (var i = idx + 1; i < state.order.length; i++) {
      var b = tabBody(state.order[i]);
      if (b && b.dataset.detached !== '1' && b.parentElement === tables) { next = b; break; }
    }
    body.classList.remove('td-body');
    body.dataset.detached = '';
    if (next) tables.insertBefore(body, next); else tables.appendChild(body);

    panel.remove();
    btn.classList.remove('detached');
    btn.title = '';
    delete state.detached[name];
    save();

    // 如果表格区现在没有激活的 tab（被拖走的那个可能正是激活的），补一个
    var active = document.querySelector('#tabs .tab.active:not(.detached)');
    if (!active) {
      var first = document.querySelector('#tabs .tab:not(.detached)');
      if (first) first.click();
    }
    if (name === 'pnl' && state.pnlChart) { /* 回去后由 tables.js 的轮询对尺寸 */ }
  }

  function focusDetached(name) {
    var panel = document.querySelector('.tabdock[data-dock-tab="' + name + '"]');
    if (!panel) return;
    panel.style.zIndex = 70;   // 抬到其它浮动面板之上
    setTimeout(function () { panel.style.zIndex = ''; }, 400);
  }

  /* ---------------- 迷你拖拽（浮动面板用）---------------- */
  function bindPanelDrag(panel, name) {
    var grip = panel.querySelector('.td-grip');
    if (!grip) return;
    grip.addEventListener('pointerdown', function (e) {
      if (e.target.closest('.ph-act')) return;
      e.preventDefault();
      var r = panel.getBoundingClientRect();
      var ox = e.clientX - r.left, oy = e.clientY - r.top;
      var onMove = function (ev) {
        var x = Math.max(4, Math.min(ev.clientX - ox, window.innerWidth - 80));
        var y = Math.max(4, Math.min(ev.clientY - oy, window.innerHeight - 48));
        panel.style.left = x + 'px';
        panel.style.top = y + 'px';
      };
      var onUp = function () {
        document.removeEventListener('pointermove', onMove);
        document.removeEventListener('pointerup', onUp);
        var r2 = panel.getBoundingClientRect();
        state.detached[name] = {
          x: Math.round(r2.left), y: Math.round(r2.top),
          w: Math.round(r2.width), h: Math.round(r2.height),
        };
        save();
      };
      document.addEventListener('pointermove', onMove);
      document.addEventListener('pointerup', onUp);
    });
    grip.addEventListener('dblclick', function () {
      panel.style.width = '';
      panel.style.height = '';
      var r = panel.getBoundingClientRect();
      state.detached[name].w = Math.round(r.width);
      state.detached[name].h = Math.round(r.height);
      save();
    });

    // ★ 八向缩放（与 panels.js 同一套 CSS 手柄，这里精简为右下 + 右 + 下）
    ['se', 'e', 's'].forEach(function (dir) {
      var h = document.createElement('span');
      h.className = 'ph-rs ph-rs-' + dir;
      panel.appendChild(h);
      h.addEventListener('pointerdown', function (e) {
        e.preventDefault(); e.stopPropagation();
        var r = panel.getBoundingClientRect();
        var sx = e.clientX, sy = e.clientY, sw = r.width, sh = r.height;
        var onMove = function (ev) {
          if (dir.indexOf('e') >= 0) panel.style.width = Math.max(320, sw + ev.clientX - sx) + 'px';
          if (dir.indexOf('s') >= 0) panel.style.height = Math.max(180, sh + ev.clientY - sy) + 'px';
        };
        var onUp = function () {
          document.removeEventListener('pointermove', onMove);
          document.removeEventListener('pointerup', onUp);
          var r2 = panel.getBoundingClientRect();
          state.detached[name].w = Math.round(r2.width);
          state.detached[name].h = Math.round(r2.height);
          save();
          if (name === 'pnl' && state.pnlChart) {
            var el = $('pnlChart');
            if (el) state.pnlChart.applyOptions({ width: el.clientWidth, height: el.clientHeight });
          }
        };
        document.addEventListener('pointermove', onMove);
        document.addEventListener('pointerup', onUp);
      });
    });
  }

  /* ---------------- 标签条：拖动换位 + 拖出 ---------------- */
  function initTabDrag() {
    var bar = $('tabs');
    if (!bar) return;
    var drag = null;

    bar.addEventListener('pointerdown', function (e) {
      var btn = e.target.closest('.tab');
      if (!btn || e.button !== 0) return;
      var r = bar.getBoundingClientRect();
      drag = {
        btn: btn, name: btn.dataset.tab,
        sx: e.clientX, sy: e.clientY,
        barTop: r.top, barBottom: r.bottom,
        moved: false, detached: false, ghost: null,
      };
    });

    document.addEventListener('pointermove', function (e) {
      if (!drag) return;
      var dx = e.clientX - drag.sx, dy = e.clientY - drag.sy;
      if (!drag.moved && Math.abs(dx) < DRAG_THRESHOLD && Math.abs(dy) < DRAG_THRESHOLD) return;
      drag.moved = true;

      // ---- 拖出判定：离标签条垂直距离 > 70px ----
      var away = (e.clientY < drag.barTop - DETACH_DISTANCE) ||
                 (e.clientY > drag.barBottom + DETACH_DISTANCE);
      if (away && !drag.detached) {
        drag.detached = true;
        detachPanel(drag.name);
        // 拖出的那一刻表格区要补一个激活 tab
        var first = document.querySelector('#tabs .tab:not(.detached)');
        if (first && !first.classList.contains('active')) first.click();
        return;
      }

      // ---- 未拖出：横向换位（实时重排）----
      if (!drag.detached) {
        var over = document.elementFromPoint(e.clientX, e.clientY);
        var target = over && over.closest ? over.closest('#tabs .tab:not(.detached)') : null;
        if (target && target !== drag.btn) {
          var rT = target.getBoundingClientRect();
          // 指针在目标左半 → 插到它前面；右半 → 后面
          var before = e.clientX < rT.left + rT.width / 2;
          if (before) target.parentNode.insertBefore(drag.btn, target);
          else target.parentNode.insertBefore(drag.btn, target.nextSibling);
        }
      }
    });

    document.addEventListener('pointerup', function () {
      if (!drag) return;
      if (drag.moved && !drag.detached) {
        // 换位落定：写回顺序
        state.order = allNames();
        save();
        // 拖完不触发 click 切 tab：拦下一次 click
        var b = drag.btn;
        var kill = function (ev) { ev.stopPropagation(); ev.preventDefault(); b.removeEventListener('click', kill, true); };
        b.addEventListener('click', kill, true);
        setTimeout(function () { b.removeEventListener('click', kill, true); }, 250);
      }
      drag = null;
    }, true);

    // 已拖出的 tab：点按钮 = 把面板叫到最前
    bar.addEventListener('click', function (e) {
      var btn = e.target.closest('.tab.detached');
      if (!btn) return;
      e.stopPropagation();
      focusDetached(btn.dataset.tab);
    }, true);
  }

  /* ---------------- 启动 ---------------- */
  load();
  applyOrder();
  initTabDrag();
  // 恢复上次拖出去的面板（延迟一拍：等 tables.js 先把内容绑好）
  setTimeout(function () {
    Object.keys(state.detached).forEach(function (n) {
      if (tabBtn(n) && tabBody(n)) detachPanel(n);
    });
    // 激活的 tab 若被拖走了，补激活第一个还在表格区的
    var active = document.querySelector('#tabs .tab.active:not(.detached)');
    if (!active) {
      var first = document.querySelector('#tabs .tab:not(.detached)');
      if (first) first.click();
    }
  }, 400);
})();
