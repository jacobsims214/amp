(() => {
  // effective CSS px at 320 already tested reflow. Here: confirm no maximum-scale lock and font bootstrap.
  const vp = document.querySelector('meta[name=viewport]')?.content || ''
  const locksZoom = /maximum-scale\s*=\s*1|user-scalable\s*=\s*no/i.test(vp)
  // focus outline present?
  const btn = document.querySelector('main button, main a')
  const prevOutline = btn ? getComputedStyle(btn).outlineWidth : null
  return { viewportMeta: vp, locksZoom, pinchZoomAllowed: !locksZoom }
})()
