(() => {
  const out = {}
  const tabNav = [...document.querySelectorAll('nav')].find(n => /Views/i.test(n.getAttribute('aria-label')||''))
  if (tabNav) out.tabs = { scrollW: tabNav.scrollWidth, clientW: tabNav.clientWidth, canScrollX: tabNav.scrollWidth > tabNav.clientWidth, lastTabCut: (() => { const t=[...tabNav.querySelectorAll('a')].pop(); if(!t) return null; const r=t.getBoundingClientRect(); return { text:t.textContent.trim(), right: Math.round(r.right), vw: document.documentElement.clientWidth } })() }
  // readouts overlap
  const labels = [...document.querySelectorAll('main .amp-label')].map(e=>{const r=e.getBoundingClientRect();return {t:(e.textContent||'').trim().slice(0,18), left:Math.round(r.left), right:Math.round(r.right)}})
  out.overlappingLabels = []
  for (let i=0;i<labels.length;i++) for (let j=i+1;j<labels.length;j++) {
    if (labels[i].right > labels[j].left + 4 && labels[i].left < labels[j].right) out.overlappingLabels.push([labels[i].t, labels[j].t])
  }
  out.viewportW = document.documentElement.clientWidth
  out.docScrollW = document.documentElement.scrollWidth
  return out
})()
