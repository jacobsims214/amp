(() => {
  // The board body: find the flex row that holds the epic list and the scheduled panel
  const schedHeader = [...document.querySelectorAll('main *')].find(e=>/^SCHEDULED$/i.test((e.textContent||'').trim()) && e.children.length<3)
  let row = schedHeader
  for (let i=0;i<8 && row;i++){ const cs=getComputedStyle(row); if ((cs.display==='flex'||cs.display==='grid') && row.children.length>1) break; row=row.parentElement }
  if (!row) return 'none'
  const kids=[...row.children].map(c=>({w:Math.round(c.getBoundingClientRect().width), cls:(typeof c.className==='string'?c.className:'').slice(0,50)}))
  return { rowCls:(typeof row.className==='string'?row.className:'').slice(0,80), kids, vw:document.documentElement.clientWidth }
})()
