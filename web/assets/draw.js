/* ================================================================
 * draw.js —— 画图层：OKX 式画图工具（趋势线/水平线/矩形/画笔/橡皮擦）
 * ================================================================ */

/* ================================================================== */
/* ★ 十一期：OKX 式画图工具（趋势线 / 水平线 / 矩形 / 画笔 / 橡皮擦）    */
/* ================================================================== */
/*
 * 用户原话：「优化K线图 要求有OKX的可以画线 画图 还有可以擦除的功能」。
 *
 * ── 为什么用一块**独立叠加 canvas**，而不是往 lightweight-charts 里塞 series ──
 *   画图要的是「自由落笔」，series 只能画「时间 → 值」的序列，一根 K 线一个点，
 *   画矩形 / 画笔都得硬凑，而且会和指标抢图例。独立 canvas 想画什么画什么。
 *
 * ── 最容易踩的坑：鼠标事件归谁 ──
 *   叠加层盖在图表上面。要是它一直 pointer-events:auto，
 *   用户就没法滚轮缩放、拖动平移了 —— 图直接废掉。
 *   所以：**只有选中某个工具时才把 pointer-events 打开**（CSS #drawCanvas.active），
 *   平时 none，事件全部穿透到图表。
 *
 * ── 坐标系：画痕存「时间 + 价格」，不存像素 ──
 *   存像素的话，用户一缩放画痕就错位；存「逻辑索引 + 价格」的话，
 *   向左翻页会把索引整体推移，画痕照样漂移。
 *   所以统一存 (time, price)，显示时再换算成像素 ——
 *   换算是**每次重绘现算**的，数据怎么翻页、怎么缩放都不会漂。
 *   时间超出已加载数据范围（比如线拖到最右边空白区）时按周期外推，
 *   不然 lightweight-charts 的 timeToCoordinate 会返回 null，线就断了一截。
 */

const DRAW_TOOLS = ['trend', 'hline', 'rect', 'brush', 'erase'];
const DRAW_HIT_PX = 9;      // 橡皮擦的判定半径（逻辑像素）
const DRAW_BRUSH_MIN_PX = 3; // 画笔采样的最小间距，太密存起来没意义

// —— 时间 ↔ 像素（经逻辑索引中转，支持数据范围外的时间）——
function drawLogicalToTs(logical) {
  const ks = state.klines;
  const n = ks.length;
  if (!n) return null;
  const i = Math.round(logical);
  if (i >= 0 && i < n) return ks[i].ts;
  const barMs = BAR_MS[state.curBar] || 900000;
  return (i < 0) ? ks[0].ts + i * barMs : ks[n - 1].ts + (i - (n - 1)) * barMs;
}
function drawTsToLogical(ts) {
  const ks = state.klines;
  if (!ks.length) return 0;
  const barMs = BAR_MS[state.curBar] || 900000;
  return (ts - ks[0].ts) / barMs;
}
function drawXToTime(x) {
  const logical = state.chart.timeScale().coordinateToLogical(x);
  return (logical === null) ? null : drawLogicalToTs(logical);
}
function drawTimeToX(ts) {
  if (ts === null || ts === undefined) return null;
  return state.chart.timeScale().logicalToCoordinate(drawTsToLogical(ts));
}
function drawYToPrice(y) { return state.candle.coordinateToPrice(y); }
function drawPriceToY(p) { return state.candle.priceToCoordinate(p); }

// —— 存取（按 合约+周期 隔离，刷新页面还在）——
function drawKey() { return 'okxDraw:' + (state.curInst || '?') + ':' + (state.curBar || '?'); }
function drawLoad() {
  state.draw.key = drawKey();
  try {
    const raw = localStorage.getItem(state.draw.key);
    state.draw.items = raw ? JSON.parse(raw) : [];
    if (!Array.isArray(state.draw.items)) state.draw.items = [];
  } catch (_) { state.draw.items = []; }
}
function drawSave() {
  try {
    // 画笔点可能很多，超 64KB 就丢最老的几条 —— 不然 localStorage 会满
    let raw = JSON.stringify(state.draw.items);
    while (raw.length > 65536 && state.draw.items.length) {
      state.draw.items.shift();
      raw = JSON.stringify(state.draw.items);
    }
    localStorage.setItem(drawKey(), raw);
  } catch (_) { /* 隐私模式 / 配额满：画痕只在本次会话内有效，不打断用户 */ }
}

// —— 画痕的像素化 ——
// 返回一组折线（每条是 [x,y] 数组），矩形会变成 4 条边、水平线 1 条贯穿线。
function drawItemToPolylines(it) {
  const w = state.draw.canvas.clientWidth, h = state.draw.canvas.clientHeight;
  const pts = (it.pts || []).map((p) => {
    const x = drawTimeToX(p.t), y = drawPriceToY(p.p);
    return (x === null || y === null) ? null : [x, y];
  });
  if (pts.some((p) => p === null)) return null;   // 有一个点算不出来就整条先不画

  if (it.type === 'hline') {
    const y = pts[0][1];
    return [[[0, y], [w, y]]];
  }
  if (it.type === 'rect') {
    const [a, b] = pts;
    return [[
      [a[0], a[1]], [b[0], a[1]], [b[0], b[1]], [a[0], b[1]], [a[0], a[1]],
    ]];
  }
  return [pts.filter(Boolean)];
}

function drawRender() {
  const cvs = state.draw.canvas, ctx = state.draw.ctx;
  if (!cvs || !ctx) return;
  const dpr = window.devicePixelRatio || 1;
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, cvs.clientWidth, cvs.clientHeight);

  const all = state.draw.drawing ? state.draw.items.concat([state.draw.drawing]) : state.draw.items;
  all.forEach((it) => {
    const polys = drawItemToPolylines(it);
    if (!polys) return;
    ctx.strokeStyle = it.color || state.draw.color;
    ctx.lineWidth = it.type === 'brush' ? 2 : 1.6;
    ctx.lineJoin = 'round';
    ctx.lineCap = 'round';
    // 矩形填充一点淡淡的同色，看得出「这是一块区域」
    if (it.type === 'rect' && polys[0].length === 5) {
      const [a, b] = it.pts.map((p) => [drawTimeToX(p.t), drawPriceToY(p.p)]);
      ctx.fillStyle = (it.color || state.draw.color) + '22';
      ctx.fillRect(Math.min(a[0], b[0]), Math.min(a[1], b[1]),
        Math.abs(b[0] - a[0]), Math.abs(b[1] - a[1]));
    }
    polys.forEach((poly) => {
      if (poly.length < 2) return;
      ctx.beginPath();
      ctx.moveTo(poly[0][0], poly[0][1]);
      for (let i = 1; i < poly.length; i++) ctx.lineTo(poly[i][0], poly[i][1]);
      ctx.stroke();
    });
  });
}

// —— 橡皮擦：点中哪条删哪条 ——
function distToSeg(px, py, x1, y1, x2, y2) {
  const dx = x2 - x1, dy = y2 - y1;
  const len2 = dx * dx + dy * dy;
  let t = len2 ? ((px - x1) * dx + (py - y1) * dy) / len2 : 0;
  t = Math.max(0, Math.min(1, t));
  const cx = x1 + t * dx, cy = y1 + t * dy;
  return Math.hypot(px - cx, py - cy);
}
function drawEraseAt(x, y) {
  // 从最新往回找：重叠时优先删最近画的那条，符合直觉
  for (let i = state.draw.items.length - 1; i >= 0; i--) {
    const polys = drawItemToPolylines(state.draw.items[i]);
    if (!polys) continue;
    for (const poly of polys) {
      for (let k = 1; k < poly.length; k++) {
        if (distToSeg(x, y, poly[k - 1][0], poly[k - 1][1], poly[k][0], poly[k][1]) <= DRAW_HIT_PX) {
          state.draw.items.splice(i, 1);
          drawSave();
          drawRender();
          return true;
        }
      }
    }
  }
  return false;
}

// —— 事件（只在选中工具时才接管鼠标）——
function drawPos(e) {
  const r = state.draw.canvas.getBoundingClientRect();
  return [e.clientX - r.left, e.clientY - r.top];
}

function initDrawTools() {
  const cvs = $('drawCanvas');
  const box = document.querySelector('.chart-box');
  if (!cvs || !box || !cvs.getContext) return;
  state.draw.canvas = cvs;
  state.draw.ctx = cvs.getContext('2d');

  // 尺寸跟容器走（DPR 对齐，线条才不糊）
  const fit = () => {
    const r = box.getBoundingClientRect();
    const dpr = window.devicePixelRatio || 1;
    cvs.width = Math.max(1, Math.round(r.width * dpr));
    cvs.height = Math.max(1, Math.round(r.height * dpr));
    drawRender();
  };
  fit();
  window.addEventListener('resize', fit);
  new ResizeObserver(fit).observe(box);

  // 图表一缩放/平移就重算像素（画痕存的是时间+价格，必须跟着换算）。
  // 顺带重算标记密度：放大之后空间够了，文字标签要**自动长回来**；
  // 缩小之后挤了，又要自动收起来。只靠 renderKline 时算一次是不够的 ——
  // 用户一滚轮，密度就变了。
  state.chart.timeScale().subscribeVisibleLogicalRangeChange(() => {
    drawRender();
    paintMarkers();
  });

  const setTool = (tool) => {
    state.draw.tool = (state.draw.tool === tool) ? null : tool;
    state.draw.drawing = null;
    document.querySelectorAll('.draw-tool[data-tool]').forEach((b) =>
      b.classList.toggle('on', b.dataset.tool === state.draw.tool));
    cvs.classList.toggle('active', !!state.draw.tool);
    cvs.classList.toggle('erasing', state.draw.tool === 'erase');
    drawRender();
  };
  document.querySelectorAll('.draw-tool[data-tool]').forEach((b) => {
    b.addEventListener('click', () => setTool(b.dataset.tool));
  });

  const toggle = $('drawToggle');
  toggle.addEventListener('click', () => {
    const bar = $('drawBar');
    bar.classList.toggle('hidden');
    toggle.classList.toggle('on', !bar.classList.contains('hidden'));
  });

  document.querySelectorAll('.draw-c').forEach((b) => {
    b.addEventListener('click', () => {
      state.draw.color = b.dataset.c;
      document.querySelectorAll('.draw-c').forEach((x) =>
        x.classList.toggle('on', x === b));
    });
  });
  // 默认黄色
  const first = document.querySelector('.draw-c');
  if (first) { state.draw.color = first.dataset.c; first.classList.add('on'); }

  $('drawUndo').addEventListener('click', () => {
    state.draw.items.pop();
    drawSave(); drawRender();
  });
  $('drawClear').addEventListener('click', () => {
    if (!state.draw.items.length) return;
    if (!confirm('清空当前合约当前周期的所有画痕？')) return;
    state.draw.items = [];
    drawSave(); drawRender();
  });

  // —— 画布上的鼠标（选中工具才收事件；CSS 已把 pointer-events 打开）——
  cvs.addEventListener('mousedown', (e) => {
    if (!state.draw.tool) return;
    e.preventDefault(); e.stopPropagation();
    if (e.button !== 0) return;
    const [x, y] = drawPos(e);

    if (state.draw.tool === 'erase') {
      drawEraseAt(x, y);
      return;
    }
    const t = drawXToTime(x), p = drawYToPrice(y);
    if (t === null || p === null || !isFinite(p)) return;

    state.draw.drag = true;
    if (state.draw.tool === 'hline') {
      // 水平线点一下即成，不用拖
      state.draw.items.push({ type: 'hline', color: state.draw.color, pts: [{ t, p }] });
      drawSave(); drawRender();
      state.draw.drag = false;
      return;
    }
    state.draw.drawing = { type: state.draw.tool, color: state.draw.color, pts: [{ t, p }] };
    if (state.draw.tool === 'brush') state.draw.lastMove = [x, y];
    drawRender();
  });

  cvs.addEventListener('mousemove', (e) => {
    if (!state.draw.tool) return;
    // 画图模式下别让鼠标事件冒泡到 .chart-box —— 那里绑着七期的
    // 「600px 魔法棒跟随」和「点一下爆星星」，画线时满屏星星会把画痕盖住。
    e.stopPropagation();
    if (!state.draw.drag || !state.draw.drawing) return;
    e.preventDefault();
    const [x, y] = drawPos(e);
    const t = drawXToTime(x), p = drawYToPrice(y);
    if (t === null || p === null || !isFinite(p)) return;

    if (state.draw.tool === 'brush') {
      // 画笔按像素距离采样：太密除了拖慢保存没别的用处
      const lm = state.draw.lastMove;
      if (lm && Math.hypot(x - lm[0], y - lm[1]) < DRAW_BRUSH_MIN_PX) return;
      state.draw.lastMove = [x, y];
      state.draw.drawing.pts.push({ t, p });
    } else if (state.draw.tool === 'trend' || state.draw.tool === 'rect') {
      state.draw.drawing.pts[1] = { t, p };
    }
    drawRender();
  });

  const finish = (e) => {
    if (!state.draw.drag) return;
    if (e) { e.preventDefault(); e.stopPropagation(); }
    state.draw.drag = false;
    state.draw.lastMove = null;
    const d = state.draw.drawing;
    state.draw.drawing = null;
    if (!d) { drawRender(); return; }
    // 只点了一下没拖（趋势线/矩形）→ 没有形状，丢弃，不当成垃圾存起来
    if (d.pts.length < 2 && d.type !== 'hline') { drawRender(); return; }
    if (d.type === 'trend' && d.pts[1] &&
        Math.abs(drawTimeToX(d.pts[1].t) - drawTimeToX(d.pts[0].t)) < 3 &&
        Math.abs(drawPriceToY(d.pts[1].p) - drawPriceToY(d.pts[0].p)) < 3) {
      drawRender(); return;
    }
    state.draw.items.push(d);
    drawSave();
    drawRender();
  };
  cvs.addEventListener('mouseup', finish);
  cvs.addEventListener('mouseleave', finish);

  // 画图模式下别让点击冒泡到 .chart-box —— 那里绑着七期的「点一下爆星星」，
  // 画一条线爆十颗星星会把画痕盖住，用户会以为图坏了。
  cvs.addEventListener('click', (e) => {
    if (state.draw.tool) { e.preventDefault(); e.stopPropagation(); }
  });

  // 换合约 / 换周期 → 换一套画痕（localStorage 按键隔离）
  drawLoad();
  drawRender();
}

// drawOnInstChange 换合约 / 换周期时由 renderKline 调用：
// 键变了就重读那一套画痕，然后重绘。
function drawOnInstChange() {
  if (!state.draw.canvas) return;
  if (drawKey() !== state.draw.key) drawLoad();
  drawRender();
}

// loadOlder 向左翻一页：拿 state.klines 第一根之前的那 KLINE_PAGE 根
async function loadOlder() {
  if (state.loadingOlder || !state.hasMore || !state.curInst) return;
  if (!state.klines.length) return;
  state.loadingOlder = true;
  const inst = state.curInst, bar = state.curBar;
  const before = state.klines[0].ts;
  $('chartHint').textContent = `加载更早的 ${KLINE_PAGE} 根…`;
  try {
    const j = await api(`/api/mark?inst=${encodeURIComponent(inst)}&bar=${encodeURIComponent(bar)}` +
      `&limit=${KLINE_PAGE}&before=${before}&_=${Date.now()}`);
    if (inst !== state.curInst || bar !== state.curBar) return;   // 期间切了合约，丢弃
    mergePage(j, false);
    const ts = (state.insts.find((x) => x.instId === inst) || {}).tickSz || 0.0001;
    renderKline(ts, true);
    updatePageHint();
  } catch (e) {
    $('chartHint').textContent = '加载更早数据失败：' + e.message;
  } finally {
    state.loadingOlder = false;
  }
}

// loadNewer 向右翻一页（七期）：拿 state.klines 最后一根之后的 KLINE_PAGE 根。
//
// 初始只加载最新 100 根，用户往右滚到右边界时向后补页 —— 服务端
// /api/mark 支持 after=<ts>（返回 after 之后**最早**的 300 根，渐进不跳段）。
// 返回 0 根说明库里没有更新的了（右端已到实时），置 state.noMoreNew 停止请求。
async function loadNewer() {
  if (state.loadingNewer || state.noMoreNew || !state.curInst) return;
  if (!state.klines.length) return;
  state.loadingNewer = true;
  const inst = state.curInst, bar = state.curBar;
  const after = state.klines[state.klines.length - 1].ts;
  $('chartHint').textContent = `加载更晚的 ${KLINE_PAGE} 根…`;
  try {
    const j = await api(`/api/mark?inst=${encodeURIComponent(inst)}&bar=${encodeURIComponent(bar)}` +
      `&limit=${KLINE_PAGE}&after=${after}&_=${Date.now()}`);
    if (inst !== state.curInst || bar !== state.curBar) return;   // 期间切了合约，丢弃
    if (!j.kline || !j.kline.length) {
      state.noMoreNew = true;   // 后面没有更新的 K 线了
    } else {
      mergePage(j, false);
      const ts = (state.insts.find((x) => x.instId === inst) || {}).tickSz || 0.0001;
      renderKline(ts, true);
    }
    updatePageHint();
  } catch (e) {
    $('chartHint').textContent = '加载更晚数据失败：' + e.message;
  } finally {
    state.loadingNewer = false;
  }
}

