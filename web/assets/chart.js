/* ================================================================
 * chart.js —— 图表层：图表初始化 / 图例拖动 / 十字提示 / 信号标注 / 权益曲线
 * ================================================================ */
/* ------------------------------------------------------------------ */
/* 图表初始化                                                          */
/* ------------------------------------------------------------------ */

function initChart() {
  const el = $('chart');
  state.chart = LightweightCharts.createChart(el, {
    layout: {
      background: { type: 'solid', color: '#0b0e11' },
      textColor: '#b7bdc6',
      fontSize: 11,
      fontFamily: 'Menlo, Consolas, monospace',
      attributionLogo: false,
    },
    // 网格压到最暗，K 线才跳得出来（原来 #181d23 太抢眼）
    grid: {
      vertLines: { color: '#14181d' },
      horzLines: { color: '#14181d' },
    },
    rightPriceScale: {
      borderColor: '#262b31',
      borderVisible: true,
      scaleMargins: { top: 0.08, bottom: 0.26 },
      entireTextOnly: true,
    },
    timeScale: {
      borderColor: '#262b31',
      timeVisible: true,
      secondsVisible: false,
      rightOffset: 8,
      barSpacing: 7,
      minBarSpacing: 0.4,
    },
    crosshair: {
      mode: LightweightCharts.CrosshairMode.Normal,
      vertLine: { color: '#4a515c', width: 1, style: LightweightCharts.LineStyle.Dashed, labelBackgroundColor: '#2b3139' },
      horzLine: { color: '#4a515c', width: 1, style: LightweightCharts.LineStyle.Dashed, labelBackgroundColor: '#2b3139' },
    },
    watermark: {
      visible: false,   // 图例已经写了合约名，水印只会显得脏
      text: '', color: '#1e2329', fontSize: 48, horzAlign: 'center', vertAlign: 'center',
    },
    localization: {
      locale: 'zh-CN',
      timeFormatter: (t) => {
        const d = new Date(t * 1000);
        const p = (n) => String(n).padStart(2, '0');
        return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
      },
    },
    handleScroll: true,
    handleScale: true,
  });

  // 币安口径：涨绿跌红
  state.candle = state.chart.addCandlestickSeries({
    upColor: C_UP, downColor: C_DOWN,
    borderUpColor: C_UP, borderDownColor: C_DOWN,
    wickUpColor: C_UP, wickDownColor: C_DOWN,
    priceLineVisible: true, lastValueVisible: true,
    priceLineColor: '#848e9c', priceLineStyle: LightweightCharts.LineStyle.Dotted,
  });

  state.volume = state.chart.addHistogramSeries({
    priceScaleId: 'vol',
    priceFormat: { type: 'volume' },
    lastValueVisible: false, priceLineVisible: false,
  });
  state.chart.priceScale('vol').applyOptions({ scaleMargins: { top: 0.78, bottom: 0 } });

  state.ma7 = state.chart.addLineSeries({ color: C_MA7, lineWidth: 1, priceLineVisible: false, lastValueVisible: false, crosshairMarkerVisible: false });
  state.ma25 = state.chart.addLineSeries({ color: C_MA25, lineWidth: 1, priceLineVisible: false, lastValueVisible: false, crosshairMarkerVisible: false });
  state.ma99 = state.chart.addLineSeries({ color: C_MA99, lineWidth: 2, priceLineVisible: false, lastValueVisible: false, crosshairMarkerVisible: false });

  // 布林带：上下轨虚线、中轨实线（之前取了数据却从来没画出来）
  const bollOpts = { lineWidth: 1, priceLineVisible: false, lastValueVisible: false, crosshairMarkerVisible: false, lineStyle: LightweightCharts.LineStyle.Dashed };
  state.bollUp = state.chart.addLineSeries({ ...bollOpts, color: C_BOLL });
  state.bollLo = state.chart.addLineSeries({ ...bollOpts, color: C_BOLL });
  state.bollMid = state.chart.addLineSeries({ ...bollOpts, color: C_BOLL_MID, lineStyle: LightweightCharts.LineStyle.Solid });

  new ResizeObserver(() => {
    state.chart.applyOptions({ width: el.clientWidth, height: el.clientHeight });
  }).observe(el);
  state.chart.applyOptions({ width: el.clientWidth, height: el.clientHeight });

  // syncChartSize 手动把图表尺寸对齐到容器。
  //
  // ★ 为什么不依赖 ResizeObserver 就够了：观察的是 #chart 这个元素，
  //   但「切合约」这个动作本身不会改变它的尺寸 —— 改变的是**它的父级**
  //   （.chart-head 换行数变多 → .chart-box 变矮）。
  //   ResizeObserver 对祖先尺寸变化也会触发，但时序上可能在
  //   setData 之后才回调，那一帧用户看到的还是旧尺寸 → 图看着缺一块。
  //   所以切合约时**显式**对一次尺寸，不等观察器。
  window.__syncChartSize = () => {
    state.chart.applyOptions({ width: el.clientWidth, height: el.clientHeight });
  };

  // ★ 十一期：**只读**调试句柄。
  //   没有任何写入口，只是让控制台能回答「现在加载了几根 K 线 /
  //   当前合约是谁 / 画了几条线 / 均价线建了没有」这类问题。
  //   没有它，每次诊断都只能靠截图猜 —— 而截图猜出来的结论经常是错的。
  window.__okx = {
    state,
    chart: () => state.chart,
    syncSize: () => window.__syncChartSize && window.__syncChartSize(),
    markers: () => state.markers.length,
    drawings: () => state.draw.items.length,
    entryLines: () => state.entryLines.length,
  };

  // 十字光标联动：图例 + 悬浮提示框 + 买卖标记气泡
  state.chart.subscribeCrosshairMove((param) => {
    if (!param || param.time === undefined || !param.seriesData) {
      hideTip();
      renderLegend(state.klines[state.klines.length - 1] || null);
      return;
    }
    const c = param.seriesData.get(state.candle);
    const ts = param.time * 1000;
    if (!c) {
      hideTip();
      renderLegend(state.kMap.get(ts) || state.klines[state.klines.length - 1] || null);
      return;
    }
    const k = state.kMap.get(ts) || { ts, o: c.open, h: c.high, l: c.low, c: c.close };
    $('pairOhlc').textContent =
      `开 ${fmtPrice(c.open)}  高 ${fmtPrice(c.high)}  低 ${fmtPrice(c.low)}  收 ${fmtPrice(c.close)}`;
    renderLegend(k);
    showTip(param, k);
  });

  // 向左滚动 → 自动加载更早的 K 线（每次一页 1000 根）
  state.scrollGuardUntil = Date.now() + 3000;   // 首屏渲染期间不触发
  state.chart.timeScale().subscribeVisibleLogicalRangeChange(onScroll);

  // ★ 十一期：画图工具（要在 state.chart 建好之后 —— 换算坐标全靠它）
  initDrawTools();
  // ★ 十一期：图例可拖动（⠿ 手柄按住拖、双击复位、位置存 localStorage）
  initLegendDrag();
}

/* ------------------------------------------------------------------ */
/* 图例（左上角，跟随十字光标实时刷新）                                  */
/* ------------------------------------------------------------------ */

// initLegendDrag：让图例可以拖到任意位置。只有 ⠿ 手柄接收鼠标事件
// （其余 pointer-events:none 穿透给图表），拖动范围钳在 .chart-box 内，
// 位置存 localStorage('okxLegendPos')，双击手柄复位到默认左上角。
function initLegendDrag() {
  const legend = $('legend'), grip = $('lgGrip');
  if (!legend || !grip) return;
  const box = legend.parentElement;                 // .chart-box
  const KEY = 'okxLegendPos';

  // 恢复上次拖动的位置
  try {
    const saved = JSON.parse(localStorage.getItem(KEY) || 'null');
    if (saved && typeof saved.x === 'number' && typeof saved.y === 'number') {
      legend.style.left = saved.x + 'px';
      legend.style.top = saved.y + 'px';
    }
  } catch (e) { /* 坏数据当没存过 */ }

  let drag = null;
  grip.addEventListener('mousedown', (e) => {
    e.preventDefault(); e.stopPropagation();        // 别触发魔法棒/爆星星
    const r = legend.getBoundingClientRect(), b = box.getBoundingClientRect();
    drag = { dx: e.clientX - r.left, dy: e.clientY - r.top, b };
    document.body.classList.add('legend-dragging');
  });
  window.addEventListener('mousemove', (e) => {
    if (!drag) return;
    const lr = legend.getBoundingClientRect(), b = drag.b;
    // 钳在图表框内，拖不丢
    const x = Math.max(0, Math.min(e.clientX - b.left - drag.dx, b.width - lr.width));
    const y = Math.max(0, Math.min(e.clientY - b.top - drag.dy, b.height - lr.height));
    legend.style.left = x + 'px';
    legend.style.top = y + 'px';
  });
  window.addEventListener('mouseup', () => {
    if (!drag) return;
    drag = null;
    document.body.classList.remove('legend-dragging');
    try {
      localStorage.setItem(KEY, JSON.stringify({
        x: parseFloat(legend.style.left) || 0,
        y: parseFloat(legend.style.top) || 0,
      }));
    } catch (e) { /* 存不了就算了，下次再拖 */ }
  });
  grip.addEventListener('dblclick', (e) => {
    e.preventDefault();
    legend.style.left = '';                         // 回到 CSS 默认 12px/20px
    legend.style.top = '';
    try { localStorage.removeItem(KEY); } catch (err) { /* 同上 */ }
  });
}

const lgCls = (v) => (v >= 0 ? 'up' : 'down');

// renderLegend 画左上角的图例。k 为 null 时全部显示 --
function renderLegend(k) {
  const name = (state.tickers[state.curInst] && state.tickers[state.curInst].name) || state.curInst || '--';
  $('lgSym').textContent = name;
  $('lgTf').textContent = state.curBar;
  if (!k) {
    $('lgOhlc').textContent = '';
    ['lgMa7', 'lgMa25', 'lgMa99', 'lgBollUp', 'lgBollMid', 'lgBollLo', 'lgVol'].forEach((id) => { $(id).textContent = '--'; });
    return;
  }
  const chg = k.o ? (k.c - k.o) / k.o * 100 : 0;
  const col = k.c >= k.o ? 'up' : 'down';
  $('lgOhlc').innerHTML =
    `<span class="k">O</span><span class="${col}">${fmtPrice(k.o)}</span> ` +
    `<span class="k">H</span><span class="${col}">${fmtPrice(k.h)}</span> ` +
    `<span class="k">L</span><span class="${col}">${fmtPrice(k.l)}</span> ` +
    `<span class="k">C</span><span class="${col}">${fmtPrice(k.c)}</span> ` +
    `<span class="${lgCls(chg)}">${chg >= 0 ? '+' : ''}${chg.toFixed(2)}%</span>`;

  const m = state.indMap || {};
  const pick = (map, ts) => (map && map.get(ts) !== undefined ? fmtPrice(map.get(ts)) : '--');
  $('lgMa7').textContent = pick(m.ma7, k.ts);
  $('lgMa25').textContent = pick(m.ma25, k.ts);
  $('lgMa99').textContent = pick(m.ma99, k.ts);
  $('lgBollUp').textContent = pick(m.bollUp, k.ts);
  $('lgBollMid').textContent = pick(m.bollMid, k.ts);
  $('lgBollLo').textContent = pick(m.bollLo, k.ts);
  $('lgVol').textContent = fmtVol(k.v);
}

// buildIdx 重建 ts→值 的快查表（图例 O(1) 查指标）
function buildIdx() {
  const mk = (arr) => {
    const m = new Map();
    (arr || []).forEach((p) => m.set(p.ts, p.v));
    return m;
  };
  state.indMap = {
    ma7: mk(state.ind.ma7), ma25: mk(state.ind.ma25), ma99: mk(state.ind.ma99),
    bollUp: mk(state.ind.bollUp), bollMid: mk(state.ind.bollMid), bollLo: mk(state.ind.bollLo),
  };
  state.kMap = new Map();
  state.klines.forEach((k) => state.kMap.set(k.ts, k));
}

// applyIndVisibility 按开关显示/隐藏指标
function applyIndVisibility() {
  const v = state.indVisible;
  const maVis = v.ma, bollVis = v.boll;
  [state.ma7, state.ma25, state.ma99].forEach((s) => s.applyOptions({ visible: maVis }));
  [state.bollUp, state.bollMid, state.bollLo].forEach((s) => s.applyOptions({ visible: bollVis }));
  state.volume.applyOptions({ visible: v.vol });
  $('lgMA').classList.toggle('hidden', !maVis);
  $('lgBOLL').classList.toggle('hidden', !bollVis);
  $('lgVOL').classList.toggle('hidden', !v.vol);
}

// ------------------------------------------------------------------ */
// 悬浮提示框（时间 / 开 / 收 / 高 / 低）
// ------------------------------------------------------------------ */

// showTip 把提示框跟在鼠标旁边。
//
// 边界处理：靠右/靠下时自动翻到另一侧，否则会被图表容器裁掉半个。
function showTip(param, k) {
  const el = $('tip');
  if (!el || !param.point) return;

  const chg = k.o ? (k.c - k.o) / k.o * 100 : 0;
  const col = k.c >= k.o ? 'up' : 'down';
  const amp = k.l ? (k.h - k.l) / k.l * 100 : 0;

  // 这一根上挂了哪些标记（买入 / 加仓 / 平仓 / 信号），一并显示在提示框底部。
  //
  // ★ 2026-10-02 三期：同一根 K 线上的多笔成交已在后端合并成一笔、金额累加
  //   （见 aggregateEvents）。这里把「合并了几笔」也写出来 —— 否则用户会疑惑
  //   「我只成交了 2 次，金额怎么变大了」。单笔时保持原来的简洁写法。
  const amtOf = (v, d) => (m.count > 1
    ? `合计 ${fmtNum(v, d)}U（${m.count} 笔）`
    : `${fmtNum(v, d)}U`);
  const mk = markersAt(k.ts).map((m) => {
    if (m.kind === 'addon') {
      return `<div class="tip-mk mk-addon">➕ 加仓 ${fmtPrice(m.price)} · ${amtOf(m.margin, 3)}${m.leverage ? ' · ' + m.leverage + 'x' : ''}</div>`;
    }
    if (m.kind === 'close') {
      return `<div class="tip-mk mk-sell">🌿 平仓 ${fmtPrice(m.price)} · ${amtOf(m.pnl, 4)}（${fmtPct(m.pnlPct)}）${m.reason ? ' · ' + esc(m.reason) : ''}</div>`;
    }
    if (m.kind === 'open') {
      return `<div class="tip-mk mk-buy">🚀 买入 ${fmtPrice(m.price)} · ${amtOf(m.margin, 3)}${m.leverage ? ' · ' + m.leverage + 'x' : ''}</div>`;
    }
    return `<div class="tip-mk mk-sig">🚀 买入信号 ${m.score}/8${m.hitList ? ' · ' + esc(m.hitList) : ''}</div>`;
  }).join('');

  el.innerHTML =
    `<div class="tip-time">${fmtTime(k.ts)}</div>` +
    `<div class="tip-row"><span>开盘</span><b class="${col}">${fmtPrice(k.o)}</b></div>` +
    `<div class="tip-row"><span>收盘</span><b class="${col}">${fmtPrice(k.c)}</b></div>` +
    `<div class="tip-row"><span>最高</span><b class="${col}">${fmtPrice(k.h)}</b></div>` +
    `<div class="tip-row"><span>最低</span><b class="${col}">${fmtPrice(k.l)}</b></div>` +
    `<div class="tip-row"><span>涨跌</span><b class="${k.c >= k.o ? 'up' : 'down'}">${chg >= 0 ? '+' : ''}${chg.toFixed(2)}%</b></div>` +
    `<div class="tip-row"><span>振幅</span><b>${amp.toFixed(2)}%</b></div>` +
    `<div class="tip-row"><span>成交量</span><b>${fmtVol(k.v)}</b></div>` +
    (mk ? `<div class="tip-mks">${mk}</div>` : '');

  el.classList.remove('hidden');

  // 先亮出来才能量到尺寸
  const host = el.parentElement;
  const bw = host ? host.clientWidth : 800;
  const bh = host ? host.clientHeight : 400;
  const w = el.offsetWidth, h = el.offsetHeight;
  let x = param.point.x + 16;
  let y = param.point.y + 16;
  if (x + w > bw - 8) x = param.point.x - w - 16;
  if (y + h > bh - 8) y = param.point.y - h - 16;
  el.style.left = Math.max(4, Math.min(x, bw - w - 4)) + 'px';
  el.style.top = Math.max(4, Math.min(y, bh - h - 4)) + 'px';
}

function hideTip() {
  const el = $('tip');
  if (el) el.classList.add('hidden');
}

/* ------------------------------------------------------------------ */
/* 买卖标记：🚀 买入信号/开仓   🍃 平仓卖出                              */
/* ------------------------------------------------------------------ */
/* 标记由后端 /api/mark 随 K 线一起返回（只回当前这一页覆盖的时间区间），
   所以往前翻页时历史那段（含回补出来的一个月）也会自动带上标记。    */

// markersAt 取某个时间点上的所有标记（提示框里用）
function markersAt(ts) {
  const sec = Math.floor(ts / 1000);
  return state.markers.filter((m) => m.time === sec);
}

// mergeMarkers 把一页的标记并进来。
// 翻页时同一笔成交会被相邻两页都返回，用 time|kind|id 去重。
function mergeMarkers(list) {
  let added = 0;
  (list || []).forEach((m) => {
    if (!m || !m.time) return;
    const key = `${m.time}|${m.kind}|${m.id === undefined ? '' : m.id}`;
    if (state.markerKey.has(key)) return;
    state.markerKey.add(key);
    state.markers.push(m);
    added++;
  });
  if (added) state.markers.sort((a, b) => a.time - b.time);
  return added;
}

function resetMarkers() {
  state.markers = [];
  state.markerKey = new Set();
  if (state.candle) state.candle.setMarkers([]);
}

// paintMarkers 把标记贴到蜡烛系列上（整幅重绘时调一次）
//
// ★ 十一期：按密度决定要不要带文字 ★
//   一根 K 线只有几个像素宽时（100 根挤在 998px 里 ≈ 9px/根），
//   「买入 2.68U×4」这种 60px 宽的标签必然互相压成一团，
//   把底下的蜡烛全盖住 —— 用户看到的就是「K 线图显示不完全」。
//   所以：**空间够才显示文字，空间不够只留箭头**。
//   每笔的金额 / 张数 / 时间在下方「交易明细」和悬浮框里都能查到，
//   信息没丢，只是不再糊在图上。
function paintMarkers() {
  if (!state.candle) return;
  let sparse = true;
  try {
    const ts = state.chart.timeScale();
    const x0 = ts.logicalToCoordinate(0);
    const x1 = ts.logicalToCoordinate(20);
    // 用「20 根 K 线的像素宽 ÷ 20」算间距，比拿面板宽度除根数更准
    //（面板宽里还含着右侧价格轴，直接除会偏小）
    const spacing = (x0 !== null && x1 !== null) ? Math.abs(x1 - x0) / 20 : 99;
    sparse = spacing < 14;
  } catch (_) { sparse = true; }

  const list = sparse
    ? state.markers.map((m) => Object.assign({}, m, { text: undefined }))
    : state.markers;
  state.candle.setMarkers(list);
}

function initPnlChart() {
  const el = $('pnlChart');
  if (!el) return;
  state.pnlChart = LightweightCharts.createChart(el, {
    layout: { background: { type: 'solid', color: '#161a1e' }, textColor: '#b7bdc6', fontSize: 11 },
    grid: { vertLines: { color: '#1d2228' }, horzLines: { color: '#1d2228' } },
    rightPriceScale: { borderColor: '#262b31' },
    timeScale: { borderColor: '#262b31', timeVisible: true },
    crosshair: {
      vertLine: { color: '#fcd535', width: 1, style: 2, labelBackgroundColor: '#fcd535' },
      horzLine: { color: '#fcd535', width: 1, style: 2, labelBackgroundColor: '#fcd535' },
    },
  });
  state.pnlLine = state.pnlChart.addAreaSeries({
    lineColor: '#fcd535', topColor: 'rgba(252,213,53,.35)', bottomColor: 'rgba(252,213,53,0)',
    lineWidth: 2,
  });
  // 光标移到曲线上任意位置 → 显示那一刻的权益、相对本金的盈亏、浮盈、持仓数
  state.pnlChart.subscribeCrosshairMove((param) => showPnlTip(param));
  new ResizeObserver(() => {
    state.pnlChart.applyOptions({ width: el.clientWidth, height: el.clientHeight });
  }).observe(el);
  state.pnlChart.applyOptions({ width: el.clientWidth, height: el.clientHeight });
}

// showPnlTip 权益曲线的悬浮框。鼠标移出图区就隐藏。
function showPnlTip(param) {
  const el = $('pnlTip');
  const box = $('pnlChart');
  if (!el || !box) return;
  const pts = state.pnlData || [];
  if (!param || !param.time || !param.point || !pts.length) {
    el.classList.add('hidden');
    return;
  }
  // 优先用 seriesData（图表插值后的值），拿不到再按秒级时间戳去原始数据里找
  let eq = null;
  if (param.seriesData && state.pnlLine) {
    const d = param.seriesData.get(state.pnlLine);
    if (d && typeof d.value === 'number') eq = d.value;
  }
  const p = pts.find((x) => Math.floor(x.ts / 1000) === param.time) || null;
  if (eq === null && p) eq = p.totalEq;
  if (eq === null) { el.classList.add('hidden'); return; }

  const principal = (state.account && state.account.principal) || 0;
  const pnl = principal > 0 ? eq - principal : 0;
  const t = p ? p.ts : param.time * 1000;
  el.innerHTML =
    `<div class="tip-time">${fmtTime(t)}</div>` +
    `<div class="tip-row"><span>账户权益</span><b>${fmtNum(eq, 4)} USDT</b></div>` +
    (principal > 0
      ? `<div class="tip-row"><span>相对本金</span><b class="${cls(pnl)}">${pnl >= 0 ? '+' : ''}${fmtNum(pnl, 4)} USDT</b></div>`
      : '') +
    (p
      ? `<div class="tip-row"><span>浮盈</span><b class="${cls(p.upl)}">${p.upl >= 0 ? '+' : ''}${fmtNum(p.upl, 4)}</b></div>` +
        `<div class="tip-row"><span>持仓</span><b>${p.posCount || 0} 个</b></div>` +
        `<div class="tip-row"><span>可用</span><b>${fmtNum(p.avail, 4)}</b></div>`
      : '');

  // 贴边翻转：右侧放不下就移到光标左边
  el.classList.remove('hidden');
  const w = el.offsetWidth, h = el.offsetHeight;
  let x = param.point.x + 16;
  if (x + w > box.clientWidth) x = Math.max(8, param.point.x - w - 16);
  let y = param.point.y + 12;
  if (y + h > box.clientHeight) y = Math.max(8, param.point.y - h - 12);
  el.style.left = x + 'px';
  el.style.top = y + 'px';
}

/* ------------------------------------------------------------------ */
