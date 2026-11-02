(() => {
  const btn = [...document.querySelectorAll('main button')].find(b => /cart abandonment/i.test(b.textContent||""))
  if (btn) { btn.scrollIntoView({block:'center'}); btn.click() }
  return new Promise(res => setTimeout(() => {
    const drawer = [...document.querySelectorAll('div')].find(d => typeof d.className === 'string' && d.className.includes('w-[520px]'))
    if (!drawer) return res('no drawer')
    const r = drawer.getBoundingClientRect()
    res({
      drawerRect: { left: Math.round(r.left), right: Math.round(r.right), w: Math.round(r.width) },
      viewport: document.documentElement.clientWidth,
      docScrollW: document.documentElement.scrollWidth,
      offscreenLeftPx: Math.max(0, Math.round(-r.left)),
    })
  }, 800))
})()
