/* ================================================================
 * fx.js —— 特效层：魔法棒光标 / 点击小星星 / 流星雨（七期）
 * ================================================================ */
/* ------------------------------------------------------------------ */
/* 七期（2026-10-02）图表特效：魔法棒光标 + 点击小星星 + 流星雨          */
/*                                                                     */
/* ★ 八期修订：魔法棒光标改成 600px ★                                  */
/*                                                                     */
/* 为什么不用 CSS `cursor: url(...)` 了：                                */
/*   浏览器对自定义光标图片有**硬性尺寸上限**（Chromium / Firefox 都在  */
/*   128×128 逻辑像素量级），超限的图片会被**静默丢弃**并回退到下一个    */
/*   候选光标 —— 也就是放大到 600px 后，用户什么魔法棒都看不到，        */
/*   而且控制台不会报任何错。                                          */
/*   所以 600px 的魔法棒必须是一个**跟着鼠标走的真实 DOM 元素**：       */
/*     · 仍然 pointer-events:none，不抢图表的鼠标事件；                 */
/*     · 只在鼠标位于 K 线图内时显示，移出即隐藏；                      */
/*     · 用 transform 定位（不改 left/top，避免每帧触发布局）；         */
/*     · 图表本身保留 `cursor: crosshair`，作为兜底的可视准星。         */
/*                                                                     */
/* 全部纯前端、低频：流星同屏最多 3 颗、canvas 只在有流星时才重绘，      */
/* 不碰图表数据，也不拦截任何鼠标事件（fxCanvas pointer-events:none）。  */
/* ------------------------------------------------------------------ */

function initFx() {
  const box = document.querySelector('.chart-box');
  const cvs = $('fxCanvas');
  if (!box || !cvs || !cvs.getContext) return;
  const ctx = cvs.getContext('2d');

  // ---- 画布尺寸跟随容器（devicePixelRatio 对齐，拖尾不糊） ----
  const fit = () => {
    const r = box.getBoundingClientRect();
    const dpr = window.devicePixelRatio || 1;
    cvs.width = Math.max(1, Math.round(r.width * dpr));
    cvs.height = Math.max(1, Math.round(r.height * dpr));
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  };
  fit();
  window.addEventListener('resize', fit);

  // ---- ⓪ 魔法棒：DOM 元素跟随鼠标 ----
  //
  // 尺寸口径：★ 十五期 320 → 80 ★ 用户反馈「移动过来居然从左边一直拖到
  // 鼠标那边斜着放」—— 根因是 SVG 方块太大：320px 方块里棒身是
  // 左下→右上的对角线（约 365px），视觉上就是一根大斜线跟着鼠标满屏跑，
  // 鼠标一靠近图表边缘整根棒子就戳出去了。
  // 现在 80px：棒身约 91px，只在小范围内跟手，绝不再拉满屏；
  // 棒尖（金色星芒那端）仍精确对齐鼠标热点 —— 「指哪儿打哪儿」不变。
  const WAND_LEN = 80;
  const wand = document.createElement('div');
  wand.className = 'fx-wand';
  // 用 CSS 变量把长度透给样式，方便以后单独调
  wand.style.setProperty('--wand-len', WAND_LEN + 'px');
  wand.innerHTML =
    '<svg viewBox="0 0 600 600" width="' + WAND_LEN + '" height="' + WAND_LEN + '" aria-hidden="true">' +
    // 棒身：从右下指向左上，棒尖在 (600*0.94, 600*0.06) 附近
    '<defs>' +
    '<linearGradient id="fxWandBody" x1="0" y1="1" x2="1" y2="0">' +
    '<stop offset="0%" stop-color="#4b3a8f"/>' +
    '<stop offset="55%" stop-color="#7c5cff"/>' +
    '<stop offset="100%" stop-color="#c9b8ff"/>' +
    '</linearGradient>' +
    '<radialGradient id="fxWandGlow" cx="50%" cy="50%" r="50%">' +
    '<stop offset="0%" stop-color="rgba(255,236,160,.95)"/>' +
    '<stop offset="45%" stop-color="rgba(255,213,79,.42)"/>' +
    '<stop offset="100%" stop-color="rgba(255,213,79,0)"/>' +
    '</radialGradient>' +
    '</defs>' +
    // 棒尖光晕
    '<circle cx="564" cy="36" r="46" fill="url(#fxWandGlow)"/>' +
    // 棒身
    '<path d="M60 540 L544 56" stroke="url(#fxWandBody)" stroke-width="14" stroke-linecap="round"/>' +
    // 棒身高光（细白线，做出金属反光）
    '<path d="M70 528 L536 62" stroke="rgba(255,255,255,.55)" stroke-width="3.5" stroke-linecap="round"/>' +
    // 握把缠绕
    '<path d="M60 540 L118 482" stroke="#2b2154" stroke-width="19" stroke-linecap="round" opacity=".65"/>' +
    // 棒尖四芒星
    '<path d="M564 8 l7 17 17 7 -17 7 -7 17 -7-17 -17-7 17-7z" fill="#ffd54f"/>' +
    // 周围小星
    '<circle cx="524" cy="14" r="6" fill="#ffe9a3"/>' +
    '<circle cx="596" cy="86" r="5" fill="#fff3c4"/>' +
    '<circle cx="508" cy="70" r="4" fill="#b39dff"/>' +
    '<circle cx="546" cy="104" r="3.5" fill="#7cd4ff"/>' +
    '</svg>';
  box.appendChild(wand);

  // 热点：棒尖在 SVG 里的位置是 (564, 36)，换算成百分比后偏移，
  // 这样无论 WAND_LEN 怎么改，棒尖始终压在鼠标上。
  const HOT = { x: 564 / 600, y: 36 / 600 };
  let wandOn = false;
  const showWand = (on) => {
    if (on === wandOn) return;
    wandOn = on;
    wand.style.opacity = on ? '1' : '0';
  };
  box.addEventListener('mouseenter', () => showWand(true));
  box.addEventListener('mouseleave', () => showWand(false));
  box.addEventListener('mousemove', (e) => {
    const r = box.getBoundingClientRect();
    const x = e.clientX - r.left, y = e.clientY - r.top;
    // 用 left/top 把 SVG 的左上角摆到「让棒尖落在 (x,y)」的位置。
    // 这里不用 transform: translate() 是因为 SVG 已经占满 600×600，
    // 平移量随鼠标变化，用 transform 反而要多做一次字符串拼接；
    // 实际开销可忽略（只是两个样式赋值，且元素 pointer-events:none）。
    wand.style.transform =
      'translate(' + (x - WAND_LEN * HOT.x) + 'px,' + (y - WAND_LEN * HOT.y) + 'px)';
    showWand(true);
  });

  // ---- ① 点击小星星：从棒尖落点爆开 10 颗，向外飞散 + 旋转淡出 ----
  const STAR_GLYPHS = ['✦', '✧', '⭐', '✨'];
  const STAR_COLORS = ['#ffd54f', '#fff3c4', '#b39dff', '#7cd4ff'];
  box.addEventListener('click', (e) => {
    const r = box.getBoundingClientRect();
    const x = e.clientX - r.left, y = e.clientY - r.top;
    for (let i = 0; i < 10; i++) {
      const s = document.createElement('span');
      s.className = 'fx-star';
      s.textContent = STAR_GLYPHS[i % STAR_GLYPHS.length];
      s.style.left = x + 'px';
      s.style.top = y + 'px';
      s.style.color = STAR_COLORS[i % STAR_COLORS.length];
      s.style.fontSize = (10 + Math.random() * 10) + 'px';
      const ang = Math.random() * Math.PI * 2;
      const dist = 24 + Math.random() * 46;
      s.style.setProperty('--dx', Math.cos(ang) * dist + 'px');
      s.style.setProperty('--dy', (Math.sin(ang) * dist - 18) + 'px');
      box.appendChild(s);
      setTimeout(() => s.remove(), 850);   // 动画放完就收，不留 DOM
    }
  });

  // ---- ② 流星雨：随机间隔生成，右上 → 左下划过，带渐隐拖尾 ----
  const meteors = [];
  const rand = (a, b) => a + Math.random() * (b - a);
  const spawn = () => {
    if (meteors.length >= 3) return;   // 同屏上限，保住 2 核小机器
    const w = cvs.width / (window.devicePixelRatio || 1);
    const h = cvs.height / (window.devicePixelRatio || 1);
    meteors.push({
      x: rand(w * 0.25, w * 1.05),
      y: rand(-40, h * 0.35),
      vx: -rand(3.2, 5.6),           // 斜向左下
      vy: rand(2.0, 3.4),
      len: rand(110, 220),           // 拖尾长度
      life: 1,
    });
  };
  let nextSpawn = performance.now() + 900;
  const tick = (now) => {
    if (now >= nextSpawn) {
      spawn();
      nextSpawn = now + rand(1400, 3800);   // 平均 ~2.5 秒一颗
    }
    if (meteors.length) {
      const w = cvs.width / (window.devicePixelRatio || 1);
      const h = cvs.height / (window.devicePixelRatio || 1);
      ctx.clearRect(0, 0, w, h);
      for (let i = meteors.length - 1; i >= 0; i--) {
        const m = meteors[i];
        m.x += m.vx; m.y += m.vy;
        if (m.x + m.len < -40 || m.y > h + 40) { meteors.splice(i, 1); continue; }
        const nx = m.vx, ny = m.vy, nl = Math.hypot(nx, ny);
        const tx = m.x - (nx / nl) * m.len, ty = m.y - (ny / nl) * m.len;
        const g = ctx.createLinearGradient(m.x, m.y, tx, ty);
        g.addColorStop(0, 'rgba(255,244,214,.95)');
        g.addColorStop(.35, 'rgba(255,213,79,.55)');
        g.addColorStop(1, 'rgba(255,213,79,0)');
        ctx.strokeStyle = g;
        ctx.lineWidth = 2;
        ctx.lineCap = 'round';
        ctx.beginPath();
        ctx.moveTo(m.x, m.y);
        ctx.lineTo(tx, ty);
        ctx.stroke();
        // 流星头部一颗亮星
        ctx.fillStyle = '#fffbe8';
        ctx.beginPath();
        ctx.arc(m.x, m.y, 2.2, 0, Math.PI * 2);
        ctx.fill();
      }
    } else {
      ctx.clearRect(0, 0, cvs.width, cvs.height);
    }
    requestAnimationFrame(tick);
  };
  requestAnimationFrame(tick);
}
