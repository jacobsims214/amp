(() => {
  // nested scroll regions inside main and their stickiness
  const regions = []
  for (const el of document.querySelectorAll('main *')) {
    const cs = getComputedStyle(el)
    if ((cs.overflowY === 'auto' || cs.overflowY === 'scroll') && el.scrollHeight > el.clientHeight + 10) {
      regions.push({ cls: (typeof el.className==='string'?el.className:'').slice(0,60), h: Math.round(el.clientHeight), scrollH: el.scrollHeight })
    }
  }
  const sticky = []
  for (const el of document.querySelectorAll('main *, aside *')) {
    const cs = getComputedStyle(el)
    if (cs.position === 'sticky' || cs.position === 'fixed') sticky.push({ tag: el.tagName.toLowerCase(), pos: cs.position, cls: (typeof el.className==='string'?el.className:'').slice(0,50) })
  }
  return { scrollRegions: regions.slice(0,8), stickyEls: sticky.slice(0,10) }
})()
