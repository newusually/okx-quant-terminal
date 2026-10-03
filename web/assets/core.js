/* ================================================================
 * core.js —— 基础层：全局状态 / 分页 / 常量 / 工具函数
 * ★ 十五期：app.js 拆分为分层文件（逻辑零改动，只做物理切分）。
 *   加载顺序必须是第一个 —— 其他层都依赖这里的 $ / state / api。
 * ================================================================ */
/* app.js —— 客户端逻辑：AJAX 拉数据 + TradingView 图表 + 实时刷新 */

const $ = (id) => document.getElementById(id);

const state = {
  insts: [],
  tickers: {},          // instId -> ticker
  curInst: '',
  curBar: '5m',
  days: 30,
  // ★ 2026-10-02 四期：1m 下线；★ 2026-10-03 二十一期：15m 下线（用户口径
  //   「删除15分钟K线图数据」），只剩 3m/5m（选项卡由 /api/state 的
  //   bars 字段渲染，这里只是首屏兜底 —— 真源是 model.EnabledBars）。
  bars: ['3m', '5m'],
  marginText: '',
  scope: 'tradeable',   // tradeable | excluded | all —— 合约列表只看哪种
  sortKey: '',          // '' 默认 | 'chg' 涨幅降序 | 'vol' 成交额降序（十四期）
  universe: null,       // 准入统计 {total,kept,dropped,byReason}
  chart: null, candle: null, volume: null, ma7: null, ma25: null, ma99: null,
  bollUp: null, bollMid: null, bollLo: null,
  pnlChart: null, pnlLine: null,
  pnlData: [],          // 权益曲线的原始点（悬停时查浮盈/持仓数用）
  pnlFitted: false,     // 只在首次铺满视野，之后轮询不打断用户缩放
  positions: [],        // 当前持仓（历史列表里给「持仓中」的行补实时盈亏）
  bfPollTimer: null,
  lastKlineKey: '',

  // ---- ★ 十一期：持仓均价虚线（60% 透明度）----
  // 存已创建的 priceLine 句柄，换合约 / 持仓变化时先整组撤掉再重建。
  // 不这么做的话，lightweight-charts 的 priceLine 会越叠越多，
  // 旧合约的均价线留在新合约的图上 —— 那是**错误数据**，比没有更糟。
  entryLines: [],
  entryLineKey: '',     // 上次画线时的 instId+bar，变了才重建（省一点重绘）

  // ---- ★ 十一期：画图工具（趋势线 / 水平线 / 矩形 / 画笔 / 橡皮擦）----
  draw: {
    canvas: null, ctx: null,
    tool: null,          // null = 没在画图模式（叠加层不接收鼠标事件）
    color: '#ffd54f',
    items: [],           // 当前合约+周期的画痕，按创建顺序
    drawing: null,       // 正在画的那一条（拖动过程中）
    drag: false,
    lastMove: null,      // 画笔的上一采样点
    key: '',             // localStorage 的键：okxDraw:<inst>:<bar>
  },

  // ---- K 线分页：初始一页，向左滚动自动向前翻 ----
  klines: [],           // 已加载的 K 线（升序），翻页时不断往前拼
  ind: {},              // 指标序列 {ma7:[], ma25:[], ...}，同样按 ts 合并
  indMap: {},           // ts -> 值 的快查表（画图例用）
  kMap: new Map(),      // ts -> K 线

  // ---- ★ 二十二期：taker 买卖比 MACD 副图 ----
  // takerMacd 是**全市场**的一条序列（不是当前合约的），来自
  // /api/takermacd（读 taker_macd 预计算表，参数 12/26/60）。
  // 前端只做一件事：按 ts 对齐画在 VOL 下面的副图里。
  takerMacd: [],        // [{ts, src, dif, dea, hist}] 升序
  takerMacdMap: {},     // ts -> 该点，给图例取值用
  takerMacdParams: null, // {fast, slow, signal} 后端回报的参数，图例上要写
  takerMacdEdgeNew: 0,  // 上次拉取时 K 线的最右端 ts（没变就不重拉）
  takerMacdEdgeOld: 0,  // 上次拉取时 K 线的最左端 ts（翻页后要补历史）
  takerMacdHidden: false, // 非 5m 周期时置位，避免反复重算隐藏状态
  tmacdLoading: false,  // MACD 序列在途去重（卡死由 12s 超时自解）
  hasMore: false,       // 更早还有没有数据
  loadingOlder: false,  // 防止一次滚动触发多次翻页
  oldBurst: 0,          // 连续补页计数（拖太左时的串行续载，见 draw.js chainOlder）
  noMoreNew: false,     // 七期：右端已到最新（loadNewer 返回 0 根时置位）
  loadingNewer: false,  // 七期：右移翻页去重

  // ---- 显示开关 & 实时 ----
  indVisible: { ma: true, boll: true, vol: true, tmacd: true },
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

/* ★ NQ 只读板块 —— 二十一期已下线 ★
 * 2026-10-03 用户口径「取消NQ所有东西」：NQ-INDEX 数据/信号/按钮全部移除，
 * 后端同步（Dukascopy + Yahoo 盘中）也已摘除。这里把名单清空，
 * isReadonlyInst 恒 false —— 合成行情、高亮等 NQ 分支全部自然失效，
 * 相关兜底代码暂留不删，想恢复时把名字加回来即可。 */
const READONLY_INSTS = {};
const isReadonlyInst = (id) => Object.prototype.hasOwnProperty.call(READONLY_INSTS, id);

// 币安配色：涨绿跌红
const C_UP = '#0ecb81', C_DOWN = '#f6465d';
const C_MA7 = '#f0b90b', C_MA25 = '#e056fd', C_MA99 = '#4facfe';
const C_BOLL = '#5c6b7a', C_BOLL_MID = '#9aa4b2';

const KLINE_PAGE = 300;    // 一页多少根（七期：向左/向右每次翻 300 根）
const KLINE_INIT = 100;    // 七期：初始只加载最新 100 根，往左/往右滚才按页补
const PAGE_TRIGGER = 5;    // 可视区边界落到第几根之内就预加载下一页

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

