/* app.js —— 客户端逻辑：AJAX 拉数据 + TradingView 图表 + 实时刷新 */

const $ = (id) => document.getElementById(id);

const state = {
  insts: [],
  tickers: {},          // instId -> ticker
  curInst: '',
  curBar: '15m',
  days: 30,
  bars: ['1m', '3m', '5m', '15m', '1H', '4H'],
  marginText: '',
  scope: 'tradeable',   // tradeable | excluded | all —— 合约列表只看哪种
  universe: null,       // 准入统计 {total,kept,dropped,byReason}
  chart: null, candle: null, volume: null, ma7: null, ma25: null, ma99: null,
  pnlChart: null, pnlLine: null,
  bfPollTimer: null,
  lastKlineKey: '',
};

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
    },
    grid: {
      vertLines: { color: '#181d23' },
      horzLines: { color: '#181d23' },
    },
    rightPriceScale: { borderColor: '#262b31', scaleMargins: { top: 0.06, bottom: 0.26 } },
    timeScale: { borderColor: '#262b31', timeVisible: true, secondsVisible: false, rightOffset: 6 },
    crosshair: {
      mode: LightweightCharts.CrosshairMode.Normal,
      vertLine: { color: '#4a515c', width: 1, style: 2, labelBackgroundColor: '#2b3139' },
      horzLine: { color: '#4a515c', width: 1, style: 2, labelBackgroundColor: '#2b3139' },
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
    upColor: '#0ecb81', downColor: '#f6465d',
    borderUpColor: '#0ecb81', borderDownColor: '#f6465d',
    wickUpColor: '#0ecb81', wickDownColor: '#f6465d',
    priceLineVisible: true, lastValueVisible: true,
  });

  state.volume = state.chart.addHistogramSeries({
    priceScaleId: 'vol',
    priceFormat: { type: 'volume' },
    lastValueVisible: false, priceLineVisible: false,
  });
  state.chart.priceScale('vol').applyOptions({ scaleMargins: { top: 0.78, bottom: 0 } });

  state.ma7 = state.chart.addLineSeries({ color: '#fcd535', lineWidth: 1, priceLineVisible: false, lastValueVisible: false, title: 'MA7' });
  state.ma25 = state.chart.addLineSeries({ color: '#5b8def', lineWidth: 1, priceLineVisible: false, lastValueVisible: false, title: 'MA25' });
  state.ma99 = state.chart.addLineSeries({ color: '#c26bf0', lineWidth: 1, priceLineVisible: false, lastValueVisible: false, title: 'MA99' });

  new ResizeObserver(() => {
    state.chart.applyOptions({ width: el.clientWidth, height: el.clientHeight });
  }).observe(el);
  state.chart.applyOptions({ width: el.clientWidth, height: el.clientHeight });

  // 十字光标联动 OHLC 显示
  state.chart.subscribeCrosshairMove((param) => {
    if (!param || !param.time || !param.seriesData) return;
    const c = param.seriesData.get(state.candle);
    if (!c) return;
    $('pairOhlc').textContent =
      `开 ${fmtPrice(c.open)}  高 ${fmtPrice(c.high)}  低 ${fmtPrice(c.low)}  收 ${fmtPrice(c.close)}`;
  });
}

function initPnlChart() {
  const el = $('pnlChart');
  if (!el) return;
  state.pnlChart = LightweightCharts.createChart(el, {
    layout: { background: { type: 'solid', color: '#161a1e' }, textColor: '#b7bdc6', fontSize: 11 },
    grid: { vertLines: { color: '#1d2228' }, horzLines: { color: '#1d2228' } },
    rightPriceScale: { borderColor: '#262b31' },
    timeScale: { borderColor: '#262b31', timeVisible: true },
  });
  state.pnlLine = state.pnlChart.addAreaSeries({
    lineColor: '#fcd535', topColor: 'rgba(252,213,53,.35)', bottomColor: 'rgba(252,213,53,0)',
    lineWidth: 2,
  });
  new ResizeObserver(() => {
    state.pnlChart.applyOptions({ width: el.clientWidth, height: el.clientHeight });
  }).observe(el);
  state.pnlChart.applyOptions({ width: el.clientWidth, height: el.clientHeight });
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
    ['下单口径', (state.marginText || '--') + (it.marginUsdt > state.entryMargin ? '（放大到 1 张）' : '')],
  ];
  $('instInfo').innerHTML = rows
    .map(([k, v]) => `<span class="k">${esc(k)}</span><span class="v">${esc(v)}</span>`).join('');
}

function renderServiceInfo(st) {
  if (!st) return;
  const info = [
    ['每笔保证金', state.marginText || '--'],
    ['策略周期', (st.strategy && st.strategy.bar) || '--'],
    ['扫描周期', ((st.strategy && st.strategy.bars_enabled) || []).join(' / ') || '--'],
    ['止盈', (st.strategy && st.strategy.exit && st.strategy.exit.take_profit_pct) + '%'],
    ['共振阈值', String((st.strategy && st.strategy.score_threshold) || '--') + ' / 8'],
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

function renderStats(st) {
  const s = st.stats || {};
  $('stEquity').textContent = s.klineRows ? '--' : '--';
  $('stPos').textContent = s.posCount || 0;
  $('stTodayPnl').textContent = fmtNum(s.todayPnl, 2);
  $('stTodayPnl').className = cls(s.todayPnl);
  $('stPnl').textContent = fmtNum(s.pnlTotal, 2);
  $('stPnl').className = cls(s.pnlTotal);
  $('stWin').textContent = s.tradesTotal > 0 ? s.winRate.toFixed(1) + '%' : '--';
  $('stKline').textContent = fmtNum(s.klineRows, 0);
  $('stInst').textContent = fmtNum(s.instCount, 0);
  $('stMargin').textContent = state.marginText || '--';
  $('footRight').textContent = `数据库：${st.dbPath || '--'} · 服务器时间 ${s.serverTime || '--'}`;
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
  state.bars = st.bars || state.bars;
  state.days = st.backfillDays || 30;
  state.marginText = st.marginText || '';
  state.entryMargin = (((st.strategy || {}).entry || {}).margin_usdt) || 0.1;
  state.maxMargin = (((st.strategy || {}).entry || {}).max_margin_usdt) || 0.5;
  renderTimeframes();
  renderStats(st);
  renderServiceInfo(st);
  setConn(true, '已连接');
  $('footLeft').textContent =
    `OKX 全合约量化终端 · ${st.version} · 数据覆盖 ${state.days} 天 · ${state.marginText}`;
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

async function loadTickers() {
  const j = await api('/api/tickers');
  const map = {};
  (j.list || []).forEach((t) => { map[t.instId] = t; });
  state.tickers = map;
  renderTickerTape();
  renderInstList();
  renderInstInfo();
  const t = map[state.curInst];
  if (t) {
    $('pairPrice').textContent = fmtPrice(t.last);
    const cg = $('pairChg');
    cg.textContent = fmtPct(t.chgPct);
    cg.className = 'chg ' + cls(t.chgPct);
  }
}

let klineTimer = null;

async function selectInst(instId) {
  state.curInst = instId;
  renderInstList();
  renderInstInfo();
  await loadKline(true);
}

async function loadKline(reset) {
  if (!state.curInst) return;
  const inst = state.curInst, bar = state.curBar;
  const key = inst + '|' + bar;
  if (reset) state.lastKlineKey = '';

  $('chartHint').textContent = '加载中…';
  let j;
  try {
    j = await api(`/api/mark?inst=${encodeURIComponent(inst)}&bar=${encodeURIComponent(bar)}&days=${state.days}&_=${Date.now()}`);
  } catch (e) {
    $('chartHint').textContent = '加载失败：' + e.message;
    return;
  }

  const list = j.kline || [];
  const cov = j.coverage || {};
  $('pairName').textContent = (state.tickers[inst] && state.tickers[inst].name) || inst;
  $('pairInst').textContent = inst + ' · ' + bar;

  const tickSize = (state.insts.find((x) => x.instId === inst) || {}).tickSz || 0.0001;
  const prec = Math.max(0, Math.min(8, Math.ceil(-Math.log10(tickSize))));

  state.candle.applyOptions({ priceFormat: { type: 'price', precision: prec, minMove: tickSize } });
  state.candle.setData(list.map((k) => ({
    time: Math.floor(k.ts / 1000), open: k.o, high: k.h, low: k.l, close: k.c,
  })));
  state.volume.setData(list.map((k) => ({
    time: Math.floor(k.ts / 1000), value: k.v,
    color: k.c >= k.o ? 'rgba(14,203,129,.45)' : 'rgba(246,70,93,.45)',
  })));

  const toLine = (arr) => arr.map((p) => ({ time: Math.floor(p.ts / 1000), value: p.v }));
  state.ma7.setData(toLine(j.ma7 || []));
  state.ma25.setData(toLine(j.ma25 || []));
  state.ma99.setData(toLine(j.ma99 || []));

  if (reset || state.lastKlineKey !== key) {
    state.chart.timeScale().fitContent();
    state.lastKlineKey = key;
  }

  $('chartHint').textContent = `${list.length} 根 ${bar} K线`;
  $('covInfo').textContent =
    `覆盖 ${(cov.days || 0).toFixed(1)} 天 / ${fmtNum(cov.count || 0, 0)} 根` +
    (cov.minTs ? `（${fmtShort(cov.minTs)} → ${fmtShort(cov.maxTs)}）` : '');

  const chg = (state.tickers[inst] || {}).chgPct;
  const cg = $('pairChg');
  cg.textContent = fmtPct(chg);
  cg.className = 'chg ' + cls(chg);

  scheduleKlineRefresh();
}

function scheduleKlineRefresh() {
  if (klineTimer) clearInterval(klineTimer);
  const ms = { '1m': 20000, '3m': 30000, '5m': 30000, '15m': 60000, '1H': 120000, '4H': 300000 }[state.curBar] || 60000;
  klineTimer = setInterval(() => loadKline(false), ms);
}

/* ------------------------------------------------------------------ */
/* 表格                                                                */
/* ------------------------------------------------------------------ */

async function loadPositions() {
  const j = await api('/api/positions');
  const rows = j.list || [];
  $('badgePos').textContent = rows.length;
  const tb = $('tbPositions');
  if (!rows.length) {
    tb.innerHTML = '<tr><td colspan="14" class="empty">暂无持仓</td></tr>';
    return;
  }
  tb.innerHTML = rows.map((p) => {
    const dir = (p.side || 'buy').toLowerCase() === 'sell' ? 'short' : 'long';
    return `<tr>
      <td title="${esc(p.instId)}">${esc(p.name || p.instId)}</td>
      <td><span class="tag-pill pill-${dir}">${dir === 'long' ? '多' : '空'}</span></td>
      <td>${fmtNum(p.sz, 0)}</td>
      <td>${fmtPrice(p.entryPx)}</td>
      <td>${fmtPrice(p.markPx)}</td>
      <td class="muted">${fmtPrice(p.liqPx)}</td>
      <td>${fmtNum(p.margin, 2)}</td>
      <td>${p.leverage}x</td>
      <td>${fmtNum(p.notional, 2)}</td>
      <td class="${cls(p.upl)}"><b>${fmtNum(p.upl, 4)}</b></td>
      <td class="${cls(p.uplPct)}">${fmtPct(p.uplPct)}</td>
      <td class="muted">${fmtPrice(p.takeProfitPx)}</td>
      <td>${(p.holdMin / 60).toFixed(1)}h</td>
      <td>${p.score ? p.score + '/8' : '--'}</td>
    </tr>`;
  }).join('');
}

async function loadHistory() {
  const j = await api('/api/history?limit=200');
  const rows = j.list || [];
  $('badgeHis').textContent = rows.length;
  const tb = $('tbHistory');
  if (!rows.length) {
    tb.innerHTML = '<tr><td colspan="12" class="empty">暂无平仓记录</td></tr>';
    return;
  }
  tb.innerHTML = rows.map((t) => {
    const dir = (t.side || 'buy').toLowerCase() === 'sell' ? 'short' : 'long';
    return `<tr>
      <td title="${esc(t.instId)}">${esc(t.name || t.instId)}</td>
      <td><span class="tag-pill pill-${dir}">${dir === 'long' ? '多' : '空'}</span></td>
      <td>${fmtNum(t.sz, 0)}</td>
      <td>${fmtPrice(t.entryPx)}</td>
      <td>${fmtPrice(t.exitPx)}</td>
      <td>${fmtNum(t.margin, 2)}</td>
      <td>${t.leverage}x</td>
      <td class="${cls(t.pnl)}"><b>${fmtNum(t.pnl, 4)}</b></td>
      <td class="${cls(t.pnlPct)}">${fmtPct(t.pnlPct)}</td>
      <td class="muted" title="${esc(t.reason)}">${esc((t.reason || '').slice(0, 18))}</td>
      <td class="muted">${fmtTime(t.openTs)}</td>
      <td class="muted">${fmtTime(t.closeTs)}</td>
    </tr>`;
  }).join('');
}

async function loadSignals() {
  const j = await api('/api/signals?limit=100');
  const rows = j.list || [];
  $('badgeSig').textContent = rows.length;
  const tb = $('tbSignals');
  if (!rows.length) {
    tb.innerHTML = '<tr><td colspan="10" class="empty">暂无信号</td></tr>';
    return;
  }
  const actedText = (a) => a === 1 ? '<span class="tag-pill pill-ok">已下单</span>'
    : (a === 2 ? '<span class="tag-pill pill-err">被拦</span>' : '<span class="tag-pill">仅记录</span>');
  tb.innerHTML = rows.map((s) => `<tr>
      <td>${esc(s.name || s.instId)}</td>
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
}

async function loadBackfill() {
  const j = await api('/api/backfill');
  const jobs = j.jobs || [], covs = j.coverage || [];
  $('badgeBf').textContent = covs.length;
  $('bfInfo').textContent = `回补队列 ${j.queueLen} · 已回补 ${covs.length} 组（目标 ${j.days} 天）`;

  const tb = $('tbBackfill');
  const covMap = {};
  covs.forEach((c) => { covMap[c.instId + '|' + c.bar] = c; });
  if (!jobs.length && !covs.length) {
    tb.innerHTML = '<tr><td colspan="8" class="empty">暂无回补任务</td></tr>';
    return;
  }
  const src = jobs.length ? jobs : covs.map((c) => ({
    instId: c.instId, bar: c.bar, rows: c.count, fromTs: c.minTs, toTs: c.maxTs,
    status: c.days >= j.days - 0.5 ? 'done' : 'pending',
    msg: `覆盖 ${c.days.toFixed(1)} 天`, name: c.name,
  }));
  tb.innerHTML = src.map((r) => {
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
}

async function loadPnl() {
  const j = await api('/api/pnl');
  const rows = j.list || [];
  const empty = $('pnlEmpty');
  if (!rows.length) {
    empty.classList.remove('hidden');
    return;
  }
  empty.classList.add('hidden');
  state.pnlLine.setData(rows.map((p) => ({ time: Math.floor(p.ts / 1000), value: p.totalEq || 0 })));
  state.pnlChart.timeScale().fitContent();
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
    loadTickers(); loadKline(false); loadPositions(); loadHistory(); loadSignals(); loadBackfill();
  };

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

  $('tabs').addEventListener('click', (e) => {
    const btn = e.target.closest('.tab');
    if (!btn) return;
    document.querySelectorAll('.tab').forEach((t) => t.classList.remove('active'));
    btn.classList.add('active');
    const name = btn.dataset.tab;
    ['positions', 'history', 'signals', 'backfill', 'pnl'].forEach((n) => {
      const el = $('tab' + n[0].toUpperCase() + n.slice(1));
      if (el) el.classList.toggle('hidden', n !== name);
    });
    if (name === 'pnl') { loadPnl(); state.pnlChart && state.pnlChart.applyOptions({ width: $('pnlChart').clientWidth, height: $('pnlChart').clientHeight }); }
    if (name === 'backfill') loadBackfill();
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
  try {
    await loadState();
    renderTimeframes();
    await loadInstruments();
    await loadTickers();
    await Promise.all([loadPositions(), loadHistory(), loadSignals(), loadBackfill()]);
    if (!state.curInst && state.insts.length) await selectInst(state.insts[0].instId);
  } catch (e) {
    setConn(false, '接口异常：' + e.message);
  }

  // 实时：行情 + 持仓 3 秒，历史/信号 15 秒
  setInterval(() => { loadTickers().catch(() => setConn(false, '行情中断')); }, 3000);
  setInterval(() => { loadPositions().catch(() => {}); }, 3000);
  setInterval(() => { loadHistory().catch(() => {}); loadSignals().catch(() => {}); }, 15000);
  setInterval(() => { loadBackfill().catch(() => {}); loadState().catch(() => {}); }, 30000);
})();
