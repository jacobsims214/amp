(() => {
  const btn = [...document.querySelectorAll('main button')].find(b => /cart abandonment/i.test(b.textContent||""))
  if (!btn) return 'no-card'
  btn.scrollIntoView({block:'center'})
  btn.click()
  return 'clicked'
})()
