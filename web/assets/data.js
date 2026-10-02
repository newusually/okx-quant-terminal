/* ================================================================
 * data.js —— 数据层：K线分页滚动 / 实时刷新 / 收盘倒计时
 * ================================================================ */
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
  const left = state.hasMore
    ? `向左滚动加载更早（每页 ${KLINE_PAGE} 根）`
    : '已到最早';
  const right = state.noMoreNew ? ' · 已到最新' : '';
  el.textContent = `已加载 ${n} 根 · ${left}${right}` + mk;
}

// onScroll 滚到左/右边缘附近就预加载下一页（七期：左右双向）
//
// scrollGuard：刚 fitContent / 刚往前插完数据时会触发一次可视区回调，
// 那一次不能当成「用户拖动」——否则一打开就把所有历史页全拉下来了。
function onScroll() {
  if (Date.now() < (state.scrollGuardUntil || 0)) return;
  const lr = state.chart.timeScale().getVisibleLogicalRange();
  if (!lr) return;
  if (state.hasMore && !state.loadingOlder && lr.from <= PAGE_TRIGGER) loadOlder();
  if (!state.noMoreNew && !state.loadingNewer &&
      lr.to >= state.klines.length - PAGE_TRIGGER) loadNewer();
}

let klineTimer = null;

async function selectInst(instId) {
  if (!instId) return;
  state.curInst = instId;
  // NQ 快捷入口的高亮跟着当前合约走
  const nqBtn = $('nqEntry');
  if (nqBtn) nqBtn.classList.toggle('on', isReadonlyInst(instId));
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
    state.noMoreNew = false;   // 七期：换合约/周期后右端重新可探
    resetMarkers();
  }

  // 先把标题刷成当前合约（本地数据，零延迟）：即使下面 /api/mark 挂了，
  // 用户也能看到「图已经切到这个合约了」，而不是标题停在旧合约上。
  renderChartHead();

  $('chartHint').textContent = '加载中…';
  let j;
  try {
    j = await api(`/api/mark?inst=${encodeURIComponent(inst)}&bar=${encodeURIComponent(bar)}` +
      `&days=${state.days}&limit=${KLINE_INIT}&_=${Date.now()}`);
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
    // ★ 十一期：先对齐尺寸再铺数据。切合约时图表头可能换行数变多，
    //   容器高度变了；不先对齐，铺完的那一帧用的是旧尺寸 → **图显示不完全**
    //   （右边/下面缺一条，或整幅被压扁），用户看到的正是这个。
    if (window.__syncChartSize) window.__syncChartSize();
    renderKline(tickSize, false);
  } else {
    setCandleFormat(tickSize);
    buildIdx();
    paintTail(j.__added || 3);
    paintMarkers();   // 新信号/新平仓会随时冒出来，标记也得跟着刷新
    renderLegend(state.klines[state.klines.length - 1] || null);
  }

  if (reset || state.lastKlineKey !== key) {
    // ★ 十一期：换成带左右留白的「完整可见」，不再用 fitContent()
    //
    //   fitContent() 会把可视区间压成 [0, 根数-1]，第一根 K 线的圆心
    //   正好落在面板左边缘 —— **左边那根被切掉一半**，时间轴第一个标签
    //   也只剩半截（实测看到的是残缺的「:30」）。用户说的
    //   「K 线图显示不完全」就是这个：图是在，但最左边的 K 线缺了一块。
    //
    //   改成显式给可视区间：左边留 2 根空位、右边留 6 根空位，
    //   所有 K 线完整落在面板里，两侧各有一段呼吸空间。
    //   右侧留白同时给「最新价」标签和画图工具的延伸区让出地方。
    const n = state.klines.length;
    if (n > 0) {
      state.chart.timeScale().setVisibleLogicalRange({ from: -2, to: n - 1 + 6 });
    }
    state.scrollGuardUntil = Date.now() + 1200;   // 改可视区也会触发回调，先压住
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

    const mark = p.markPx || p.last || 0;
    // 距止损：标记价离**程序止损线**（exit.stop_loss_pct，当前 -300%）还有多远。
    //
    // ★ 七期更正：这列原来叫「距爆仓」，显示逐仓公式估算的强平价距离
    //   （entry×(1−1/lev+0.5%)，20x 下恒在 4.5% 附近）。但本系统是**全仓 cross**：
    //   OKX 实际强平看的是整个账户 adjEq 跌破维持保证金 mmr，单仓跌 4.5% 根本
    //   不会爆（账户 32.6U 权益 backing ~10U 敞口，最坏价格归零也只亏 10U）。
    //   那个数字纯属吓人，废弃。
    //
    //   两个概念别混：
    //     · 交易所强平线 —— OKX 按杠杆定的规则，程序改不了、关不掉（约 -4.5%@20x）；
    //       全仓模式下由账户整体权益决定，账户级巡检另有专门输出。
    //     · 程序止损线 —— exit.stop_loss_pct（当前 300 = -300% ROI），是我们自己
    //       的市价平仓线。20x 下它对应价格 -15%，落在负价格区之外的部分物理上到不了。
    const slPx = p.stopLossPx || 0;
    let distHtml = '<span class="muted">--</span>';
    let slPxHtml = '<span class="muted">--</span>';
    if (slPx !== 0) {
      if (slPx > 0) {
        slPxHtml = `<span class="muted">${fmtPrice(slPx)}</span>`;
      } else {
        // 做多且止损深于 -100% ROI（当前 -300%）→ 止损价落在负价格区 = 永远到不了
        slPxHtml = '<span class="muted" title="止损深于 -100%，对应价格已为负 → 物理上不可达">不可达</span>';
      }
      if (mark > 0) {
        const dist = Math.abs(mark - slPx) / mark * 100;
        distHtml = `<span class="muted" title="距程序止损线（-300%）。交易所强平线由 OKX 按杠杆决定，全仓模式下看账户整体权益，单仓强平价不适用"><b>${dist.toFixed(0)}%</b></span>`;
      }
    }

    return `<tr>
      <td><a class="inst-link" href="#" data-inst="${esc(p.instId)}" title="点开 ${esc(p.instId)} 的 K 线图">${esc(p.name || p.instId)}</a></td>
      <td><span class="tag-pill pill-${dir}">${dir === 'long' ? '多' : '空'}</span></td>
      <td>${fmtNum(p.sz, 0)}</td>
      <td>${fmtPrice(p.entryPx)}</td>
      <td>${fmtPrice(mark)}</td>
      <td>${slPxHtml}</td>
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

  // ★ 十一期：顶栏「持仓金额」★
  // 用户原话：「持仓多少钱 右上角不显示 要求显示」。
  // 主数字给**保证金合计**（自己真金白银押进去多少），小字给**名义价值合计**
  // （杠杆放大后的仓位规模）。两个都是从这份 rows 现算的 ——
  // 和下面表格里每一行显示的是同一份真实数据，不存在"顶栏一个口径、表格一个口径"。
  // ★ 十四期：后端名义价值已改真实口径（张数×面值×最新价，对账 OKX
  //   notionalUsd 误差 <1%；旧口径 margin×leverage 会虚高 20%，且对
  //   「最高 10x」的合约虚高一倍）。
  const sumMargin = rows.reduce((s, p) => s + (Number(p.margin) || 0), 0);
  const sumNotional = rows.reduce((s, p) => s + (Number(p.notional) || 0), 0);
  $('stPosAmt').textContent = fmtNum(sumMargin, 2) + ' U';
  $('stPosAmt').className = sumMargin > 0 ? 'accent' : '';
  $('stPosAmtSub').textContent = '名义 ' + fmtVol(sumNotional) + ' U';

  // 持仓变了 → 均价线跟着重画（renderKline 里也调一次，负责换合约的场景）
  try { paintEntryLines(); } catch (e) { console.warn('均价线刷新失败', e); }
}

