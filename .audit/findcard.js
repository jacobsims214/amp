(() => {
  const all = [...document.querySelectorAll('main *')]
  const hits = all.filter(e => /cart abandonment/i.test(e.textContent||""))
  return { total: all.length, hits: hits.slice(0,3).map(e => ({tag: e.tagName, cls: (typeof e.className==='string'?e.className:'').slice(0,60)})) }
})()
