// Flatten a markdown string to plain single-line text, for one-line contexts
// (truncated cards, tooltips) where raw markers would show up as noise.
export function stripMarkdown(src: string): string {
  if (!src) return ''
  return src
    .replace(/```[a-z]*\n?/gi, '')
    .replace(/!\[([^\]]*)\]\([^)]*\)/g, '$1')
    .replace(/\[([^\]]*)\]\([^)]*\)/g, '$1')
    .replace(/^\s{0,3}#{1,6}\s+/gm, '')
    .replace(/^\s*(?:[-*+]|\d+[.)])\s+/gm, '')
    .replace(/^\s*>\s?/gm, '')
    .replace(/`([^`]*)`/g, '$1')
    .replace(/(\*\*|__)(.+?)\1/g, '$2')
    // Emphasis only: not inside a word (2*3*4), and the closing * must follow
    // a non-space, so globs like "*.go and *.ts" keep their asterisks.
    .replace(/(^|[^\w*])\*(?![\s*])([^*]*?[^\s*])\*(?![\w*])/g, '$1$2')
    .replace(/(^|[^\w])_(?!\s)([^_]+?)_(?=$|[^\w])/g, '$1$2')
    .replace(/~~(.+?)~~/g, '$1')
    .replace(/\s+/g, ' ')
    .trim()
}
