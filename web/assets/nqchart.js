/* ================================================================
 * nqchart.js —— 第二张图：NQ（纳斯达克100）5m 柱子 + 主图那份 taker MACD
 *
 * 用户口径（2026-10-03 二十二·三期）：
 *   「K线图下面再给我做个K线图，但是这个K线图的数据有要求就是必须是 duck*
 *    的 30 天的 K 线图，其中 K 线图的 K 线柱子必须是 NQ 也就是纳斯达克指数
 *    期货的 5 分钟数据，下面要放上刚刚上面 K 线图的 macd 数据，而不是 NQ 的
 *    macd 数据，而且时间要对齐；周末时间因为纳斯达克指数无法交易所以不交易的
 *    时候请还是要实时更新数据，就是不显示不交易的 K 线图；交易的时间的数据
 *    请对齐 macd；并且保存在数据库这样读取直接数据库进行，更新数据也在数据库
 *    进行。」
 *
 * 拆解成四条实现约束：
 *   ① 柱子 = NQ 5m（Dukascopy 落 kline 表 inst_id=NQ-INDEX，30 天）
 *   ② 副图 = **主图那份** taker 买卖比 MACD(12,26,60)，不是 NQ 自己的 MACD
 *   ③ 时间对齐：后端只回「NQ 有柱子」的时间戳对应的 MACD（见 api_nqchart.go）
 *      —— 纳指周末休市，若把 24 小时的 MACD 原样铺上去，副图会比柱子长一截
 *   ④ 全链路走库：本文件只读 /api/nqchart（纯读接口），写入由后端
 *      StartNQSync（Dukascopy 历史）+ StartNQIntraday（Yahoo 当天）负责
 *
 * 二十二期·五又加了一张副图（副图 2，「MACD2」）。用户口径：
 *   「NQ 这个 macd2 数据绑定为所有美股+ETF 数据的每 5 分钟的 takervol 总和，
 *    还是 12 26 60 参数，给我把这个 macd2 副图绑定到 NQ 的 macd 副图下面给我
 *    呆着，还是数据库保存，绑定 NQ 主图 K 线数据，移动缩小放大都跟随主图，
 *    和副图 1 的 macd 一样。」
 *   → 数据源 taker_macd_us（美股+ETF 190 个合约的每 5m 买卖量总和 → 比值 →
 *     MACD 12/26/60）；数据点在 nqState.macd2，系列是 hist2/dif2/dea2，
 *     用独立 priceScaleId='nqmacd2' 压在副图 1 下方。
 *   → 「跟随主图缩放平移」是**共享 timeScale 的天然结果**，没有额外代码。
 *
 * ★ 为什么休市「不画」是免费得到的：Dukascopy 周末/节假日当天文件是 404，
 *   库里根本没有那些 ts 的行；lightweight-charts 的时间轴是按**数据点**排的，
 *   缺的时段自然被压缩，不会留出空白格子。所以「不显示不交易的 K 线图」
 *   不需要前端做任何过滤 —— 但**更新**照跑（StartNQSync 每轮都试），
 *   开盘那一刻数据自然接上。
 *   （注：实际库里有休市占位平柱，由后端按 v>0 过滤掉，见 api_nqchart.go。）
 *
 * ★ 函数一律带 nq 前缀：core.js 里有全局 fmtVol/fmtPct/fmtPrice，
 *   重名会让整个文件 SyntaxError 静默不执行（本项目老毛病，二十二期踩过）。
 *   同理，本文件新增的 ID 一律带 nq / nqLg2 前缀，不去挤别的模块的命名域。
 * ================================================================ */

// NQ 图状态
//
// ★ 二十二·五加了第二张副图（macd2）：state 里带 2 后缀的那几项都归它。
//   两张副图刻意分开存（macd / macd2），不合并成一个数组 —— 它们的池子、
//   时间覆盖、刷新节奏都不一样，合并后任何一边出问题都会污染另一边。
var nqState = {
  chart: null,
  candle: null,
  hist: null,
  dif: null,
  dea: null,
  hist2: null,     // 副图 2：美股/ETF 池 takervol 总和的 MACD
  dif2: null,
  dea2: null,
  rows: [],        // 已加载的 NQ 5m 蜡烛（升序）
  macd: [],        // 与之对齐的 MACD 点（后端已按 NQ 时间戳过滤）—— 副图 1
  macd2: [],       // 同上，副图 2（美股+ETF 池）
  map: {},         // ts -> macd 点（图例取值）
  map2: {},        // ts -> macd2 点
  loading: false,
  loadingAt: 0,
  timer: null,
  lastKey: '',     // 首尾 ts 指纹：没变就不重绘（省掉整幅 setData 的抖动）
};

// nqFmt 价格格式化：指数点位，保留 1 位小数（与 Dukascopy 口径一致）
function nqFmt(v) {
  if (v === null || v === undefined || !isFinite(v)) return '--';
  return Number(v).toFixed(1);
}

// nqFmtTime 毫秒 ts → "MM-DD HH:mm"（本地时区，与顶栏一致）
function nqFmtTime(ms) {
  var d = new Date(ms);
  var p = function (n) { return (n < 10 ? '0' : '') + n; };
  return p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' + p(d.getHours()) + ':' + p(d.getMinutes());
}

// nqInitChart 建第二个 lightweight-charts 实例（主图建完后再建）
//
// 布局：蜡烛占上部（scaleMargins bottom 0.62），两张 MACD 副图各挂一个
// 独立 priceScale（'nqmacd' / 'nqmacd2'）分别压在中、下两段 —— 三条
// scaleMargins 的具体取值见下，改一条就要连另外两条一起核对。
function nqInitChart() {
  if (nqState.chart) return;
  var host = document.getElementById('nqchart');
  if (!host || typeof LightweightCharts === 'undefined') return;

  nqState.chart = LightweightCharts.createChart(host, {
    width: host.clientWidth,
    height: host.clientHeight,
    // 与主图同一套视觉口径（背景/网格/字体/中文时间格式）——
    // 两张图上下叠着，配色或时间格式不一致会明显"两张皮"。
    layout: {
      background: { type: 'solid', color: '#0b0e11' },
      textColor: '#b7bdc6',
      fontSize: 11,
      fontFamily: 'Menlo, Consolas, monospace',
      attributionLogo: false,
    },
    grid: {
      vertLines: { color: '#14181d' },
      horzLines: { color: '#14181d' },
    },
    localization: {
      locale: 'zh-CN',
      timeFormatter: function (t) {
        var d = new Date(t * 1000);
        var p = function (n) { return String(n).length < 2 ? '0' + n : String(n); };
        return p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' + p(d.getHours()) + ':' + p(d.getMinutes());
      },
    },
    rightPriceScale: {
      borderColor: '#262b31', borderVisible: true,
      // ★ 二十二·五：原来只有一张副图（bottom 0.32），现在上下叠两张，
      //   主图要让出下半个版面。三条 scaleMargins 必须**互不重叠**：
      //     蜡烛     3% ~ 45%
      //     副图 1  48% ~ 71%
      //     副图 2  74% ~ 97%
      //   改其中之一就要连另外两条一起看，否则会叠在一起糊成一片。
      //   （面板本身只有 300px 高，chart-box 约 220px，所以每段都很紧凑 ——
      //     嫌挤就拖 h-split[data-split="B"] 把整块拉高。）
      scaleMargins: { top: 0.03, bottom: 0.55 },
      entireTextOnly: true,
    },
    timeScale: {
      borderColor: '#262b31', timeVisible: true, secondsVisible: false,
      rightOffset: 4, barSpacing: 6, minBarSpacing: 0.3,
    },
    crosshair: {
      mode: LightweightCharts.CrosshairMode.Normal,
      vertLine: { color: '#4a515c', width: 1, style: LightweightCharts.LineStyle.Dashed, labelBackgroundColor: '#2b3139' },
      horzLine: { color: '#4a515c', width: 1, style: LightweightCharts.LineStyle.Dashed, labelBackgroundColor: '#2b3139' },
    },
    handleScroll: true,
    handleScale: true,
  });

  nqState.candle = nqState.chart.addCandlestickSeries({
    upColor: '#0ecb81', downColor: '#f6465d',
    borderUpColor: '#0ecb81', borderDownColor: '#f6465d',
    wickUpColor: '#0ecb81', wickDownColor: '#f6465d',
    priceFormat: { type: 'price', precision: 1, minMove: 0.1 },
  });

  // ---- 副图 1：taker 买卖比 MACD（与主图同一份数据、同一套配色）----
  nqState.hist = nqState.chart.addHistogramSeries({
    priceScaleId: 'nqmacd',
    priceFormat: { type: 'price', precision: 5, minMove: 0.00001 },
    lastValueVisible: false, priceLineVisible: false,
  });
  nqState.chart.priceScale('nqmacd').applyOptions({ scaleMargins: { top: 0.48, bottom: 0.29 } });

  nqState.dif = nqState.chart.addLineSeries({
    priceScaleId: 'nqmacd', color: '#f0b90b', lineWidth: 1,
    priceLineVisible: false, lastValueVisible: false, crosshairMarkerVisible: false,
  });
  nqState.dea = nqState.chart.addLineSeries({
    priceScaleId: 'nqmacd', color: '#4facfe', lineWidth: 1,
    priceLineVisible: false, lastValueVisible: false, crosshairMarkerVisible: false,
  });

  // ---- 副图 2：美股/ETF 池 takervol 总和的 MACD（二十二·五）----
  //
  // 用户口径：「把 macd2 副图绑定到 NQ 的 macd 副图下面给我呆着，
  // 绑定 NQ 主图 K 线数据，移动缩小放大都跟随主图，和副图 1 的 macd 一样」。
  //
  // ★ 「跟随主图」不是靠监听事件做出来的，而是**天然如此**：
  //   同一个 chart 实例里的所有 series 共享一个 timeScale —— 只要不给它
  //   单独开 pane / 单独的 time scale，滚轮缩放、拖拽平移、双击复位
  //   全都会同步作用在两张副图上。副图 1 本来就是这么工作的。
  //   唯一要自己管的是「上下位置」：靠独立 priceScaleId + scaleMargins
  //   把两张副图放在不同的纵向区间（见上面 nqInitChart 的注释）。
  //
  // ★ 配色与副图 1 完全一致（金 DIF / 蓝 DEA / 红绿柱）：它们是同一种指标、
  //   同一组参数，只有池子不同 —— 用不同颜色反而会被误认成两种指标。
  //   区分靠图例文字（「美股+ETF 总和」）与上下位置，不靠颜色。
  nqState.hist2 = nqState.chart.addHistogramSeries({
    priceScaleId: 'nqmacd2',
    priceFormat: { type: 'price', precision: 5, minMove: 0.00001 },
    lastValueVisible: false, priceLineVisible: false,
  });
  nqState.chart.priceScale('nqmacd2').applyOptions({ scaleMargins: { top: 0.74, bottom: 0.03 } });

  nqState.dif2 = nqState.chart.addLineSeries({
    priceScaleId: 'nqmacd2', color: '#f0b90b', lineWidth: 1,
    priceLineVisible: false, lastValueVisible: false, crosshairMarkerVisible: false,
  });
  nqState.dea2 = nqState.chart.addLineSeries({
    priceScaleId: 'nqmacd2', color: '#4facfe', lineWidth: 1,
    priceLineVisible: false, lastValueVisible: false, crosshairMarkerVisible: false,
  });

  // 十字光标 → 图例 + 顶部 OHLC
  nqState.chart.subscribeCrosshairMove(function (param) {
    if (!param || !param.time) return;
    var ts = param.time * 1000;
    var k = null;
    for (var i = 0; i < nqState.rows.length; i++) {
      if (nqState.rows[i].ts === ts) { k = nqState.rows[i]; break; }
    }
    if (k) {
      document.getElementById('nqOhlc').textContent =
        '开 ' + nqFmt(k.o) + '  高 ' + nqFmt(k.h) + '  低 ' + nqFmt(k.l) + '  收 ' + nqFmt(k.c);
      document.getElementById('nqLgOhlc').textContent = 'O ' + nqFmt(k.o) + ' H ' + nqFmt(k.h) +
        ' L ' + nqFmt(k.l) + ' C ' + nqFmt(k.c);
    }
    nqLgMacd(ts);
  });

  // 尺寸跟手：容器变化（拖 h-split / 面板缩放 / 窗口）都要重算
  if (typeof ResizeObserver !== 'undefined') {
    new ResizeObserver(function () { nqResize(); }).observe(host);
  }
}

// nqResize 按容器实际尺寸刷新画布
function nqResize() {
  var host = document.getElementById('nqchart');
  if (!host || !nqState.chart) return;
  try {
    nqState.chart.applyOptions({ width: host.clientWidth, height: host.clientHeight });
  } catch (e) { /* boot 未完成时静默 */ }
}

// nqFmtVol 买卖量格式化：这个池子的数字以「亿」为主，原样打出来 11 位数字会把图例撑爆
function nqFmtVol(v) {
  if (v === null || v === undefined || !isFinite(v)) return '--';
  var a = Math.abs(v);
  if (a >= 1e8) return (v / 1e8).toFixed(2) + '亿';
  if (a >= 1e4) return (v / 1e4).toFixed(2) + '万';
  return v.toFixed(0);
}

// nqLgMacd 刷新两张副图的图例（十字光标所在那根）
//
// ★ 上下两张副图共用一个十字光标，所以必须在同一个回调里一起更新 ——
//   分两处更新的话，光标移动时会出现「上面这张已经变了、下面还是上一根」的
//   错位闪动（本项目主图副图也踩过同类问题）。
function nqLgMacd(ts) {
  // ---- 副图 1：加密池买卖比 ----
  var m = nqState.map[ts];
  if (!m) {
    var src = document.getElementById('nqLgSrc');
    if (src) src.textContent = '买卖比 --';
    ['nqLgDif', 'nqLgDea', 'nqLgHist'].forEach(function (id) {
      var e = document.getElementById(id);
      if (e) e.textContent = '--';
    });
  } else {
    var src1 = document.getElementById('nqLgSrc');
    if (src1) src1.textContent = '买卖比 ' + Number(m.src).toFixed(3);
    var d1 = document.getElementById('nqLgDif'), d2 = document.getElementById('nqLgDea'), d3 = document.getElementById('nqLgHist');
    if (d1) d1.textContent = Number(m.dif).toFixed(5);
    if (d2) d2.textContent = Number(m.dea).toFixed(5);
    if (d3) d3.textContent = Number(m.hist).toFixed(5);
    var dot = document.getElementById('nqLgHistDot');
    if (dot) dot.style.background = m.hist >= 0 ? '#0ecb81' : '#f6465d';
  }

  // ---- 副图 2：美股/ETF 池的 takervol 总和 ----
  var m2 = nqState.map2[ts];
  var s2 = document.getElementById('nqLg2Src');
  if (!m2) {
    if (s2) s2.textContent = '美股+ETF 总和 --';
    ['nqLg2Dif', 'nqLg2Dea', 'nqLg2Hist'].forEach(function (id) {
      var e = document.getElementById(id);
      if (e) e.textContent = '--';
    });
    return;
  }
  if (s2) {
    s2.textContent = '美股+ETF 买 ' + nqFmtVol(m2.buy) + ' / 卖 ' + nqFmtVol(m2.sell);
    s2.title = '全池买卖比 ' + Number(m2.src).toFixed(4) + ' · ' + m2.n + ' 个合约参与聚合';
  }
  var e1 = document.getElementById('nqLg2Dif'), e2 = document.getElementById('nqLg2Dea'), e3 = document.getElementById('nqLg2Hist');
  if (e1) e1.textContent = Number(m2.dif).toFixed(5);
  if (e2) e2.textContent = Number(m2.dea).toFixed(5);
  if (e3) e3.textContent = Number(m2.hist).toFixed(5);
  var dot2 = document.getElementById('nqLg2HistDot');
  if (dot2) dot2.style.background = m2.hist >= 0 ? '#0ecb81' : '#f6465d';
}

// nqSessionInfo 交易时段提示：纳指（CME）周日 18:00 ET → 周五 17:00 ET，
// 每日 17:00–18:00 ET 维护休市。这里只给一个「是否在交易中」的粗判，
// 用本地时间近似（ET 与北京时间差 12/13 小时，周末判断足够用）。
function nqSessionInfo(now) {
  var d = new Date(now);
  var day = d.getDay();        // 0=周日 … 6=周六
  var h = d.getHours() + d.getMinutes() / 60;
  // 北京时间：周六全天休；周日 06:00 前休（对应周五 17:00 ET 收盘后）；
  // 周一 04:00–05:00 维护（对应 17:00–18:00 ET）
  if (day === 6) return { open: false, text: '休市（周六）' };
  if (day === 0 && h < 6) return { open: false, text: '休市（周末）' };
  if (day === 1 && h >= 4 && h < 5) return { open: false, text: '维护中（每日 1 小时）' };
  return { open: true, text: '交易中' };
}

// nqTickSession 刷新顶栏的交易时段标签 + 距下一根 5m 收盘倒计时
function nqTickSession() {
  var now = Date.now();
  var s = nqSessionInfo(now);
  var tag = document.getElementById('nqSessionTag');
  if (tag) {
    tag.textContent = s.open ? '● 交易中' : '○ ' + s.text;
    tag.className = 'nq-tag ' + (s.open ? 'on' : 'off');
  }
  var el = document.getElementById('nqSession');
  if (el) {
    var next = Math.ceil(now / 300000) * 300000;
    var left = Math.max(0, Math.round((next - now) / 1000));
    el.textContent = Math.floor(left / 60) + ':' + ('0' + (left % 60)).slice(-2);
  }
}

// nqPaint 全量铺数据 + 定视口
function nqPaint(fit) {
  if (!nqState.chart) return;
  nqState.candle.setData(nqState.rows.map(function (k) {
    return { time: Math.floor(k.ts / 1000), open: k.o, high: k.h, low: k.l, close: k.c };
  }));

  // 两张副图共用同一套铺法（hist 按正负染色，dif/dea 两条线）——
  // 写成函数铺两遍，而不是复制两段：改一张漏一张是本项目的老毛病。
  var paintMacd = function (sHist, sDif, sDea, pts) {
    if (!sHist || !sDif || !sDea) return;
    sHist.setData(pts.filter(function (p) { return isFinite(p.hist); }).map(function (p) {
      return {
        time: Math.floor(p.ts / 1000), value: p.hist,
        color: p.hist >= 0 ? 'rgba(14,203,129,.75)' : 'rgba(246,93,93,.75)',
      };
    }));
    var line = function (key) {
      return pts.filter(function (p) { return isFinite(p[key]); }).map(function (p) {
        return { time: Math.floor(p.ts / 1000), value: p[key] };
      });
    };
    sDif.setData(line('dif'));
    sDea.setData(line('dea'));
  };
  paintMacd(nqState.hist, nqState.dif, nqState.dea, nqState.macd);      // 副图 1：加密池
  paintMacd(nqState.hist2, nqState.dif2, nqState.dea2, nqState.macd2);  // 副图 2：美股/ETF 池

  if (fit && nqState.rows.length) {
    // 全量 30 天一次铺开（用户口径「30 天的 K 线图」），用户可自行滚轮缩放
    try { nqState.chart.timeScale().fitContent(); } catch (e) {}
  }
}

// nqLoad 拉数据（纯读库接口）+ 渲染
function nqLoad(force) {
  var now = Date.now();
  if (nqState.loading) {
    if (now - (nqState.loadingAt || 0) < 60000) return Promise.resolve();
    console.warn('nqLoad 上一轮疑似卡死，强制重试');
  }
  nqState.loading = true;
  nqState.loadingAt = now;
  var ctl = typeof AbortController !== 'undefined' ? new AbortController() : null;
  var timer = ctl ? setTimeout(function () { ctl.abort(); }, 15000) : null;
  var hint = document.getElementById('nqHint');
  if (hint) hint.textContent = '加载中…';

  return api('/api/nqchart?days=30', { signal: ctl && ctl.signal }).then(function (j) {
    nqState.rows = (j && j.kline) || [];
    nqState.macd = (j && j.macd) || [];
    nqState.macd2 = (j && j.macd2) || [];
    var m = {};
    nqState.macd.forEach(function (p) { m[p.ts] = p; });
    nqState.map = m;
    var m2 = {};
    nqState.macd2.forEach(function (p) { m2[p.ts] = p; });
    nqState.map2 = m2;

    // ★ 指纹必须把两张副图的点数也算进去：美股池是后铺的（190 个合约要拉
    //   好几分钟），首屏很可能出现「柱子有了、副图 2 还是空」的中间态。
    //   只认 NQ 柱子的首尾 ts 的话，这个中间态会被判成「没变化」而不重绘 ——
    //   副图 2 要等到下一根 NQ 柱子出现才显示。这是本项目
    //   「缓存判据漏了维度」的老坑，一次说清。
    var key = nqState.rows.length
      ? (nqState.rows[0].ts + '|' + nqState.rows[nqState.rows.length - 1].ts + '|' + nqState.rows.length +
         '|m' + nqState.macd.length + '|m2' + nqState.macd2.length)
      : 'empty';
    if (force || key !== nqState.lastKey) {
      nqPaint(force || !nqState.lastKey);
      nqState.lastKey = key;
    }

    // 顶栏最新价 / 涨跌幅（相对窗口首根，与只读板块口径一致）
    var last = nqState.rows[nqState.rows.length - 1];
    var first = nqState.rows[0];
    if (last) {
      var pe = document.getElementById('nqPrice');
      if (pe) pe.textContent = nqFmt(last.c);
      var chg = first && first.o > 0 ? (last.c - first.o) / first.o * 100 : 0;
      var ce = document.getElementById('nqChg');
      if (ce) {
        ce.textContent = (chg >= 0 ? '+' : '') + chg.toFixed(2) + '%';
        ce.className = 'chg ' + (chg > 0 ? 'up' : (chg < 0 ? 'down' : 'flat'));
      }
      var lg = document.getElementById('nqLgOhlc');
      if (lg) lg.textContent = 'O ' + nqFmt(last.o) + ' H ' + nqFmt(last.h) +
        ' L ' + nqFmt(last.l) + ' C ' + nqFmt(last.c);
      nqLgMacd(last.ts);
    }

    var cov = (j && j.coverage) || {};
    var span = cov.spanDays ? cov.spanDays.toFixed(1) : (cov.bars ? (cov.bars / 288).toFixed(1) : '0');
    if (hint) {
      hint.textContent = nqState.rows.length
        ? ('NQ 5m · ' + nqState.rows.length + ' 根 · ' + span + ' 天 · 副图 ' +
           nqState.macd.length + '/' + nqState.macd2.length + ' 点')
        : 'NQ 数据还没铺上（后台正在按 30 天回补，Dukascopy 限流要分几轮）';
    }
    var covEl = document.getElementById('nqCov');
    if (covEl) {
      covEl.textContent = nqState.rows.length
        ? ('覆盖 ' + nqFmtTime(nqState.rows[0].ts) + ' → ' + nqFmtTime(nqState.rows[nqState.rows.length - 1].ts))
        : '覆盖 -- （等待 NQ 回补）';
    }
  }).catch(function (e) {
    if (hint) hint.textContent = '加载失败：' + (e && e.message ? e.message : e);
    console.warn('nqLoad 失败', e);
  }).then(function () {
    if (timer) clearTimeout(timer);
    nqState.loading = false;
  });
}

// nqBoot 初始化（main.js 的 boot 里调用）
function nqBoot() {
  nqInitChart();
  var btn = document.getElementById('nqBtnRefresh');
  if (btn) btn.addEventListener('click', function () { nqLoad(true); });
  nqTickSession();
  setInterval(nqTickSession, 1000);
  // 首屏错峰：boot 瞬间连接池紧张（低内存机器实测），延后 2 秒再拉
  setTimeout(function () { nqLoad(true); }, 2000);
  // 每 60 秒轮询：NQ 柱子 5 分钟才出一根，盘中增量由后端 Yahoo 线每 5 分钟写库
  nqState.timer = setInterval(function () { nqLoad(false); }, 60000);
}
