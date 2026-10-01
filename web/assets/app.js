/* app.js —— 客户端逻辑：AJAX 拉数据 + TradingView 图表 + 实时刷新 */

const $ = (id) => document.getElementById(id);

const state = {
  insts: [],
  tickers: {},          // instId -> ticker
  curInst: '',
  curBar: '15m',
  days: 30,
  // ★ 2026-10-01 二期：1m/3m/5m 重新上线，共 4 个周期（选项卡由 /api/state 的
  //   bars 字段渲染，这里只是首屏兜底 —— 真源是 model.EnabledBars）。
  //   四个周期都参与扫描开仓，K 线只保留最近 10 天（每天凌晨清一次）。
  bars: ['1m', '3m', '5m', '15m'],
  marginText: '',
  scope: 'tradeable',   // tradeable | excluded | all —— 合约列表只看哪种
  universe: null,       // 准入统计 {total,kept,dropped,byReason}
  chart: null, candle: null, volume: null, ma7: null, ma25: null, ma99: null,
  bollUp: null, bollMid: null, bollLo: null,
  pnlChart: null, pnlLine: null,
  pnlData: [],          // 权益曲线的原始点（悬停时查浮盈/持仓数用）
  pnlFitted: false,     // 只在首次铺满视野，之后轮询不打断用户缩放
  positions: [],        // 当前持仓（历史列表里给「持仓中」的行补实时盈亏）
  bfPollTimer: null,
  lastKlineKey: '',

  // ---- K 线分页：初始一页，向左滚动自动向前翻 ----
  klines: [],           // 已加载的 K 线（升序），翻页时不断往前拼
  ind: {},              // 指标序列 {ma7:[], ma25:[], ...}，同样按 ts 合并
  indMap: {},           // ts -> 值 的快查表（画图例用）
  kMap: new Map(),      // ts -> K 线
  hasMore: false,       // 更早还有没有数据
  loadingOlder: false,  // 防止一次滚动触发多次翻页

  // ---- 显示开关 & 实时 ----
  indVisible: { ma: true, boll: true, vol: true },
  tickTimer: null,      // 每秒：收盘倒计时 + 用最新价刷新最后一根

  // ---- K 线标注（买入小火箭 / 卖出小绿叶）----
  markers: [],          // 已加载区间内的标注点（升序）
  markerKey: new Set(), // 去重：翻页时同一笔成交会被两页都返回

  // ---- 账户（顶栏实时数字）----
  account: null,

  // ---- 分页 ----
  // ⚠ 分页现在是**服务端**做的：每次只把当前这一页拉回来，不再整批下载。
  // 每张表各自记住当前页和每页条数，切 tab 回来还是原来那页。
  // 各 tab 的 total 由接口返回（列表里只有当前页，不能用 length 当总数）。
  pgs: {},
  historyRows: [],      // 历史仓位：当前页
  eventRows: [],        // 交易明细：当前页
  signalRows: [],       // 信号：当前页
  bfRows: [],           // 回补：当前页
  evKind: '',           // 交易明细的动作过滤：'' | open | addon | close
};

// 每页条数可选项
const PG_SIZES = [20, 50, 100];

// pgState 取（或初始化）某张表的分页状态
function pgState(key) {
  if (!state.pgs[key]) state.pgs[key] = { page: 1, size: 20 };
  return state.pgs[key];
}

// pgSlice 按当前页切一段出来。
//
// ⚠ 现在的分页是**服务端**做的（/api/history 等直接返回当前页），
// 所以 total 必须由调用方从响应里传进来 —— 不能再用 rows.length 当总数，
// 那只是「当前这一页的条数」，会让分页条永远显示 1/1。
function pgSlice(key, rows, total) {
  const st = pgState(key);
  const t = (typeof total === 'number' && total >= 0) ? total : rows.length;
  const pages = Math.max(1, Math.ceil(t / st.size));
  if (st.page > pages) st.page = pages;
  if (st.page < 1) st.page = 1;
  const start = (st.page - 1) * st.size;
  // 服务端已经切好了页，这里原样返回；只有没传 total 的调用方（纯本地数据）
  // 才在客户端切。
  const slice = (typeof total === 'number') ? rows : rows.slice(start, start + st.size);
  return { slice, start, total: t, pages };
}

// renderPager 渲染分页条。
// key 用来找 #pager<Key>；onGo 是「翻页后重画表格」的回调。
function renderPager(key, total, onGo) {
  const el = $('pager' + key.charAt(0).toUpperCase() + key.slice(1));
  if (!el) return;
  const st = pgState(key);
  const pages = Math.max(1, Math.ceil(total / st.size));
  if (st.page > pages) st.page = pages;
  if (total <= 0) { el.innerHTML = '<span class="pg-total">共 0 条</span>'; return; }

  const from = (st.page - 1) * st.size + 1;
  const to = Math.min(total, st.page * st.size);

  let btns = '';
  const push = (p, label, dis, on) => {
    btns += `<button data-pg="${p}"${dis ? ' disabled' : ''}${on ? ' class="on"' : ''}>${label}</button>`;
  };
  push(1, '«', st.page === 1);
  push(st.page - 1, '‹', st.page === 1);
  const lo = Math.max(1, st.page - 2);
  const hi = Math.min(pages, lo + 4);
  for (let p = lo; p <= hi; p++) push(p, String(p), false, p === st.page);
  push(st.page + 1, '›', st.page === pages);
  push(pages, '»', st.page === pages);

  el.innerHTML =
    `<span class="pg-total">共 <b>${total}</b> 条 · 显示 ${from}-${to} · 第 ${st.page}/${pages} 页</span>` +
    btns +
    `<span class="pg-size">每页 <select data-pgsize="1">` +
      PG_SIZES.map((s) => `<option value="${s}"${s === st.size ? ' selected' : ''}>${s}</option>`).join('') +
    `</select> 条</span>`;

  el.querySelectorAll('button[data-pg]').forEach((b) => {
    b.onclick = () => {
      const p = parseInt(b.getAttribute('data-pg'), 10);
      if (!p || p === st.page) return;
      st.page = p;
      onGo();
    };
  });
  const sel = el.querySelector('select[data-pgsize]');
  if (sel) sel.onchange = () => {
    st.size = parseInt(sel.value, 10) || 20;
    st.page = 1;
    onGo();
  };
}

// 各周期毫秒数（收盘倒计时、实时价能否套用最后一根都靠它）
const BAR_MS = { '1m': 60e3, '3m': 180e3, '5m': 300e3, '15m': 900e3 };

// 币安配色：涨绿跌红
const C_UP = '#0ecb81', C_DOWN = '#f6465d';
const C_MA7 = '#f0b90b', C_MA25 = '#e056fd', C_MA99 = '#4facfe';
const C_BOLL = '#5c6b7a', C_BOLL_MID = '#9aa4b2';

const KLINE_PAGE = 1000;   // 一页多少根（初始加载 & 每次向前翻都这么多）
const PAGE_TRIGGER = 5;    // 可视区左边界落到第几根之前就预加载下一页

/* ------------------------------------------------------------------ */
/* 工具                                                                */
/* ------------------------------------------------------------------ */

const fmtPrice = (v) => {
  if (v === null || v === undefined || isNaN(v)) return '--';
  const a = Math.abs(v);
  if (a === 0) return '0';
  let d = 2;
  if (a < 0.0001) d = 8;
  else if (a < 0.01) d = 6;
  else if (a < 1) d = 5;
  else if (a < 100) d = 4;
  else if (a < 10000) d = 2;
  else d = 1;
  return v.toLocaleString('en-US', { minimumFractionDigits: d, maximumFractionDigits: d });
};

const fmtNum = (v, d = 2) =>
  (v === null || v === undefined || isNaN(v)) ? '--'
    : Number(v).toLocaleString('en-US', { minimumFractionDigits: d, maximumFractionDigits: d });

const fmtVol = (v) => {
  if (!v || isNaN(v)) return '--';
  if (v >= 1e9) return (v / 1e9).toFixed(2) + 'B';
  if (v >= 1e6) return (v / 1e6).toFixed(2) + 'M';
  if (v >= 1e3) return (v / 1e3).toFixed(2) + 'K';
  return v.toFixed(2);
};

const fmtPct = (v, d = 2) =>
  (v === null || v === undefined || isNaN(v)) ? '--' : (v >= 0 ? '+' : '') + Number(v).toFixed(d) + '%';

const cls = (v) => (v > 0 ? 'up' : (v < 0 ? 'down' : 'muted'));

const fmtTime = (ms) => {
  if (!ms) return '--';
  const d = new Date(ms);
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
};

const fmtShort = (ms) => {
  if (!ms) return '--';
  const d = new Date(ms);
  const p = (n) => String(n).padStart(2, '0');
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
};

const esc = (s) => String(s === null || s === undefined ? '' : s)
  .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');

async function api(path, opt) {
  const r = await fetch(path, opt);
  const j = await r.json();
  if (j && j.ok === false) throw new Error(j.error || '接口返回失败');
  return j;
}

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
}

/* ------------------------------------------------------------------ */
/* 图例（左上角，跟随十字光标实时刷新）                                  */
/* ------------------------------------------------------------------ */

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

  // 这一根上挂了哪些标记（买入 / 加仓 / 平仓 / 信号），一并显示在提示框底部
  const mk = markersAt(k.ts).map((m) => {
    if (m.kind === 'addon') {
      return `<div class="tip-mk mk-addon">➕ 加仓 ${fmtPrice(m.price)} · ${fmtNum(m.margin, 3)}U${m.leverage ? ' · ' + m.leverage + 'x' : ''}</div>`;
    }
    if (m.kind === 'close') {
      return `<div class="tip-mk mk-sell">🌿 平仓 ${fmtPrice(m.price)} · ${fmtNum(m.pnl, 4)}U（${fmtPct(m.pnlPct)}）${m.reason ? ' · ' + esc(m.reason) : ''}</div>`;
    }
    if (m.kind === 'open') {
      return `<div class="tip-mk mk-buy">🚀 买入 ${fmtPrice(m.price)} · ${fmtNum(m.margin, 3)}U${m.leverage ? ' · ' + m.leverage + 'x' : ''}</div>`;
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
function paintMarkers() {
  if (!state.candle) return;
  state.candle.setMarkers(state.markers);
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
/* 顶部 / 侧栏                                                         */
/* ------------------------------------------------------------------ */

function renderTimeframes() {
  const box = $('tfGroup');
  box.innerHTML = '';
  state.bars.forEach((b) => {
    const btn = document.createElement('button');
    btn.className = 'tf' + (b === state.curBar ? ' active' : '');
    btn.textContent = b;
    btn.onclick = () => { state.curBar = b; renderTimeframes(); loadKline(true); };
    box.appendChild(btn);
  });
}

function renderInstList() {
  const kw = $('instSearch').value.trim().toUpperCase();
  const box = $('instList');
  const list = state.insts.filter((it) =>
    !kw || it.instId.toUpperCase().includes(kw) || (it.name || '').toUpperCase().includes(kw));
  $('instCount').textContent = list.length + ' / ' + state.insts.length;

  const frag = document.createDocumentFragment();
  list.slice(0, 400).forEach((it) => {
    const t = state.tickers[it.instId] || {};
    const px = t.last || it.last || 0;
    const cg = t.chgPct !== undefined ? t.chgPct : it.chgPct;
    const row = document.createElement('div');
    row.className = 'inst-row' + (it.instId === state.curInst ? ' active' : '');
    row.onclick = () => selectInst(it.instId);
    // 被排除的合约打一个原因标签；可交易的不打（默认列表里全是可交易的）
    const badge = it.tradeable
      ? `<span class="badge-ok">可交易${it.marginUsdt ? ' · ' + fmtNum(it.marginUsdt, 3) + 'U' : ''}</span>`
      : `<span class="badge-no" title="${esc(it.excludeLabel || it.excludeReason)}">${esc(it.excludeLabel || '已排除')}</span>`;
    row.innerHTML = `
      <div class="l">
        <span class="nm">${esc(it.name || it.instId)}</span>
        <span class="sub">${esc(it.base)} · 额 ${fmtVol(it.quoteVol24h)} ${badge}</span>
      </div>
      <div class="r">
        <span class="px">${fmtPrice(px)}</span>
        <span class="cg ${cls(cg)}">${fmtPct(cg)}</span>
      </div>`;
    frag.appendChild(row);
  });
  box.innerHTML = '';
  box.appendChild(frag);
}

// renderUniverse 展示准入过滤的总体结果（多少可交易 / 各自因为什么被排除）
function renderUniverse(u) {
  state.universe = u;
  const el = $('uniSummary');
  if (!u) { el.textContent = ''; return; }
  const why = u.byReason || {};
  // 文案直接取后端给的 labels（保证和落库的原因码一一对应）
  const labels = u.labels || {};
  const parts = Object.keys(why).filter((k) => why[k] > 0)
    .sort((a, b) => why[b] - why[a])
    .map((k) => `${(labels[k] || k).replace(/，.*$/, '')} ${why[k]}`);
  el.textContent = `共 ${u.total} 个合约 · 可交易 ${u.kept} · 排除 ${u.dropped}` +
    (parts.length ? `（${parts.join(' / ')}）` : '');
}

function renderTickerTape() {
  const arr = Object.values(state.tickers).sort((a, b) => (b.quoteVol24h || 0) - (a.quoteVol24h || 0)).slice(0, 40);
  $('tickerTape').innerHTML = arr.map((t) => `
    <span class="tk">
      <span class="n">${esc(t.name || t.instId)}</span>
      <span class="p">${fmtPrice(t.last)}</span>
      <span class="${cls(t.chgPct)}">${fmtPct(t.chgPct)}</span>
    </span>`).join('');
}

function renderInstInfo() {
  const it = state.insts.find((x) => x.instId === state.curInst);
  const t = state.tickers[state.curInst] || {};
  if (!it) { $('instInfo').innerHTML = '<div class="k muted">未选择合约</div>'; return; }
  // 当前生效的准入上限（来自 /api/state，后端热读 configs/okx_strategy.json）
  const st_ = state.lastState || {};
  const catName = { '1': '加密', '3': '美股/ETF', '4': '商品' }[it.instCategory] || it.instCategory || '--';
  const rows = [
    ['合约', it.instId],
    ['名称', it.name],
    ['最新价', fmtPrice(t.last || it.last)],
    ['24h 涨跌', fmtPct(t.chgPct)],
    ['24h 最高', fmtPrice(t.high24h)],
    ['24h 最低', fmtPrice(t.low24h)],
    ['24h 成交额', fmtVol(it.quoteVol24h)],
    ['合约面值 ctVal', it.ctVal],
    ['乘数 ctMult', it.ctMult],
    ['最小变动 tickSz', it.tickSz],
    ['下单粒度 lotSz', it.lotSz],
    ['最小下单 minSz', it.minSz],
    ['最大杠杆', (it.lever || '--') + 'x'],
    ['状态', it.state],
    // ---- 准入（「这只币到底能不能买」）----
    ['品种分类', catName],
    ['准入结论', it.tradeable ? '✔ 可交易' : '✘ ' + (it.excludeLabel || it.excludeReason || '已排除')],
    ['最小一手保证金', it.marginUsdt ? fmtNum(it.marginUsdt, 4) + ' U' : '--'],
    // 把「这只币」和「准入上限」摆在一起，一眼看出是不是卡在资金这一关上
    ['准入上限', st_ && st_.maxOrderMarginUsdt != null
      ? '≤ ' + fmtNum(st_.maxOrderMarginUsdt, 2) + ' U'
        + (it.marginUsdt > 0
          ? (it.marginUsdt <= st_.maxOrderMarginUsdt + 1e-9 ? '（满足）' : '（超出，买不起）')
          : '')
      : '--'],
    ['下单口径', (state.marginText || '--') + (it.marginUsdt > state.entryMargin ? '（放大到 1 张）' : '')],
  ];
  $('instInfo').innerHTML = rows
    .map(([k, v]) => `<span class="k">${esc(k)}</span><span class="v">${esc(v)}</span>`).join('');
}

function renderServiceInfo(st) {
  if (!st) return;
  const sx = (st.strategy && st.strategy.exit) || {};
  const sa = (st.strategy && st.strategy.addon) || {};
  const addonTxt = sa.enabled === false
    ? '关闭'
    // ★ 2026-10-01 起「加满自动平仓」已删除 —— 加仓次数只限制还能补几次，
    //   不再是出场条件，所以这里不再显示「满则平仓」。
    : `最多 ${sa.max_times || '--'} 次（只限制补仓，不影响出场）`;
  // 超时平仓：360 → "6 小时"。整数小时就说小时，否则说分钟，和后台的
  // HoldText() 口径一致（以前后台写「60 分钟」、网页写「1 小时」，两边对不上）。
  const holdTxt = sx.max_hold_minutes > 0
    ? (sx.max_hold_minutes % 60 === 0 && sx.max_hold_minutes >= 60
      ? (sx.max_hold_minutes / 60) + ' 小时'
      : sx.max_hold_minutes + ' 分钟')
    : (sx.max_hold_bars > 0 ? sx.max_hold_bars + ' 根' : '关闭');
  const info = [
    ['每笔保证金', state.marginText || '--'],
    // 准入上限：symbolList 里「最小一手保证金 ≤ 这个值」才可买入。
    // 后端热读 configs/okx_strategy.json 的 max_order_margin_usdt，
    // 改完 JSON 保存，这里下次轮询就会变 —— 不用重启服务。
    ['可买入上限', (st.maxOrderMarginUsdt != null
      ? '最小一手 ≤ ' + fmtNum(st.maxOrderMarginUsdt, 2) + 'U'
      : '--') + '（改 JSON 即时生效）'],
    ['策略周期', (st.strategy && st.strategy.bar) || '--'],
    ['扫描周期', ((st.strategy && st.strategy.bars_enabled) || []).join(' / ') || '--'],
    ['信号周期', ((st.strategy && st.strategy.signal_bars) || []).join(' / ') || '--'],
    // 出场条件三条一起列出来，一眼能看出「没有任何一条跟加仓次数有关」
    ['出场条件', `止盈 ${(sx.take_profit_pct != null ? sx.take_profit_pct : '--')}% · 超时 ${holdTxt}` +
      (sx.boll_upper_exit ? ' · 布林上轨' : '')],
    ['止盈', (sx.take_profit_pct != null ? sx.take_profit_pct : '--') + '%'],
    ['止损', sx.stop_loss_pct > 0 ? sx.stop_loss_pct + '%' : '不设'],
    ['超时平仓', holdTxt],
    ['布林上轨平仓', sx.boll_upper_exit ? '开' : '关'],
    ['加仓', addonTxt],
    ['共振阈值', String((st.strategy && st.strategy.score_threshold) || '--') + ' / 8'],
    // 三条独立红线（2026-10-01 起）：
    //   记录表 30 天 → 月度任务里清；K 线 365 天 → 年度任务里清；日志 30 天 → 月度任务里清
    ['记录保留', String(st.retainDays || 30) + ' 天（月度清理）'],
    ['K线保留', String(st.klineRetainDays || 365) + ' 天（年度清理）'],
    ['日志保留', String(st.logRetainDays || 30) + ' 天（月度清理）'],
    ['磁盘守卫', (st.archiveMinFreeGB || 10) + ' GB 以下只留当月 · 当前可用 ' +
      (st.freeDiskGB != null ? st.freeDiskGB.toFixed(1) : '--') + ' GB'],
    ['回补天数', String(st.backfillDays) + ' 天'],
    ['队列', String(st.queueLen)],
    ['在线时长', Math.floor((st.uptimeSec || 0) / 60) + ' 分'],
    ['版本', st.version],
  ];
  $('svcInfo').innerHTML = info
    .map(([k, v]) => `<span class="k">${esc(k)}</span><span class="v">${esc(v)}</span>`).join('');

  const tb = $('tblTables').querySelector('tbody');
  tb.innerHTML = Object.keys(st.tables || {}).sort()
    .map((k) => `<tr><td>${esc(k)}</td><td>${fmtNum(st.tables[k], 0)}</td></tr>`).join('');
}

// renderStats 顶栏 + 底栏。
//
// 顶栏这几个数字是「实时」的：/api/account 每 2 秒拉一次（引擎那边每 3 秒
// 往 equity 表写一条快照），所以权益 / 可用 / 浮盈 是跟着盘面跳的。
//
// 注意：老版本这里写的是 `s.klineRows ? '--' : '--'`，两个分支都是 '--'，
// 导致「账户权益」从来没显示过 —— 顺手修掉。
function renderStats() {
  const st = state.lastState || {};
  const s = st.stats || {};
  const a = state.account || st.account || {};

  // ---- 账户三件套：权益 / 可用 / 本金 ----
  $('stEquity').textContent = a.hasEquity ? fmtNum(a.totalEq, 4) : '--';
  $('stEquity').className = a.hasEquity ? 'accent' : '';
  $('stAvail').textContent = a.hasEquity ? fmtNum(a.avail, 4) : '--';
  $('stPrincipal').textContent = a.principal > 0 ? fmtNum(a.principal, 4) : '--';

  // ---- 浮盈 / 总盈亏 / 今日 ----
  $('stUpl').textContent = fmtNum(a.upl, 4);
  $('stUpl').className = cls(a.upl);
  $('stPnl').textContent = fmtNum(a.totalPnl, 4);
  $('stPnl').className = cls(a.totalPnl);
  $('stRoi').textContent = a.principal > 0 ? fmtPct(a.roi, 2) : '--';
  $('stRoi').className = 'sub ' + cls(a.roi);
  $('stTodayPnl').textContent = fmtNum(a.todayPnl, 4);
  $('stTodayPnl').className = cls(a.todayPnl);

  // ---- 持仓 / 胜率 ----
  // 没成交过时也要显示 0 / 0.0%，不能留 -- —— 留白会让用户以为界面坏了
  // （顶栏这一排本来就是实时刷新的，给 0 也是「实时」的一部分）。
  $('stPos').textContent = a.posCount || s.posCount || 0;
  $('stPos').className = (a.posCount || 0) > 0 ? 'accent' : '';
  const total = s.tradesTotal || 0;
  $('stWin').textContent = total > 0 ? (s.winRate || 0).toFixed(1) + '%' : '0.0%';
  $('stWin').className = total > 0 ? (s.winRate >= 50 ? 'up' : 'down') : 'muted';
  $('stWinSub').textContent = total > 0
    ? `${total} 笔 · 已实现 ${fmtNum(a.realized, 3)}`
    : '待首笔平仓';

  // ---- 自动交易状态灯 ----
  renderLiveBadge(st.live);

  // 底栏：把原来挤在顶栏的 K线行数 / 合约数 / 计划仓位挪过来
  $('footLeft').textContent =
    `OKX 全量化终端 · ${st.version || ''} · 数据覆盖 ${state.days} 天 · ${state.marginText || ''}` +
    ` · K线 ${fmtNum(s.klineRows, 0)} 行 · 合约 ${fmtNum(s.instCount, 0)} 个`;
  $('footRight').textContent = `数据库：${st.dbPath || '--'} · 服务器时间 ${s.serverTime || '--'}`;
}

// renderLiveBadge 顶栏右侧那个「自动交易」状态灯
function renderLiveBadge(live) {
  const el = $('stLive');
  if (!el) return;
  if (!live) { el.textContent = '--'; el.className = 'live-pill'; return; }
  if (!live.running) { el.textContent = '已关闭'; el.className = 'live-pill off'; return; }

  // 有错误优先报错，其次显示「dry_run / 实盘」
  if (live.lastError) {
    el.textContent = '异常';
    el.className = 'live-pill err';
    el.title = live.lastError;
    return;
  }
  const mode = live.dryRun ? '试算' : (live.simulated ? '模拟' : '实盘');
  el.textContent = `${mode} 巡检 ${live.exitEverySec}s`;
  el.className = 'live-pill' + (live.dryRun || live.simulated ? ' warn' : ' on');
  el.title = `止盈巡检每 ${live.exitEverySec} 秒 · 信号扫描每 ${live.entryEverySec} 秒\n` +
    `累计：巡检 ${live.ticks} 轮 / 扫描 ${live.scans} 轮 / 开仓 ${live.opens} 笔 / 平仓 ${live.exits} 笔\n` +
    (live.lastNote || '');
}

function setConn(live, text) {
  const el = $('conn');
  el.className = 'conn ' + (live ? 'live' : 'dead');
  el.querySelector('span').textContent = text;
}

/* ------------------------------------------------------------------ */
/* 数据加载                                                            */
/* ------------------------------------------------------------------ */

async function loadState() {
  const st = await api('/api/state');
  state.lastState = st;
  state.bars = st.bars || state.bars;
  state.days = st.backfillDays || 30;
  state.marginText = st.marginText || '';
  state.entryMargin = (((st.strategy || {}).entry || {}).margin_usdt) || 0.1;
  state.maxMargin = (((st.strategy || {}).entry || {}).max_margin_usdt) || 0.5;
  if (st.account) state.account = st.account;
  renderTimeframes();
  renderStats();
  renderServiceInfo(st);
  setConn(true, '已连接');
}

// loadAccount 顶栏的实时数字（每 2 秒）。
//
// 单独开一个轻接口而不是反复拉 /api/state：/api/state 要顺着 information_schema
// 数各表行数，2 秒一次太浪费；这里只有一条 equity 快照 + 几个 COUNT。
async function loadAccount() {
  const j = await api('/api/account');
  state.account = j.account || state.account;
  if (j.live) state.lastState = Object.assign({}, state.lastState || {}, { live: j.live });
  renderStats();
}

async function loadInstruments() {
  const j = await api('/api/instruments?scope=' + encodeURIComponent(state.scope));
  state.insts = j.list || [];
  renderUniverse(j.universe);
  renderInstList();
  if (!state.curInst && state.insts.length) {
    // 默认选中成交额最大的
    await selectInst(state.insts[0].instId);
  } else {
    renderInstList();
  }
}

let tickerN = 0;

async function loadTickers() {
  const j = await api('/api/tickers');
  const map = {};
  (j.list || []).forEach((t) => { map[t.instId] = t; });
  state.tickers = map;
  tickerN++;

  // 行情每 2 秒来一次，但合约列表（400 行 DOM）和信息面板不用这么勤，
  // 每 5 次（≈10 秒）重建一次就够，否则页面会被无谓的重排拖卡。
  renderTickerTape();
  if (tickerN % 5 === 1) { renderInstList(); renderInstInfo(); }

  // 左上角标题（名称 / 最新价 / 涨跌幅）跟着行情走。
  // 一定要用统一的 renderChartHead，不要在这儿单独写几个字段 ——
  // 以前就是这里和 loadKline 各写一半，导致「点了别的合约名，标题不动」。
  renderChartHead();
}

/* ------------------------------------------------------------------ */
/* K 线图左上角：合约名 / 代码 / 最新价 / 涨跌幅                          */
/* ------------------------------------------------------------------ */

// renderChartHead 把「当前合约」的名称与价格同步写进图表左上角。
//
// 为什么必须是独立的同步函数：
//   1) 点击下方表格（持仓 / 交易明细 / 历史 / 信号）里的合约名时，用户期待
//      标题**立刻**变。以前标题只在 loadKline 成功返回后才写，于是要先等一次
//      /api/mark 往返；中途任何一步出错（比如 renderInstList 抛异常）标题就
//      永远停在旧合约上 —— 这就是「左上角合约名和价格不更新」的根因。
//   2) 价格以前只在 loadTickers 里「ticker 存在才写」，拿不到行情时既不更新
//      也不清空，留着上一个合约的价格，看起来像是没切换。
//
// 所以这里：先无条件把名称/代码刷新（纯本地数据，零延迟），
// 价格有就写、没有就显示 "--"，绝不留上一个合约的残留值。
function renderChartHead() {
  const inst = state.curInst || '';
  const bar = state.curBar || '';
  const tk = (state.tickers && state.tickers[inst]) || {};
  const it = (state.insts || []).find((x) => x.instId === inst) || {};

  // 名称：优先行情里的 name → 合约列表里的 name → 自己从 instId 推 "BASE/USDT"
  let name = tk.name || it.name || '';
  if (!name && inst) name = inst.split('-')[0] + '/USDT';
  $('pairName').textContent = name || '--';
  $('pairInst').textContent = inst ? (inst + ' · ' + bar) : '请选择合约';

  const p = $('pairPrice');
  if (tk.last > 0) {
    p.textContent = fmtPrice(tk.last);
    p.className = tk.chgPct > 0 ? 'up' : (tk.chgPct < 0 ? 'down' : '');
  } else {
    // 拿不到该合约的行情：显示占位，别把上一个合约的价格留在屏幕上
    p.textContent = '--';
    p.className = '';
  }
  const cg = $('pairChg');
  if (tk.chgPct == null || !(tk.last > 0)) {
    cg.textContent = '--';
    cg.className = 'chg flat';
  } else {
    cg.textContent = fmtPct(tk.chgPct);
    cg.className = 'chg ' + cls(tk.chgPct);
  }
}

/* ------------------------------------------------------------------ */
/* K 线分页：初始 1000 根，向左滚到边上自动加载更早的 1000 根            */
/* ------------------------------------------------------------------ */

// mergeByTs 把两组 {ts,...} 序列按时间合并去重（新的覆盖旧的），返回升序数组
function mergeByTs(older, newer) {
  if (!older || !older.length) return (newer || []).slice();
  if (!newer || !newer.length) return older.slice();
  const map = new Map();
  older.forEach((x) => map.set(x.ts, x));
  newer.forEach((x) => map.set(x.ts, x));   // 新的覆盖旧的
  return Array.from(map.values()).sort((a, b) => a.ts - b.ts);
}

// mergePage 把一页新数据并进 state（klines 与指标一起按 ts 对齐）
//
// 顺带把「这一页新增了多少根」记在 j.__added 上：定时刷新时只需要
// 重画最后这几根，不用整幅 setData。
function mergePage(j, reset) {
  const k = j.kline || [];
  const ind = {
    ma7: j.ma7 || [], ma25: j.ma25 || [], ma99: j.ma99 || [],
    bollUp: j.bollUp || [], bollMid: j.bollMid || [], bollLo: j.bollLo || [],
  };
  if (reset) {
    state.klines = k.slice();
    state.ind = ind;
    resetMarkers();
    mergeMarkers(j.markers);
    j.__added = k.length;
    return;
  }
  const before = state.kMap && state.kMap.size ? state.kMap.size : state.klines.length;
  state.klines = mergeByTs(state.klines, k);
  Object.keys(ind).forEach((key) => {
    state.ind[key] = mergeByTs(state.ind[key] || [], ind[key]);
  });
  mergeMarkers(j.markers);
  j.__added = Math.max(3, state.klines.length - before + 1);
}

// renderKline 把 state 里的数据整幅铺到图上（换合约 / 换周期 / 翻页时用）
//
// keepRange：翻页（往前插数据）时必须保持当前可视位置，
//            否则 setData 之后画面会跳回最左边。
function renderKline(tickSize, keepRange) {
  const lr = keepRange ? state.chart.timeScale().getVisibleLogicalRange() : null;
  const prevCount = state.klines.length;

  setCandleFormat(tickSize);
  buildIdx();
  paintAll();
  paintMarkers();

  if (lr && state.klines.length > prevCount) {
    // 往前插了 bar，逻辑索引整体右移，可视区跟着右移同样的量
    const shift = state.klines.length - prevCount;
    state.chart.timeScale().setVisibleLogicalRange({ from: lr.from + shift, to: lr.to + shift });
    state.scrollGuardUntil = Date.now() + 600;   // 插完数据别再立刻触发下一页
  }
  renderLegend(state.klines[state.klines.length - 1] || null);
}

// setCandleFormat 按合约最小变动价位设置价格精度
function setCandleFormat(tickSize) {
  if (!tickSize) return;
  const prec = Math.max(0, Math.min(8, Math.ceil(-Math.log10(tickSize))));
  state.candle.applyOptions({ priceFormat: { type: 'price', precision: prec, minMove: tickSize } });
}

const toCandle = (k) => ({ time: Math.floor(k.ts / 1000), open: k.o, high: k.h, low: k.l, close: k.c });
const toVolBar = (k) => ({
  time: Math.floor(k.ts / 1000), value: k.v,
  color: k.c >= k.o ? 'rgba(14,203,129,.45)' : 'rgba(246,70,93,.45)',
});
const toLine = (arr) => (arr || [])
  .filter((p) => p.v !== null && p.v !== undefined && isFinite(p.v))
  .map((p) => ({ time: Math.floor(p.ts / 1000), value: p.v }));

// paintAll 全量重绘（只在「数据集合变了」时调用）
function paintAll() {
  state.candle.setData(state.klines.map(toCandle));
  state.volume.setData(state.klines.map(toVolBar));
  state.ma7.setData(toLine(state.ind.ma7));
  state.ma25.setData(toLine(state.ind.ma25));
  state.ma99.setData(toLine(state.ind.ma99));
  state.bollUp.setData(toLine(state.ind.bollUp));
  state.bollMid.setData(toLine(state.ind.bollMid));
  state.bollLo.setData(toLine(state.ind.bollLo));
}

// paintTail 只更新最后 n 根（定时刷新 / 实时报价走这条路径）
//
// 这是「实时更新」的关键：以前每次轮询都对几千根 K 线 setData 一遍，
// 既卡又会让可视区抖动。改成系列 update() 之后，只有最后一根在动。
function paintTail(n) {
  const len = state.klines.length;
  if (!len) return;
  if (!n || n < 1) n = 1;
  const from = Math.max(0, len - n);
  const tail = state.klines.slice(from);
  tail.forEach((k) => {
    state.candle.update(toCandle(k));
    state.volume.update(toVolBar(k));
  });
  updateLineTail(state.ma7, state.ind.ma7, from, len);
  updateLineTail(state.ma25, state.ind.ma25, from, len);
  updateLineTail(state.ma99, state.ind.ma99, from, len);
  updateLineTail(state.bollUp, state.ind.bollUp, from, len);
  updateLineTail(state.bollMid, state.ind.bollMid, from, len);
  updateLineTail(state.bollLo, state.ind.bollLo, from, len);
}

// updateLineTail 指标序列只 update 落在 [from,to) 区间里的点
function updateLineTail(series, arr, from, to) {
  if (!series || !arr || !arr.length) return;
  const kFrom = state.klines[from] ? state.klines[from].ts : 0;
  const kTo = state.klines[to - 1] ? state.klines[to - 1].ts : 0;
  for (let i = arr.length - 1; i >= 0; i--) {
    const ts = arr[i].ts;
    if (ts < kFrom) break;
    if (ts <= kTo && arr[i].v !== null && isFinite(arr[i].v)) {
      series.update({ time: Math.floor(ts / 1000), value: arr[i].v });
    }
  }
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

// updatePageHint 底部提示：已加载多少根 / 还能不能继续往前翻
function updatePageHint() {
  const n = state.klines.length;
  $('chartHint').textContent = `已加载 ${n} 根 ${state.curBar} K线`;
  const el = $('pageInfo');
  if (!el) return;
  // 顺带报一下这一段里标了多少个买卖点，免得用户以为标记没出来
  const buys = state.markers.filter((m) => m.kind === 'open' || m.kind === 'signal').length;
  const sells = state.markers.filter((m) => m.kind === 'close').length;
  const mk = state.markers.length ? ` · 🚀 ${buys} · 🍃 ${sells}` : '';
  el.textContent = (state.hasMore
    ? `已加载 ${n} 根 · 向左滚动继续加载（每页 ${KLINE_PAGE} 根）`
    : `已加载 ${n} 根 · 已到最早`) + mk;
}

// onScroll 滚到左边缘附近就预加载下一页
//
// scrollGuard：刚 fitContent / 刚往前插完数据时会触发一次可视区回调，
// 那一次不能当成「用户往左拖」——否则一打开就把所有历史页全拉下来了。
function onScroll() {
  if (!state.hasMore || state.loadingOlder) return;
  if (Date.now() < (state.scrollGuardUntil || 0)) return;
  const lr = state.chart.timeScale().getVisibleLogicalRange();
  if (!lr) return;
  if (lr.from <= PAGE_TRIGGER) loadOlder();
}

let klineTimer = null;

async function selectInst(instId) {
  if (!instId) return;
  state.curInst = instId;
  // 标题立刻刷 —— 不等网络。用户点了哪张表里的合约名，左边标题马上就得变。
  renderChartHead();
  // 左边合约列表 / 右侧合约信息只是「顺带刷新」，它们失败绝不能挡住画图。
  // （以前 renderInstList 一抛异常，loadKline 就永远不被调用，标题也就不动了。）
  try { renderInstList(); } catch (e) { console.warn('renderInstList 失败', e); }
  try { renderInstInfo(); } catch (e) { console.warn('renderInstInfo 失败', e); }
  await loadKline(true);
}

// jumpToInst 从下方表格（信号 / 持仓 / 历史）点合约名 → 直接切 K 线过去。
//
// 为什么要先放宽 scope：这些表里的合约未必在当前左侧列表（默认只看
// 「可交易」，而信号可能是被排除的合约，或者今天准入结论变了）。不先
// 放宽，就会出现「图切过去了、左边列表却找不到它高亮」的割裂感。
async function jumpToInst(instId, bar) {
  if (!instId) return;
  if (bar && BAR_MS[bar]) {
    state.curBar = bar;
    try { renderTimeframes(); } catch (e) { console.warn('renderTimeframes 失败', e); }
  }
  if (!(state.insts || []).some((x) => x.instId === instId)) {
    state.scope = 'all';
    document.querySelectorAll('.scope').forEach((b) =>
      b.classList.toggle('active', b.dataset.scope === 'all'));
    try {
      await loadInstruments();
    } catch (e) { /* 列表拉不到也不影响画图，/api/mark 不依赖它 */ }
  }
  // selectInst 内部已经做了「先同步刷标题、再拉数据」，且各步都有保护；
  // 万一还是抛了（比如图表库异常），也要保证切换动作整体不中断。
  try {
    await selectInst(instId);
  } finally {
    renderChartHead();
  }
  // 图表在页面上半部分，滚回去用户才看得到
  const box = document.querySelector('.chart-box');
  if (box && box.scrollIntoView) box.scrollIntoView({ behavior: 'smooth', block: 'center' });
  else window.scrollTo({ top: 0, behavior: 'smooth' });
  // 闪一下边框，明确告诉用户「切到这张图了」
  const panel = document.querySelector('main .center');
  if (panel) {
    panel.classList.add('jump-flash');
    setTimeout(() => panel.classList.remove('jump-flash'), 1200);
  }
}

// loadKline(reset)
//   reset=true  → 重新载入最新一页（换合约 / 换周期 / 手动刷新）
//   reset=false → 定时刷新，只把最新一页并进来（已加载的老数据保留）
async function loadKline(reset) {
  if (!state.curInst) return;
  const inst = state.curInst, bar = state.curBar;
  const key = inst + '|' + bar;
  if (reset) {
    state.lastKlineKey = '';
    state.klines = [];
    state.ind = {};
    state.hasMore = false;
    resetMarkers();
  }

  // 先把标题刷成当前合约（本地数据，零延迟）：即使下面 /api/mark 挂了，
  // 用户也能看到「图已经切到这个合约了」，而不是标题停在旧合约上。
  renderChartHead();

  $('chartHint').textContent = '加载中…';
  let j;
  try {
    j = await api(`/api/mark?inst=${encodeURIComponent(inst)}&bar=${encodeURIComponent(bar)}` +
      `&days=${state.days}&limit=${KLINE_PAGE}&_=${Date.now()}`);
  } catch (e) {
    $('chartHint').textContent = '加载失败：' + e.message;
    renderChartHead();          // 失败也别让标题留着上一个合约
    return;
  }
  // 期间用户又切了别的合约：这次结果作废。但标题要保持和当前状态一致。
  if (inst !== state.curInst || bar !== state.curBar) { renderChartHead(); return; }

  mergePage(j, reset);

  const cov = j.coverage || {};
  const page = j.page || {};
  // hasMore 以「已加载的最老一根」与「库里最老一根」比较为准
  state.hasMore = state.klines.length > 0 &&
    page.earliestTs > 0 && state.klines[0].ts > page.earliestTs;

  // 名称 / 最新价 / 涨跌幅 统一由 renderChartHead 负责，这里不再各写一半
  renderChartHead();

  const tickSize = (state.insts.find((x) => x.instId === inst) || {}).tickSz || 0.0001;

  // 定时刷新走「只重画尾巴」：整幅 setData 会让可视区抖动，而且越到后面越卡
  if (reset) {
    renderKline(tickSize, false);
  } else {
    setCandleFormat(tickSize);
    buildIdx();
    paintTail(j.__added || 3);
    paintMarkers();   // 新信号/新平仓会随时冒出来，标记也得跟着刷新
    renderLegend(state.klines[state.klines.length - 1] || null);
  }

  if (reset || state.lastKlineKey !== key) {
    state.chart.timeScale().fitContent();
    state.scrollGuardUntil = Date.now() + 1200;   // fitContent 也会触发可视区回调，先压住
    state.lastKlineKey = key;
  }

  updatePageHint();
  $('covInfo').textContent =
    `覆盖 ${(cov.days || 0).toFixed(1)} 天 / ${fmtNum(cov.count || 0, 0)} 根` +
    (cov.minTs ? `（${fmtShort(cov.minTs)} → ${fmtShort(cov.maxTs)}）` : '');

  const chg = (state.tickers[inst] || {}).chgPct;
  const cg = $('pairChg');
  cg.textContent = fmtPct(chg);
  cg.className = 'chg ' + cls(chg);

  const newest = state.klines[state.klines.length - 1];
  if (newest) {
    // 「延迟」不能拿 now - 最新一根开盘时间算：一根 15m K 线要走 15 分钟，
    // 开盘时间天生落后 now 0~15 分钟，那是「走盘中」不是「数据滞后」。
    // 真正的滞后口径 = 最新一根的「结束时间」落后 now 多少。
    const barMs = BAR_MS[bar] || 900000;
    const lag = Date.now() - newest.ts;               // 距开盘
    const stale = Date.now() - (newest.ts + barMs);   // 距本根该结束的时刻（<0 = 还在走）
    if (stale < 0) {
      const leftMin = Math.ceil(-stale / 60000);
      const leftTxt = barMs >= 3600000
        ? Math.ceil(-stale / 3600000) + ' 小时'
        : leftMin + ' 分';
      $('liveInfo').textContent =
        `实时 · 最新一根 ${fmtTime(newest.ts)} 走盘中（本根还剩 ${leftTxt}）`;
      $('liveInfo').className = 'muted';
    } else if (lag < barMs * 1.5) {
      $('liveInfo').textContent =
        `实时 · 最新一根 ${fmtTime(newest.ts)}（更新于 ${Math.max(0, Math.round(stale / 1000))}s 前）`;
      $('liveInfo').className = 'muted';
    } else {
      $('liveInfo').textContent = `⚠ 数据滞后 ${Math.round(stale / 60000)} 分钟`;
      $('liveInfo').className = 'down';
    }
  }

  scheduleKlineRefresh();
}

// 各周期的轮询间隔：短周期勤一点，长周期没必要
const REFRESH_MS = { '1m': 3000, '3m': 4000, '5m': 6000, '15m': 8000 };

function scheduleKlineRefresh() {
  if (klineTimer) clearTimeout(klineTimer);
  const ms = REFRESH_MS[state.curBar] || 10000;
  // 用 setTimeout 串起来而不是 setInterval：请求慢的时候不会堆积
  klineTimer = setTimeout(async () => {
    if (!document.hidden) await loadKline(false).catch(() => {});
    scheduleKlineRefresh();
  }, ms);
}

/* ------------------------------------------------------------------ */
/* 实时：收盘倒计时 + 用最新价刷新最后一根                                */
/* ------------------------------------------------------------------ */

// tickLive 每秒跑一次
//   1) 更新本根 K 线的收盘倒计时（币安那种 14:59 倒数）
//   2) 用 ticker 的最新价就地刷新最后一根 K 线的高低收 —— 图会「跳」
//
// 只在这一根还没走完的时候才动它，否则会把已经收盘的历史 K 线改坏。
function tickLive() {
  // ---- 倒计时 ----
  const d = BAR_MS[state.curBar];
  if (d) {
    const left = Math.max(0, Math.ceil(Date.now() / d) * d - Date.now());
    const mm = String(Math.floor(left / 60000)).padStart(2, '0');
    const ss = String(Math.floor((left % 60000) / 1000)).padStart(2, '0');
    $('barCountdown').textContent = `${mm}:${ss}`;
    $('barCountdown').className = left < 10000 ? 'countdown hot' : 'countdown';
  }

  // ---- 用最新价刷新最后一根 ----
  const t = state.tickers[state.curInst];
  const n = state.klines.length;
  if (!t || !t.last || !n || !d) return;
  const last = state.klines[n - 1];
  if (Date.now() - last.ts >= d) return;   // 这根已经收盘了，别改它

  const px = t.last;
  if (px === last.c && px <= last.h && px >= last.l) return;   // 没变化就不重画
  last.c = px;
  if (px > last.h) last.h = px;
  if (px < last.l) last.l = px;

  state.candle.update(toCandle(last));
  state.volume.update(toVolBar(last));
  if (!document.hidden) renderLegend(last);
}

/* ------------------------------------------------------------------ */
/* 表格                                                                */
/* ------------------------------------------------------------------ */

async function loadPositions() {
  const j = await api('/api/positions');
  const rows = j.list || [];
  state.positions = rows;   // 历史列表里「持仓中」的行要用它补实时浮盈
  $('badgePos').textContent = rows.length;
  const tb = $('tbPositions');
  if (!rows.length) {
    tb.innerHTML = '<tr><td colspan="15" class="empty">暂无持仓</td></tr>';
    return;
  }
  tb.innerHTML = rows.map((p) => {
    const dir = (p.side || 'buy').toLowerCase() === 'sell' ? 'short' : 'long';

    // 距爆仓：标记价离强平价还有几个百分点。越近越红。
    // 逐仓做多时强平价在下方，所以「跌多少就爆」= (标记价−强平价)/标记价。
    const liq = p.liqPx || 0;
    const mark = p.markPx || p.last || 0;
    let distHtml = '<span class="muted">--</span>';
    if (liq > 0 && mark > 0) {
      const dist = Math.abs(mark - liq) / mark * 100;
      const c = dist < 3 ? 'down' : (dist < 8 ? 'accent' : 'muted');
      distHtml = `<span class="${c}"><b>${dist.toFixed(2)}%</b></span>`;
    }

    return `<tr>
      <td><a class="inst-link" href="#" data-inst="${esc(p.instId)}" title="点开 ${esc(p.instId)} 的 K 线图">${esc(p.name || p.instId)}</a></td>
      <td><span class="tag-pill pill-${dir}">${dir === 'long' ? '多' : '空'}</span></td>
      <td>${fmtNum(p.sz, 0)}</td>
      <td>${fmtPrice(p.entryPx)}</td>
      <td>${fmtPrice(mark)}</td>
      <td class="muted">${fmtPrice(liq)}</td>
      <td>${distHtml}</td>
      <td>${fmtNum(p.margin, 3)}</td>
      <td>${p.leverage}x</td>
      <td>${fmtNum(p.notional, 2)}</td>
      <td class="${cls(p.upl)}"><b>${fmtNum(p.upl, 4)}</b></td>
      <td class="${cls(p.uplPct)}">${fmtPct(p.uplPct)}</td>
      <td class="muted">${fmtPrice(p.takeProfitPx)}</td>
      <td>${(p.holdMin / 60).toFixed(1)}h</td>
      <td>${p.score ? p.score + '/8' : '--'}</td>
    </tr>`;
  }).join('');

  // 顶栏的持仓数也跟着走，别等下一次 /api/account
  $('stPos').textContent = rows.length;
}

// ★ 历史窗口：30 天（2026-10-01 由 3 天改为 30 天）。
//
// 为什么改：本地 trade 表原来只记「程序自己下的单」，一共 8 条，
// 用户看历史面板就 8 行、还以为分页被切了。现在两件事一起做：
//   ① 后端每 10 分钟从 OKX /api/v5/account/positions-history 同步真实平仓仓位
//      （3 个月窗口，实测 100+ 个仓位 / 8000+ 笔成交）；
//   ② 前端窗口从 3 天放宽到 30 天，和「只保留最近 30 天」的保留策略对齐。
// 分页照旧走服务端，page/size 由 #pagerHistory 控制，能一直往后翻。
const HIST_DAYS = 30;

async function loadHistory() {
  // 持仓中的仓位不受天数限制 —— 开了 5 天还没平，它仍然是当前持仓。
  //
  // 分页在服务端：只把当前这一页拉下来（默认 20 条），不再整批传。
  const st = pgState('history');
  const j = await api(`/api/history?days=${HIST_DAYS}&size=${st.size}&page=${st.page}`);
  state.historyRows = j.list || [];
  renderHistory(j);
}

function renderHistory(meta) {
  const rows = state.historyRows || [];
  const total = (meta && typeof meta.total === 'number') ? meta.total : rows.length;
  const openTotal = (meta && typeof meta.openCount === 'number') ? meta.openCount : 0;
  $('badgeHis').textContent = openTotal > 0 ? `${total} · ${openTotal} 持仓中` : total;

  const tb = $('tbHistory');
  if (!rows.length) {
    tb.innerHTML = '<tr><td colspan="12" class="empty">最近 30 天暂无交易记录 —— 引擎出信号开仓后会自动出现在这里；OKX 上的历史仓位每 10 分钟同步一次</td></tr>';
    renderPager('history', total, loadHistory);
    return;
  }

  // 持仓中的行用实时行情补盈亏，所以数字是跳动的
  const posMap = {};
  (state.positions || []).forEach((p) => { posMap[p.instId] = p; });

  const { slice } = pgSlice('history', rows, total);
  tb.innerHTML = slice.map((t) => {
    const dir = (t.side || 'buy').toLowerCase() === 'sell' ? 'short' : 'long';
    const isOpen = (t.status || 'closed') === 'open';
    const live = isOpen ? posMap[t.instId] : null;
    // 持仓中的行没有平仓价/平仓盈亏，用实时浮盈顶上，让用户看到它一直在动
    const pnl = isOpen ? (live ? live.upl : 0) : t.pnl;
    const pnlPct = isOpen ? (live ? live.uplPct : 0) : t.pnlPct;
    const reason = isOpen ? '持仓中（未平仓）' : (t.reason || '');
    return `<tr${isOpen ? ' class="row-open"' : ''}>
      <td><a class="inst-link" href="#" data-inst="${esc(t.instId)}" title="点开 ${esc(t.instId)} 的 K 线图">${esc(t.name || t.instId)}</a></td>
      <td><span class="tag-pill pill-${dir}">${dir === 'long' ? '多' : '空'}</span></td>
      <td>${fmtNum(t.sz, 0)}</td>
      <td>${fmtPrice(t.entryPx)}</td>
      <td>${isOpen ? '<span class="pill-live">持仓中</span>' : fmtPrice(t.exitPx)}</td>
      <td>${fmtNum(t.margin, 2)}</td>
      <td>${t.leverage}x</td>
      <td class="${cls(pnl)}"><b>${fmtNum(pnl, 4)}</b></td>
      <td class="${cls(pnlPct)}">${fmtPct(pnlPct)}</td>
      <td class="muted" title="${esc(reason)}">${esc(reason.slice(0, 18))}</td>
      <td class="muted">${fmtTime(t.openTs)}</td>
      <td class="muted">${isOpen ? '待平仓' : fmtTime(t.closeTs)}</td>
    </tr>`;
  }).join('');

  renderPager('history', total, loadHistory);
}

// ---------------------------------------------------------------------------
// 交易明细（逐笔开仓 / 加仓 / 平仓流水）
// ---------------------------------------------------------------------------
//
// 和历史仓位的区别：历史是「一个仓位一行」的汇总，加仓会被合并进均价里
// 看不见；这里是一次一笔，能直接看到「几点几分加了多少仓、什么价、多少钱」。

async function loadEvents() {
  // 服务端分页：一次只取当前页，表头统计（笔数/已实现盈亏）由后端对
  // 整个 30 天窗口聚合，不受当前页影响。
  const st = pgState('events');
  const kind = state.evKind || '';
  const j = await api(`/api/events?days=${HIST_DAYS}&size=${st.size}&page=${st.page}` +
    (kind ? `&kind=${encodeURIComponent(kind)}` : ''));
  state.eventRows = j.list || [];
  renderEvents(j);
}

function renderEvents(meta) {
  const rows = state.eventRows || [];
  const m0 = meta || {};
  const total = typeof m0.total === 'number' ? m0.total : rows.length;
  const openN = Number(m0.openCount || 0);
  const addonN = Number(m0.addonCount || 0);
  const closeN = Number(m0.closeCount || 0);
  const pnlSum = Number(m0.pnlSum || 0);
  $('badgeEv').textContent = openN + addonN + closeN;

  const m = $('eventsMeta');
  if (m) {
    m.innerHTML = `最近 ${HIST_DAYS} 天 · 共 <b>${openN + addonN + closeN}</b> 笔：` +
      `买入 <b>${openN}</b> · 加仓 <b style="color:#3b82f6">${addonN}</b> · 平仓 <b>${closeN}</b>` +
      ` · 已实现盈亏 <b class="${pnlSum >= 0 ? 'up' : 'down'}">${fmtNum(pnlSum, 4)} U</b>`;
  }

  const tb = $('tbEvents');
  if (!rows.length) {
    tb.innerHTML = '<tr><td colspan="10" class="empty">最近 30 天暂无交易明细</td></tr>';
    renderPager('events', total, loadEvents);
    return;
  }

  const KIND = {
    open: { txt: '买入', pill: 'pill-open' },
    addon: { txt: '加仓', pill: 'pill-addon' },
    close: { txt: '平仓', pill: 'pill-close' },
  };

  const { slice } = pgSlice('events', rows, total);
  tb.innerHTML = slice.map((e) => {
    const k = KIND[e.kind] || { txt: e.kind, pill: '' };
    const isClose = e.kind === 'close';
    return `<tr>
      <td class="muted">${fmtTime(e.ts)}</td>
      <td><a class="inst-link" href="#" data-inst="${esc(e.instId)}" title="点开 ${esc(e.instId)} 的 K 线图">${esc(e.name || e.instId)}</a></td>
      <td><span class="tag-pill ${k.pill}">${k.txt}</span></td>
      <td>${fmtPrice(e.px)}</td>
      <td>${fmtNum(e.sz, 0)}</td>
      <td>${e.margin > 0 ? fmtNum(e.margin, 3) : '<span class="muted">--</span>'}</td>
      <td>${e.leverage ? e.leverage + 'x' : '<span class="muted">--</span>'}</td>
      <td class="${cls(e.pnl)}"><b>${isClose ? fmtNum(e.pnl, 4) : '--'}</b></td>
      <td class="${cls(e.pnlPct)}">${isClose ? fmtPct(e.pnlPct) : '--'}</td>
      <td class="muted" title="${esc(e.reason)}">${esc((e.reason || '').slice(0, 24))}</td>
    </tr>`;
  }).join('');

  renderPager('events', total, loadEvents);
}

async function loadSignals() {
  const st = pgState('signals');
  const j = await api(`/api/signals?size=${st.size}&page=${st.page}`);
  state.signalRows = j.list || [];
  renderSignals(j);
}

function renderSignals(meta) {
  const rows = state.signalRows || [];
  const total = (meta && typeof meta.total === 'number') ? meta.total : rows.length;
  $('badgeSig').textContent = total;
  const tb = $('tbSignals');
  if (!rows.length) {
    tb.innerHTML = '<tr><td colspan="10" class="empty">暂无信号</td></tr>';
    renderPager('signals', total, loadSignals);
    return;
  }
  const actedText = (a) => a === 1 ? '<span class="tag-pill pill-ok">已下单</span>'
    : (a === 2 ? '<span class="tag-pill pill-err">被拦</span>' : '<span class="tag-pill">仅记录</span>');
  const { slice } = pgSlice('signals', rows, total);
  tb.innerHTML = slice.map((s) => `<tr>
      <td><a class="inst-link" href="#" data-inst="${esc(s.instId)}" data-bar="${esc(s.bar || '')}" title="点开 ${esc(s.instId)} 的 K 线图">${esc(s.name || s.instId)}</a></td>
      <td>${esc(s.bar)}</td>
      <td class="muted">${fmtTime(s.ts)}</td>
      <td>${fmtPrice(s.close)}</td>
      <td class="accent">${s.score}/8</td>
      <td class="muted" title="${esc(s.hitList)}">${esc((s.hitList || '').slice(0, 20))}</td>
      <td>${(s.rsi || 0).toFixed(1)}</td>
      <td>${s.td || 0}</td>
      <td>${actedText(s.acted)}</td>
      <td class="muted" title="${esc(s.reason)}">${esc((s.reason || '').slice(0, 22))}</td>
    </tr>`).join('');
  renderPager('signals', total, loadSignals);
}

async function loadBackfill() {
  const j = await api('/api/backfill');
  const jobs = j.jobs || [], covs = j.coverage || [];
  $('badgeBf').textContent = covs.length;
  $('bfInfo').textContent = `回补队列 ${j.queueLen} · 已回补 ${covs.length} 组（目标 ${j.days} 天）`;

  const covMap = {};
  covs.forEach((c) => { covMap[c.instId + '|' + c.bar] = c; });
  state.bfRows = jobs.length ? jobs : covs.map((c) => ({
    instId: c.instId, bar: c.bar, rows: c.count, fromTs: c.minTs, toTs: c.maxTs,
    status: c.days >= j.days - 0.5 ? 'done' : 'pending',
    msg: `覆盖 ${c.days.toFixed(1)} 天`, name: c.name,
  }));
  state.bfCovMap = covMap;
  renderBackfill();
}

function renderBackfill() {
  const src = state.bfRows || [];
  const covMap = state.bfCovMap || {};
  const tb = $('tbBackfill');
  if (!src.length) {
    tb.innerHTML = '<tr><td colspan="8" class="empty">暂无回补任务</td></tr>';
    renderPager('backfill', 0, renderBackfill);
    return;
  }
  const { slice } = pgSlice('backfill', src);
  tb.innerHTML = slice.map((r) => {
    const cov = covMap[r.instId + '|' + r.bar] || {};
    const st = r.status || 'pending';
    const stCls = st === 'done' ? 'pill-ok' : (st === 'error' ? 'pill-err' : 'pill-run');
    return `<tr>
      <td>${esc(r.name || r.instId)}</td>
      <td>${esc(r.bar)}</td>
      <td>${fmtNum(r.rows || cov.count || 0, 0)}</td>
      <td class="accent">${(cov.days || 0).toFixed(1)} 天</td>
      <td class="muted">${fmtShort(r.fromTs || cov.minTs)}</td>
      <td class="muted">${fmtShort(r.toTs || cov.maxTs)}</td>
      <td><span class="tag-pill ${stCls}">${esc(st)}</span></td>
      <td class="muted" title="${esc(r.msg)}">${esc((r.msg || '').slice(0, 30))}</td>
    </tr>`;
  }).join('');
  renderPager('backfill', src.length, renderBackfill);
}

async function loadPnl() {
  // 最近 30 天（和「只保留最近 30 天」的保留窗口一致；引擎每 3 秒写一条快照，
  // 30 天原始点约 86 万个，后端先按天窗口过滤再抽稀到 1500 点，
  // 不然浏览器画不动）
  const j = await api(`/api/pnl?days=${HIST_DAYS}`);
  const rows = j.list || [];
  state.pnlData = rows;
  const empty = $('pnlEmpty');
  if (!rows.length) {
    empty.classList.remove('hidden');
    const meta0 = $('pnlMeta');
    if (meta0) meta0.textContent = '';
    return;
  }
  empty.classList.add('hidden');
  state.pnlLine.setData(rows.map((p) => ({ time: Math.floor(p.ts / 1000), value: p.totalEq || 0 })));
  // 权益快照是从引擎上线那一刻才开始累积的，还没满 30 天就如实写出来，
  // 免得用户以为曲线画错了或者数据丢了。
  const meta = $('pnlMeta');
  if (meta) {
    const t0 = rows[0].ts;
    const t1 = rows[rows.length - 1].ts;
    const spanH = (t1 - t0) / 3600000;
    const spanTxt = spanH >= 48 ? (spanH / 24).toFixed(1) + ' 天' : spanH.toFixed(1) + ' 小时';
    meta.textContent =
      `最近 ${HIST_DAYS} 天 · ${rows.length} 个采样点（${fmtShort(t0)} → ${fmtShort(t1)}，跨度 ${spanTxt}）` +
      (j.rawCount > rows.length ? ` · 原始 ${j.rawCount} 点已抽稀` : '');
  }
  // 只在第一次铺满视野：之后每 10 秒轮询不再 fitContent，
  // 否则用户刚缩放/拖动就被拽回去。
  if (!state.pnlFitted) {
    state.pnlChart.timeScale().fitContent();
    state.pnlFitted = true;
  }
}

/* ------------------------------------------------------------------ */
/* 事件                                                                */
/* ------------------------------------------------------------------ */

function bindEvents() {
  $('instSearch').addEventListener('input', renderInstList);

  // 合约列表范围切换：可交易 / 被排除 / 全部
  $('scopeGroup').addEventListener('click', async (e) => {
    const btn = e.target.closest('.scope');
    if (!btn) return;
    state.scope = btn.dataset.scope;
    document.querySelectorAll('.scope').forEach((b) => b.classList.toggle('active', b === btn));
    await loadInstruments();
  });

  $('btnRefresh').onclick = () => {
    loadTickers(); loadKline(true); loadPositions(); loadHistory(); loadSignals(); loadBackfill();
  };

  // 指标显示开关（MA / BOLL / 成交量）
  $('indGroup').addEventListener('click', (e) => {
    const btn = e.target.closest('.ind');
    if (!btn) return;
    const key = btn.dataset.ind;
    state.indVisible[key] = !state.indVisible[key];
    btn.classList.toggle('active', state.indVisible[key]);
    applyIndVisibility();
  });

  // 手动往前翻一页（不想滚动时用）
  $('btnOlder').onclick = () => loadOlder();

  $('btnBackfill').onclick = async () => {
    if (!state.curInst) return;
    $('bfInfo').textContent = '正在回补 ' + state.curInst + ' ' + state.curBar + ' …';
    try {
      const r = await api('/api/backfill', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ inst: state.curInst, bar: state.curBar }),
      });
      $('bfInfo').textContent = r.enqueued ? '已加入回补队列（队列 ' + r.queueLen + '）' : '该任务已在队列中';
      pollBackfillUntilDone();
    } catch (e) {
      $('bfInfo').textContent = '回补失败：' + e.message;
    }
  };

  // 表格里的合约名可点：直接切到那个合约的 K 线图。
  // 绑在 document 上做委托，持仓 / 历史 / 交易明细 / 信号四张表共用一套逻辑，
  // 以后再加表格不用重复绑。
  //
  // 为什么带 stopPropagation：下方表格是每 10 秒整表 innerHTML 重建的，
  // 让事件继续冒泡有几率撞上下一次重建，导致切换被吞掉。
  document.addEventListener('click', (e) => {
    const a = e.target.closest('a.inst-link');
    if (!a) return;
    e.preventDefault();
    e.stopPropagation();
    const inst = a.dataset.inst;
    if (!inst) {
      // data-inst 为空说明这行没拿到 instId，明确报出来而不是静默无反应
      $('chartHint').textContent = '该行缺少合约代码，无法切换';
      return;
    }
    // 先同步把标题切过去，网络部分失败也不影响「已经切了」这件事被看见
    state.curInst = inst;
    try { renderChartHead(); } catch (err) { /* 标题渲染失败不阻断 */ }
    jumpToInst(inst, a.dataset.bar).catch((err) => {
      console.error('切换合约失败', err);
      $('chartHint').textContent = '切换合约失败：' + ((err && err.message) || err);
    });
  });

  // 交易明细的动作过滤（全部 / 买入 / 加仓 / 平仓）。
  // 过滤在服务端做（/api/events?kind=...）—— 在前端筛只会筛「当前这一页」，
  // 用户会以为「加仓一共就这几笔」。
  const evF = $('evFilter');
  if (evF) evF.addEventListener('click', (e) => {
    const b = e.target.closest('.chip');
    if (!b) return;
    state.evKind = b.dataset.kind || '';
    evF.querySelectorAll('.chip').forEach((c) => c.classList.toggle('on', c === b));
    pgState('events').page = 1;
    loadEvents().catch(() => {});
  });

  $('tabs').addEventListener('click', (e) => {
    const btn = e.target.closest('.tab');
    if (!btn) return;
    document.querySelectorAll('.tab').forEach((t) => t.classList.remove('active'));
    btn.classList.add('active');
    const name = btn.dataset.tab;
    ['positions', 'history', 'events', 'signals', 'backfill', 'pnl'].forEach((n) => {
      const el = $('tab' + n[0].toUpperCase() + n.slice(1));
      if (el) el.classList.toggle('hidden', n !== name);
    });
    if (name === 'pnl') { loadPnl(); state.pnlChart && state.pnlChart.applyOptions({ width: $('pnlChart').clientWidth, height: $('pnlChart').clientHeight }); }
    if (name === 'backfill') loadBackfill();
    // 交易明细：切过来时立刻拉一次，不用等下一个轮询周期
    if (name === 'events') loadEvents().catch(() => {});
  });

  window.addEventListener('resize', () => {
    if (state.pnlChart) state.pnlChart.applyOptions({ width: $('pnlChart').clientWidth, height: $('pnlChart').clientHeight });
  });
}

function pollBackfillUntilDone() {
  if (state.bfPollTimer) clearInterval(state.bfPollTimer);
  let n = 0;
  state.bfPollTimer = setInterval(async () => {
    n++;
    await loadBackfill();
    const cov = (await api(`/api/kline?inst=${encodeURIComponent(state.curInst)}&bar=${state.curBar}&days=${state.days}&limit=1&auto=0`)).coverage;
    if (cov && cov.days >= state.days - 0.5) {
      clearInterval(state.bfPollTimer); state.bfPollTimer = null;
      await loadKline(true);
      $('bfInfo').textContent = `回补完成：覆盖 ${cov.days.toFixed(1)} 天`;
    } else if (n > 120) {
      clearInterval(state.bfPollTimer); state.bfPollTimer = null;
    }
  }, 5000);
}

/* ------------------------------------------------------------------ */
/* 启动                                                                */
/* ------------------------------------------------------------------ */

(async function boot() {
  initChart();
  initPnlChart();
  bindEvents();
  applyIndVisibility();
  try {
    await loadState();
    renderTimeframes();
    await loadInstruments();
    await loadTickers();
    await Promise.all([loadPositions(), loadHistory(), loadSignals(), loadBackfill(), loadAccount()]);
    if (!state.curInst && state.insts.length) await selectInst(state.insts[0].instId);
  } catch (e) {
    setConn(false, '接口异常：' + e.message);
  }

  // 每秒：收盘倒计时 + 用最新价刷新最后一根 K 线（图会「跳」起来）
  state.tickTimer = setInterval(tickLive, 1000);

  // 实时：行情 2 秒，账户 2 秒（顶栏权益/持仓/本金跟着跳），
  //       持仓 3 秒，历史/信号 10 秒，回补进度 20 秒
  setInterval(() => { loadTickers().catch(() => setConn(false, '行情中断')); }, 2000);
  setInterval(() => { loadAccount().catch(() => {}); }, 2000);
  setInterval(() => { loadPositions().catch(() => {}); }, 3000);
  // 历史/信号/交易明细 10 秒一刷（重绘时保持当前页码，不会把用户翻到的页弹回去）
  setInterval(() => {
    loadHistory().catch(() => {});
    loadSignals().catch(() => {});
    loadEvents().catch(() => {});
  }, 10000);
  setInterval(() => {
    loadBackfill().catch(() => {});
    loadState().catch(() => {});
    // 权益曲线也实时刷新：只在面板开着的时候拉，省得白跑
    const pnlTab = $('tabPnl');
    if (pnlTab && !pnlTab.classList.contains('hidden')) loadPnl().catch(() => {});
  }, 10000);

  // 页面重新可见时立刻补一次数据（后台标签页会被浏览器节流）
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) {
      loadTickers().catch(() => {});
      loadAccount().catch(() => {});
      loadKline(false).catch(() => {});
    }
  });
})();
