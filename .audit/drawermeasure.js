(() => {
  const drawer = [...document.querySelectorAll('div')].find(d => d.className.includes('w-[520px]') && d.className.includes('fixed'))
  if (!drawer) return 'no drawer'
  const r = drawer.getBoundingClientRect()
  const scroller = drawer.querySelector('[class*=overflow]') || drawer
  return {
    drawerRect: { left: Math.round(r.left), right: Math.round(r.right), w: Math.round(r.width) },
    viewport: document.documentElement.clientWidth,
    offscreenLeft: Math.round(r.left) < 0 ? Math.round(-r.left) : 0,
    scrolls: { drawerScrollW: drawer.scrollWidth, drawerClientW: drawer.clientWidth },
    titleCut: (() => { const h = drawer.querySelector('h1,h2,h3'); if(!h) return null; const hr = h.getBoundingClientRect(); return { left: Math.round(hr.left), text: (h.textContent||'').slice(0,30) } })(),
  }
})()
