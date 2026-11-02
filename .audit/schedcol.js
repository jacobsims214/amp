(() => {
  // Find the board's split: left list vs right scheduled
  const sched = [...document.querySelectorAll('main *')].find(e=>/^SCHEDULED$/i.test((e.textContent||'').trim()))
  let container = sched
  for (let i=0;i<5 && container;i++){ if (getComputedStyle(container).display==='flex' || getComputedStyle(container).display==='grid') break; container = container.parentElement }
  const r = container ? container.getBoundingClientRect() : null
  // left list width
  const leftList = [...document.querySelectorAll('main *')].find(e=>/Q4 STOREFRONT/i.test(e.textContent||'') && e.children.length<4)
  return { containerCls: container?(typeof container.className==='string'?container.className:'').slice(0,80):null, containerW: r?Math.round(r.width):null, vw: document.documentElement.clientWidth }
})()
