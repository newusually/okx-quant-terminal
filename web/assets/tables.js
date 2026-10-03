/* ================================================================
 * tables.js —— 表格层：持仓/历史/明细/信号/回补渲染 + 事件绑定
 * ================================================================ */
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

// setScope 切换合约列表范围（可交易 / 被排除 / 全部）
async function setScope(scope) {
  state.scope = scope;
  // ★ 只动范围按钮组（#scopeGroup）：十四期加了排序按钮组（.skey），
  //   不能用全局 '.scope' 一把抓，否则切范围会把排序高亮也冲掉
  document.querySelectorAll('#scopeGroup .scope').forEach((b) =>
    b.classList.toggle('active', b.dataset.scope === scope));
  await loadInstruments();
}

// openNQ —— 二十一期已下线（2026-10-03 用户口径「取消NQ所有东西 包括并且
// 删除NQ按钮 数据等页面还有信号」）。入口按钮与监听一并移除。

function bindEvents() {
  $('instSearch').addEventListener('input', renderInstList);

  // 合约列表范围切换：可交易 / 被排除 / 全部
  $('scopeGroup').addEventListener('click', async (e) => {
    const btn = e.target.closest('.scope');
    if (!btn) return;
    await setScope(btn.dataset.scope);
  });

  // ★ 十四期：列表排序（默认 / 涨幅 / 金额）
  $('sortGroup').addEventListener('click', (e) => {
    const btn = e.target.closest('.skey');
    if (!btn) return;
    state.sortKey = btn.dataset.sort || '';
    document.querySelectorAll('#sortGroup .skey').forEach((b) =>
      b.classList.toggle('active', b === btn));
    renderInstList();
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
    const name = btn.dataset.tab;
    // ★ 十五期：View Transitions 共享元素补间（用户点名要的功能）。
    //   浏览器对 .tables 区块拍旧/新两帧快照后自动补间 —— CSS 里配了
    //   view-transition-name: tables，只补间这一块，页面其余部分不闪。
    //   不支持的浏览器（Firefox 旧版）自动退回原来的瞬时切换。
    const activate = () => {
      document.querySelectorAll('.tab').forEach((t) => t.classList.remove('active'));
      btn.classList.add('active');
      ['positions', 'history', 'events', 'signals', 'backfill', 'pnl'].forEach((n) => {
        const el = $('tab' + n[0].toUpperCase() + n.slice(1));
        if (!el) return;
        // ★ 二十二期：被拖出成独立面板的 tab-body 不受这里的 hidden 管理 ——
        //   它已经住进自己的浮动窗口里了，切别的 tab 不能把它藏掉。
        if (el.dataset.detached === '1') return;
        el.classList.toggle('hidden', n !== name);
      });
    };
    if (document.startViewTransition) document.startViewTransition(activate);
    else activate();
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
