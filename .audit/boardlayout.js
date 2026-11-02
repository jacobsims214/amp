(() => {
  const sched = [...document.querySelectorAll('main *')].find(e=>/^SCHEDULED$/i.test((e.textContent||'').trim()) && e.children.length<3)
  const cols = [...document.querySelectorAll('main [class*=grid], main [class*=flex]')].filter(e=>{const cs=getComputedStyle(e); return cs.display==='grid' && cs.gridTemplateColumns.split(' ').length>1}).slice(0,4).map(e=>({cls:(typeof e.className==='string'?e.className:'').slice(0,70), cols:getComputedStyle(e).gridTemplateColumns}))
  return { hasScheduledPanel: !!sched, gridCols: cols }
})()
