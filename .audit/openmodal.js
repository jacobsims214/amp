(() => {
  const btn = [...document.querySelectorAll('button')].find(b => /EPIC/i.test(b.textContent||""))
  if (!btn) return 'no epic btn'
  btn.click()
  return new Promise(res => setTimeout(() => {
    const fixed = [...document.querySelectorAll('div.fixed')].filter(d => d.className.includes('z-50'))
    const m = fixed.map(d => d.querySelector('.max-w-lg, .max-w-md, [class*=max-w]')).find(Boolean)
    if (!m) return res({fixedCount: fixed.length, found: fixed.map(d=>d.className.slice(0,60))})
    const r = m.getBoundingClientRect()
    res({ modalRect: {left:Math.round(r.left), right:Math.round(r.right), w:Math.round(r.width)}, viewport: document.documentElement.clientWidth })
  }, 700))
})()
