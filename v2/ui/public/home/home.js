// The /home/ page's script. A file, not inline: the site CSP (script-src 'self') blocks inline script (#2471).
(() => {
  // Hero starfield: slow drift with a few stars in the band colors.
  const c = document.getElementById('stars'), ctx = c.getContext('2d');
  const BANDS = ['#D9482B', '#EB8A2E', '#EBC34B', '#2C66B8'];
  const reduce = matchMedia('(prefers-reduced-motion: reduce)').matches;
  let w = 0, h = 0, stars = [];
  function size() {
    const dpr = Math.min(2, devicePixelRatio || 1), r = c.getBoundingClientRect();
    w = r.width; h = r.height; c.width = w * dpr; c.height = h * dpr; ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    stars = Array.from({ length: Math.round(w * h / 2600) }, (_, i) => ({
      x: Math.random() * w, y: Math.random() * h, z: Math.random(), ph: Math.random() * 6.28,
      tint: i % 23 === 0 ? BANDS[(i / 23 | 0) % 4] : null,
    }));
  }
  function draw(t) {
    ctx.fillStyle = '#060A0C'; ctx.fillRect(0, 0, w, h);
    // a faint horizon glow in the band colors, lower right
    const g = ctx.createRadialGradient(w * 0.85, h * 1.05, 0, w * 0.85, h * 1.05, h * 0.9);
    g.addColorStop(0, 'rgba(235,138,46,0.16)'); g.addColorStop(0.5, 'rgba(217,72,43,0.06)'); g.addColorStop(1, 'rgba(6,10,12,0)');
    ctx.fillStyle = g; ctx.fillRect(0, 0, w, h);
    for (const s of stars) {
      const x = (s.x - t * 0.006 * (0.3 + s.z)) % w, xx = x < 0 ? x + w : x;
      const a = (0.25 + 0.65 * s.z) * (0.7 + 0.3 * Math.sin(t / 900 + s.ph));
      ctx.fillStyle = s.tint ? s.tint : `rgba(232,236,242,${a})`;
      const sz = s.tint ? 2 : s.z > 0.9 ? 1.6 : 1;
      ctx.fillRect(xx, s.y, sz, sz);
    }
  }
  size(); addEventListener('resize', size);
  if (reduce) draw(0); else (function loop(t) { draw(t); requestAnimationFrame(loop); })(0);

  // Served from an AMP, the install commands name that AMP's MCP endpoint.
  if (/^https?:$/.test(location.protocol)) {
    for (const el of document.querySelectorAll('.amp-url')) el.textContent = location.origin + '/mcp';
  }

  // Copy buttons copy their code block, comments stripped.
  for (const btn of document.querySelectorAll('.copy')) {
    const pre = document.getElementById(btn.dataset.copy);
    btn.addEventListener('click', async () => {
      const text = pre.innerText.split('\n').filter(l => !l.startsWith('#')).join('\n').replace(/\n{3,}/g, '\n\n').trim();
      try { await navigator.clipboard.writeText(text); btn.textContent = 'Copied'; }
      catch { const r = document.createRange(); r.selectNodeContents(pre); getSelection().removeAllRanges(); getSelection().addRange(r); btn.textContent = 'Selected'; }
      setTimeout(() => { btn.textContent = 'Copy'; }, 1800);
    });
  }

  // Sample run record: tokens tick up once, like live telemetry.
  if (!reduce) {
    const el = (id) => document.getElementById(id), fmt = n => n.toLocaleString('en-US');
    const target = { tin: 42180, tout: 18044, tcost: 0.43 };
    const t0 = performance.now();
    (function tick(now) {
      const p = Math.min(1, (now - t0) / 1800), e = 1 - Math.pow(1 - p, 3);
      el('tin').textContent = fmt(Math.round(target.tin * e));
      el('tout').textContent = fmt(Math.round(target.tout * e));
      el('tcost').textContent = (target.tcost * e).toFixed(2);
      if (p < 1) requestAnimationFrame(tick);
    })(t0);
  }
})();
