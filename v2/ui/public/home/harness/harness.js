// The /home/harness/ page's script. A file, not inline: the site CSP (script-src 'self')
// blocks inline script (#2968, as for /home/ in #2471). Loaded with defer.
(() => {
  // Hero starfield, as on /home/: slow drift with a few stars in the band colors.
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
    const g = ctx.createRadialGradient(w * 0.15, h * 1.05, 0, w * 0.15, h * 1.05, h * 0.9);
    g.addColorStop(0, 'rgba(44,102,184,0.14)'); g.addColorStop(0.5, 'rgba(44,102,184,0.05)'); g.addColorStop(1, 'rgba(6,10,12,0)');
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

  // Copy buttons copy their command: prompts and # comments stripped.
  for (const btn of document.querySelectorAll('.copy')) {
    const pre = document.getElementById(btn.dataset.copy);
    btn.addEventListener('click', async () => {
      const text = pre.innerText.split('\n').map(l => l.replace(/\s+#.*$/, '')).filter(l => !l.startsWith('#')).join('\n').trim();
      try { await navigator.clipboard.writeText(text); btn.textContent = 'Copied'; }
      catch { const r = document.createRange(); r.selectNodeContents(pre); getSelection().removeAllRanges(); getSelection().addRange(r); btn.textContent = 'Selected'; }
      setTimeout(() => { btn.textContent = 'Copy'; }, 1800);
    });
  }
})();
