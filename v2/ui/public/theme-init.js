// Theme before first paint (no flash). Keep in step with src/lib/theme.ts.
// An external file, not inline: the CSP is script-src 'self'.
(function () {
  var t = 'system';
  try { var v = localStorage.getItem('amp.theme'); if (v === 'light' || v === 'dark') t = v; } catch (e) {}
  var dark = t === 'dark' || (t === 'system' && window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches);
  document.documentElement.setAttribute('data-theme', dark ? 'dark' : 'light');
  var m = document.querySelector('meta[name="theme-color"]');
  if (m) m.setAttribute('content', dark ? '#101215' : '#F2F2F0');
})();
