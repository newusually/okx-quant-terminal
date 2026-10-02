/* ================================================================
 * render.js —— 渲染层：合约列表 / 顶栏 / 图表头 / K线渲染 / 持仓均价线
 * ================================================================ */
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
  let list = state.insts.filter((it) =>
    !kw || it.instId.toUpperCase().includes(kw) || (it.name || '').toUpperCase().includes(kw));
  // ★ 十四期：排序（用户原话「上涨排序 金额排序」）★
  // chg 用实时 ticker 的涨幅优先（没行情再退合约快照），vol 按 24h 成交额。
  // 都是**降序**：涨幅最高的 / 金额最大的排最前，不用滚动就能看到头部。
  if (state.sortKey === 'chg') {
    const cg = (it) => { const t = state.tickers[it.instId]; return (t && t.chgPct !== undefined && t.chgPct !== null) ? t.chgPct : (it.chgPct || 0); };
    list = list.slice().sort((a, b) => cg(b) - cg(a));
  } else if (state.sortKey === 'vol') {
    list = list.slice().sort((a, b) => (b.quoteVol24h || 0) - (a.quoteVol24h || 0));
  }
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
    // 只读板块用单独一种样式：它不是「暂时不合格」，而是**设计上不可交易**
    const badge = isReadonlyInst(it.instId)
      ? `<span class="badge-ro" title="只读板块：只展示数据与信号，不接入交易 API">只读 · 不可交易</span>`
      : it.tradeable
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
    // ★ 2026-10-02 七期：触发改为纯价格条件（收盘价低于买入价 drop_pct% 且该根涨 > bar_rise_pct%）。
    : `最多 ${sa.max_times || '--'} 次 · 触发：收盘价较买价 −${sa.drop_pct ?? '--'}% 且该根涨 > ${sa.bar_rise_pct ?? '--'}%`;
  // 超时平仓：60 → "1 小时"。整数小时就说小时，否则说分钟，和后台的
  // HoldText() 口径一致（以前后台写「60 分钟」、网页写「1 小时」，两边对不上）。
  const holdTxt = sx.max_hold_minutes > 0
    ? (sx.max_hold_minutes % 60 === 0 && sx.max_hold_minutes >= 60
      ? (sx.max_hold_minutes / 60) + ' 小时'
      : sx.max_hold_minutes + ' 分钟')
    : (sx.max_hold_bars > 0 ? sx.max_hold_bars + ' 根' : '关闭');
  // 出场规则一句话（与后台 runExits 口径一致）：
  // 只列出**真正开着**的通道。七期口径：「止盈 0.35% · 止损 -300% · 超时 24 小时」。
  const exitRuleParts = [];
  if (sx.take_profit_pct > 0) exitRuleParts.push('止盈 ' + sx.take_profit_pct + '%');
  if (sx.boll_upper_exit) exitRuleParts.push('布林上轨');
  if (sx.stop_loss_pct > 0) exitRuleParts.push('止损 -' + sx.stop_loss_pct + '%');
  if (sx.max_hold_minutes > 0) exitRuleParts.push('超时 ' + holdTxt);
  else if (sx.max_hold_bars > 0) exitRuleParts.push('超时 ' + sx.max_hold_bars + ' 根');
  const exitRuleTxt = exitRuleParts.length ? exitRuleParts.join(' · ') : '无（不会自动平仓）';
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
    // 出场条件：★ 四期起唯一通道是超时（止盈与布林上轨已关闭）。
    // 只列开着的通道 —— 把关闭项写成「0%」会让人以为只是线设在 0，与事实相反。
    ['出场条件', exitRuleTxt],
    ['止盈', sx.take_profit_pct > 0 ? sx.take_profit_pct + '%' : '关闭'],
    ['止损', sx.stop_loss_pct > 0 ? sx.stop_loss_pct + '%' : '不设'],
    ['超时平仓', holdTxt],
    ['布林上轨平仓', sx.boll_upper_exit ? '开' : '关'],
    ['加仓', addonTxt],
    // ★ 五期口径：score_threshold 现在就是字面语义「Score ≥ 3」，不再需要括号注解
    //   （三期时它写的是 4、用来表达「> 3」，才要额外说明）。
    ['共振阈值', String((st.strategy && st.strategy.score_threshold) || '--') + ' / 8'],
    // 门槛「触发信号那根 K 线的涨跌幅」（带符号，六期起负值 = 必须真跌）。
    // 0 = 该条件已关闭。当前 -0.7%。
    ['K线涨跌幅要求', (st.minBarRisePct > 0
      ? '> ' + fmtNum(st.minBarRisePct, 2) + '%（必须真涨，收盘 vs 开盘）'
      : st.minBarRisePct < 0
        ? '< ' + fmtNum(st.minBarRisePct, 2) + '%（必须真跌，收盘 vs 开盘）'
        : '已关闭')],
    // 品类过滤（三期已取消）。显示出来，免得以后有人以为「美股/ETF 被排掉了」。
    ['品类过滤', st.excludeStockEtf ? '只做加密（美股/ETF/商品排除）' : '不限（只看最小一手 ≤ 上限）'],
    // 三条独立红线（2026-10-01 二期起）：
    //   记录表 30 天 → 月度任务里清；K 线 10 天 → 每日任务里清；日志 30 天 → 月度任务里清
    ['记录保留', String(st.retainDays || 30) + ' 天（月度清理）'],
    ['K线保留', String(st.klineRetainDays || 10) + ' 天（每日清理）'],
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
  // ★ 只读板块（NQ）没有 OKX 行情，它的「最新价」是本地用 K 线合成的
  //   （见 syncReadonlyTicker）。这里必须把合成条目搬过来 ——
  //   否则每 2 秒一次的 ticker 轮询会把它整个冲掉，
  //   标题价格变「--」、图例名称退化成裸 instId。
  Object.keys(READONLY_INSTS).forEach((id) => {
    const prev = state.tickers && state.tickers[id];
    if (prev) map[id] = prev;
  });
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
  paintEntryLines();   // ★ 十一期：均价虚线（换合约时这里会把旧线撤掉）
  drawOnInstChange();  // ★ 十一期：换合约/周期 → 换一套画痕并重绘

  if (lr && state.klines.length > prevCount) {
    // 往前插了 bar，逻辑索引整体右移，可视区跟着右移同样的量
    const shift = state.klines.length - prevCount;
    state.chart.timeScale().setVisibleLogicalRange({ from: lr.from + shift, to: lr.to + shift });
    state.scrollGuardUntil = Date.now() + 600;   // 插完数据别再立刻触发下一页
  }
  // ★ 顺序要紧：先给只读板块合成行情，再渲染图例 ★
  //   renderLegend 的名称取自 state.tickers[curInst].name ——
  //   反过来（先图例后合成）它的文本框里会残留裸 instId。
  syncReadonlyTicker();
  renderLegend(state.klines[state.klines.length - 1] || null);
}

// syncReadonlyTicker 给只读板块（NQ）合成一条行情快照。
//
// 它没有 OKX 行情、/api/tickers 里也没有它 —— 不补这一条，标题和列表里的
// 价格 / 涨跌幅会永远是「--」，看起来像数据坏了。用图上首尾两根 K 线算出
// 「最新价 + 区间涨跌幅」（口径与 OKX 合约一致：都是相对当前窗口起点）。
function syncReadonlyTicker() {
  const inst = state.curInst;
  if (!isReadonlyInst(inst)) return;
  const ks = state.klines || [];
  if (!ks.length) return;
  const last = ks[ks.length - 1], first = ks[0];
  const it = (state.insts || []).find((x) => x.instId === inst) || {};
  state.tickers[inst] = Object.assign({}, state.tickers[inst], {
    instId: inst,
    name: it.name || READONLY_INSTS[inst],
    last: last.c,
    chgPct: first.o > 0 ? ((last.c - first.o) / first.o) * 100 : 0,
  });
  try { renderChartHead(); } catch (e) { /* 标题刷新失败不该影响画图 */ }
}

// setCandleFormat 按合约最小变动价位设置价格精度
function setCandleFormat(tickSize) {
  if (!tickSize) return;
  let prec = Math.max(0, Math.min(8, Math.ceil(-Math.log10(tickSize))));
  // 只读板块是指数（NQ），报价习惯两位小数（29461.75）——
  // 按 tickSz=0.25 推出来只有 1 位，读起来像被截断了。
  if (isReadonlyInst(state.curInst)) prec = Math.max(prec, 2);
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
  drawRender();   // ★ 十一期：数据动了 → 画痕的像素位置可能变，重算一遍
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

/* ================================================================== */
/* ★ 十一期：持仓均价虚线（60% 透明度）                                */
/* ================================================================== */
/* 用户原话：「持仓平均价画虚线 透明度60%」。
 *
 * 实现选 createPriceLine 而不是自己画 canvas：
 *   · createPriceLine 自带右侧价格轴标签 + 跟随缩放平移，
 *     用户滚到哪它都贴在正确的价格上 —— 自己画 canvas 就得自己
 *     处理 priceToCoordinate / 重绘时机，等于重造一遍它已经做好的事；
 *   · lineStyle 用 LineStyle.Dashed（虚线），颜色用 rgba(...,0.6)。
 *
 * ★ 关键纪律：换合约时必须把上一组线**全部撤掉**。
 *   priceLine 是挂在 series 上的，setData 换数据不会清掉它 ——
 *   不撤的话，BTC 的均价线会留在 CAP 的图上，那是凭空多出来的一条假线。 */
const AVG_LINE_COLOR = 'rgba(255, 213, 79, 0.6)';   // 十一期：透明度 60%

function clearEntryLines() {
  state.entryLines.forEach((pl) => {
    try { state.candle.removePriceLine(pl); } catch (_) { /* 已被 setData 清掉 */ }
  });
  state.entryLines = [];
  state.entryLineKey = '';
}

// paintEntryLines 给**当前合约**的每笔持仓画一条均价虚线。
// 一个合约同时只会有一个仓位（trader.go 的「该合约已持仓」闸门），
// 但这里按数组写，万一以后放开同合约多仓，代码不用改。
function paintEntryLines() {
  if (!state.candle || !state.curInst) return;

  const key = state.curInst + '|' + state.curBar;
  // 持仓数据 3 秒刷一次，均价基本不动；key 没变就别反复拆建（会闪）
  const pxs = (state.positions || [])
    .filter((p) => p.instId === state.curInst && Number(p.entryPx) > 0)
    .map((p) => ({ px: Number(p.entryPx), sz: Number(p.sz) || 0, lev: p.leverage }));
  const sig = key + '#' + pxs.map((x) => x.px).join(',');

  if (sig === state.entryLineKey && state.entryLines.length) return;

  clearEntryLines();
  state.entryLineKey = sig;

  pxs.forEach((p) => {
    try {
      const pl = state.candle.createPriceLine({
        price: p.px,
        color: AVG_LINE_COLOR,
        lineWidth: 1,
        lineStyle: LightweightCharts.LineStyle.Dashed,   // ★ 虚线
        axisLabelVisible: true,
        title: '均价',
      });
      state.entryLines.push(pl);
    } catch (e) { console.warn('画均价线失败', e); }
  });
}
