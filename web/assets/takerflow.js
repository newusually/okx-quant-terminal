/* ================================================================
 * takerflow.js —— taker 买卖流向面板（二十二期）
 *
 * 位置：合约列表右侧、K 线图左侧（用户口径「放在K线图左边,合约列表面板右边」）
 *
 * 四列：
 *   1. 时间              —— 5 分钟切片
 *   2. 买:卖             —— 全池主动买量 ÷ 主动卖量（整个池子加总）
 *   3. ETH→下根          —— 该切片之后一根 5m 的 ETH 涨跌幅
 *   4. 涨幅王 本根→下根   —— 该切片内涨幅最高的合约 + 它当根涨幅 → 下一根涨跌幅
 *
 * ★ 第 4 列口径（2026-10-03 二次变更）：不是成交量最大，是**涨幅最高**。
 *   「ETH 这个 5 分钟涨 2% 比其他合约都高 → 显示 ETH，然后显示它下个 5 分钟的涨跌幅」。
 *
 * ★ 第 3、4 列的时间轴比第 2 列晚一根（前瞻对齐）：
 *   面板的用途是「看这 5 分钟，接下来一根发生了什么」。
 *   表头用「→下根」明确标出来，否则会被误读成数据错位。
 *
 * ★ 涨跌配色与全站一致（--up 绿 = 涨、--down 红 = 跌，加密口径，
 *   与顶栏/合约列表/持仓表完全同一套变量 —— 单独改这一处反而会造成
 *   同屏两套红绿含义）。
 *
 * ★ 分页：页码可见（renderPager 复用），客户端切页（数据一次拉够，
 *   pgSlice 不传 total 就走本地切片）。默认直接拉满 30 天（用户口径
 *   「30天的数据要全部带上」）。
 *
 * ★ 函数一律带 tk 前缀（2026-10-03 踩坑）：
 *   core.js 里已有全局 fmtVol / fmtPct，本文件原来重名 → 整个脚本
 *   SyntaxError「Identifier 'fmtVol' has already been declared」，
 *   而浏览器只把它记进 console，页面上表现就是「面板一直显示加载中」。
 *   这类「脚本静默不执行」在本项目是老毛病，前缀隔离最省心。
 * ================================================================ */

// taker 面板状态
const tkState = {
  rows: [],       // 已加载的行（降序，最新在前）
  limit: 288 * 30, // ★ 用户口径「30天的数据要全部带上」：默认直接拉满 30 天，
                  //   不再先 10 天再让用户点「加载全部」（那版面板永远只有 10 天）。
  maxLimit: 288 * 30,
  loading: false,
  loadingAt: 0,   // 本轮开始时间（用于卡死自解，见 loadTakerFlow 注释）
  summary: null,
  poolSize: 0,
  note: '',
  allLoaded: true, // 默认已拉满，「加载全部」按钮自然进 disabled 态
};

// tkFmtVol 量级格式化：12345678 → "1234.6万"；小于 1 万直接给 2 位小数
function tkFmtVol(v) {
  if (!isFinite(v) || v <= 0) return '0';
  const a = Math.abs(v);
  if (a >= 1e8) return (v / 1e8).toFixed(2) + '亿';
  if (a >= 1e4) return (v / 1e4).toFixed(1) + '万';
  return v.toFixed(2);
}

// tkFmtRatio 买卖比：保留 2 位，"N:1"
function tkFmtRatio(r) {
  if (!isFinite(r) || r <= 0) return '--';
  return r.toFixed(2) + ':1';
}

// tkFmtTime 时间戳(ms) → "MM-DD HH:MM"
function tkFmtTime(ts) {
  const d = new Date(Number(ts));
  const p = (n) => String(n).padStart(2, '0');
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

// tkFmtPct 涨跌幅：带符号 + 2 位；ok=false 表示下一根还没生成 → 「—」
function tkFmtPct(v, ok) {
  if (!ok || !isFinite(v)) return '—';
  return (v >= 0 ? '+' : '') + v.toFixed(2) + '%';
}

// tkPctClass 涨跌配色类（与全站 cls 同逻辑）
function tkPctClass(v, ok) {
  if (!ok || !isFinite(v)) return 'flat';
  return v > 0 ? 'up' : (v < 0 ? 'down' : 'flat');
}

// tkShortInst 合约名瘦身：BTC-USDT-SWAP → BTC
function tkShortInst(inst) {
  if (!inst) return '—';
  return String(inst).replace('-USDT-SWAP', '').replace('-USDT', '');
}

// loadTakerFlow 拉数据并渲染
//
// ★ 2026-10-03 踩坑：boot 阶段并发请求太多（本页一次要发 state/tickers/
//   account/positions/... 十几条），浏览器的连接池会被打满，返回
//   net::ERR_INSUFFICIENT_RESOURCES。此时 fetch 的 promise **既不 resolve
//   也不 reject**（socket 卡死），于是 tkState.loading 永远停在 true，
//   后面每 30 秒的轮询全被 `if (tkState.loading) return` 挡掉 ——
//   现象就是面板一直显示「加载中…」，页面还不报错，极难查。
//   对策：① 加 AbortController 超时，卡死的请求 12 秒必定失败；
//        ② loading 用「时间戳」而不是布尔，超过 60 秒自动放行下一轮；
//        ③ 失败时把错误写进面板，别让用户看到永远的「加载中」。
async function loadTakerFlow(reset) {
  const now = Date.now();
  // 布尔锁 + 超时自解：正常 1 秒内就结束，卡住的话 60 秒后允许重试
  if (tkState.loading) {
    if (now - (tkState.loadingAt || 0) < 60000) return;
    console.warn('loadTakerFlow 上一轮疑似卡死，强制重试');
  }
  tkState.loading = true;
  tkState.loadingAt = now;
  if (reset) tkState.limit = tkState.maxLimit;   // ★ 首屏就拉满 30 天（原来这里写死 2880 把默认值打回 10 天）

  // 12 秒超时（全量 30 天约 1.3MB，慢网络也够；卡死则必定中断）
  const ctl = typeof AbortController !== 'undefined' ? new AbortController() : null;
  const timer = ctl ? setTimeout(() => ctl.abort(), 12000) : null;

  try {
    const j = await api(`/api/takerflow?limit=${tkState.limit}&days=30`,
      ctl ? { signal: ctl.signal } : undefined);
    tkState.rows = j.rows || [];
    tkState.summary = j.summary || null;
    tkState.poolSize = j.poolSize || 0;
    tkState.note = j.note || '';
    tkState.allLoaded = tkState.limit >= tkState.maxLimit;
    renderTakerFlow();
  } catch (e) {
    const tb = $('tbTaker');
    const msg = (e && e.name === 'AbortError') ? '请求超时（12 秒）' : ((e && e.message) || e);
    if (tb) tb.innerHTML = `<tr><td colspan="4" class="empty">加载失败：${msg}</td></tr>`;
    console.warn('loadTakerFlow 失败', e);
  } finally {
    if (timer) clearTimeout(timer);
    tkState.loading = false;
    tkState.loadingAt = 0;
  }
}

// renderTakerFlow 渲染汇总条 + 当前页表格 + 可见页码
function renderTakerFlow() {
  const tb = $('tbTaker');
  if (!tb) return;

  // ---- 汇总条（用户明确要「都加起来，总和是多少」）----
  const s = tkState.summary || {};
  const ratioEl = $('tkSumRatio');
  if (ratioEl) {
    const r = Number(s.ratio) || 0;
    ratioEl.textContent = r > 0 ? tkFmtRatio(r) : '--';
    ratioEl.style.color = r > 1 ? 'var(--up)' : (r > 0 && r < 1 ? 'var(--down)' : '');
  }
  if ($('tkSumBuy')) $('tkSumBuy').textContent = tkFmtVol(Number(s.buyTotal) || 0);
  if ($('tkSumSell')) $('tkSumSell').textContent = tkFmtVol(Number(s.sellTotal) || 0);
  if ($('tkSumMeta')) {
    $('tkSumMeta').textContent = `${s.slices || 0} 个切片 · ${tkState.poolSize} 个合约池`;
  }
  if ($('tkNote')) $('tkNote').textContent = tkState.note || '';
  if ($('tkFootInfo')) {
    $('tkFootInfo').textContent = tkState.allLoaded
      ? `已载全部 ${tkState.rows.length} 根（30 天）`
      : `已载 ${tkState.rows.length} 根（约 ${(tkState.rows.length / 288).toFixed(0)} 天）`;
  }
  const allBtn = $('tkAll');
  if (allBtn) {
    // ★ 默认就拉满 30 天后这个按钮没用了 —— 直接藏掉，别占一行
    allBtn.classList.toggle('hidden', !!tkState.allLoaded);
    allBtn.disabled = tkState.allLoaded;
    allBtn.textContent = tkState.allLoaded ? '已载全部' : '加载全部 30 天';
  }

  // ---- 表格（客户端分页，页码可见）----
  const { slice, total } = pgSlice('taker', tkState.rows);
  if (!total) {
    tb.innerHTML = '<tr><td colspan="4" class="empty">暂无数据（等待首铺完成）</td></tr>';
    renderPager('taker', 0, renderTakerFlow);
    return;
  }

  const buf = [];
  for (const r of slice) {
    const ratio = Number(r.ratio) || 0;
    const rCls = ratio > 1 ? 'buy' : (ratio > 0 && ratio < 1 ? 'sell' : '');
    buf.push('<tr>');
    buf.push(`<td class="tk-t">${tkFmtTime(r.ts)}</td>`);
    buf.push(`<td class="tk-r ${rCls}">${tkFmtRatio(ratio)}</td>`);
    buf.push(`<td class="${tkPctClass(r.ethNextPct, r.ethNextOk)}">${tkFmtPct(r.ethNextPct, r.ethNextOk)}</td>`);
    // 第 4 列：涨幅王 —— 合约名 + 当根涨幅（挑选依据）→ 下一根涨跌幅
    buf.push(`<td><span class="tk-inst" title="${r.topInst || ''}">${tkShortInst(r.topInst)}</span> ` +
      `<span class="tk-rise">${tkFmtPct(r.topRise, !!r.topInst)}</span>` +
      `<span class="tk-arrow">→</span>` +
      `<span class="${tkPctClass(r.topNextPct, r.topNextOk)}">${tkFmtPct(r.topNextPct, r.topNextOk)}</span></td>`);
    buf.push('</tr>');
  }
  tb.innerHTML = buf.join('');

  // 页码条（复用全站 renderPager；翻页回调 = 只重画表格，不重新拉数据）
  renderPager('taker', total, renderTakerFlow);
}

// bindTakerPanel 事件绑定（加载全部 + 首屏 + 轮询）
function bindTakerPanel() {
  const all = $('tkAll');
  if (all) {
    all.addEventListener('click', async () => {
      if (tkState.allLoaded || tkState.loading) return;
      tkState.limit = tkState.maxLimit;
      await loadTakerFlow(false);
    });
  }
  // ★ 首屏错峰加载：boot 那一刻全站在抢连接池，这时候去 fetch 十有八九
  //   撞上 ERR_INSUFFICIENT_RESOURCES。延后 1.5 秒等主数据先落地。
  setTimeout(() => {
    loadTakerFlow(true).catch(() => {});
    // 首屏若失败（网络抖动 / 连接池），5 秒和 15 秒各补一次。
    // ★ 必须同时检查 !loading：首屏请求卡死时要等 12 秒超时自解，
    //   盲补会被 60 秒 loading 守卫静默吞掉（2026-10-03 实测）。
    [5000, 15000].forEach((d) => setTimeout(() => {
      if (!tkState.rows.length && !tkState.loading) loadTakerFlow(true).catch(() => {});
    }, d));
  }, 1500);
  // 每 30 秒轮询（5m 切片 5 分钟才出一根新数据，30 秒足够快）
  setInterval(() => loadTakerFlow(false), 30000);
}
