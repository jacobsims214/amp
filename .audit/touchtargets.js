(() => {
  const small = []
  for (const el of document.querySelectorAll('main a, main button, aside a, aside button, main input, main select, [role=button]')) {
    const r = el.getBoundingClientRect()
    if (r.width === 0 || r.height === 0) continue
    if (r.width < 24 || r.height < 24) {
      small.push({ tag: el.tagName.toLowerCase(), text: (el.textContent||el.getAttribute('aria-label')||'').trim().slice(0,30), w: Math.round(r.width), h: Math.round(r.height) })
    }
  }
  // font sizes below 12px
  const tiny = new Set()
  for (const el of document.querySelectorAll('main *')) {
    const fs = parseFloat(getComputedStyle(el).fontSize)
    if (fs > 0 && fs < 11 && (el.textContent||'').trim()) tiny.add(fs + 'px: ' + (el.textContent||'').trim().slice(0,25))
  }
  // viewport meta
  const vp = document.querySelector('meta[name=viewport]')?.content
  return { smallTargets: small.slice(0, 20), count: small.length, tinyText: [...tiny].slice(0, 10), viewportMeta: vp }
})()
