/* ================================================================
 * main.js —— 启动入口：boot 序列 + 轮询注册
 * ★ 必须最后一个加载（它立即执行 boot，依赖前面所有层的函数）。
 * ================================================================ */
/* ------------------------------------------------------------------ */
/* 启动                                                                */
/* ------------------------------------------------------------------ */

(async function boot() {
  initChart();
  initPnlChart();
  initFx();
  bindEvents();
  applyIndVisibility();
  // ★ 二十二期：taker 买卖流向面板。自带首屏加载 + 30 秒轮询，
  //   放在这里是为了不阻塞下面的主数据加载 —— 面板挂了页面照样能用。
  try { bindTakerPanel(); } catch (e) { console.warn('bindTakerPanel 失败', e); }
  // ★ 二十二·三期：主图下面第二张图（NQ 5m 柱子 + taker MACD 副图）。
  //   同上，自带首屏加载 + 60 秒轮询，失败不影响主流程。
  try { nqBoot(); } catch (e) { console.warn('nqBoot 失败', e); }
  // ★ 二十二·四：买入信号声音提醒（macd>0 and ref macd<0 and refref macd<0）。
  //   纯网页出声；自带首屏初始化 + 15 秒轮询，失败不影响主流程。
  try { sigBoot(); } catch (e) { console.warn('sigBoot 失败', e); }
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
