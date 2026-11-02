(() => {
  const main = document.querySelector('main')
  const aside = document.querySelector('aside')
  const de = document.documentElement
  // Find the widest elements inside main that overflow its client width
  const mainRight = main ? main.getBoundingClientRect().right : 0
  const offenders = []
  if (main) {
    for (const el of main.querySelectorAll('*')) {
      const r = el.getBoundingClientRect()
      if (r.right > mainRight + 2 || r.left < main.getBoundingClientRect().left - 2) {
        const cs = getComputedStyle(el)
        if (r.width > 4 && cs.position !== 'fixed') {
          offenders.push({
            tag: el.tagName.toLowerCase(),
            cls: (el.className && typeof el.className === 'string') ? el.className.slice(0, 80) : '',
            text: (el.textContent || '').trim().slice(0, 40),
            left: Math.round(r.left), right: Math.round(r.right), w: Math.round(r.width),
          })
        }
      }
      if (offenders.length > 14) break
    }
  }
  return JSON.stringify({
    viewport: { w: de.clientWidth, scrollW: de.scrollWidth, bodyScrollW: document.body.scrollWidth },
    aside: aside ? { w: Math.round(aside.getBoundingClientRect().width), cls: aside.className.slice(0, 120) } : null,
    main: main ? { clientW: main.clientWidth, scrollW: main.scrollWidth, canScrollX: main.scrollWidth > main.clientWidth } : null,
    offenders,
  })
})()
