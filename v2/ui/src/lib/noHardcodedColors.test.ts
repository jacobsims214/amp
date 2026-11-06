import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join, relative } from 'node:path'
import { describe, expect, it } from 'vitest'

// Colors come from the tokens in index.css so a theme (dark mode) reaches every
// surface. bg-white / text-black / bg-black are fixed colors and do not flip.
const ALLOWLIST = new Set([
  // Artifact previews render someone's HTML/SVG/image document, which is
  // authored for a white page whatever theme the app is in.
  'pages/ArtifactView.tsx',
  'pages/Artifacts.tsx',
])
const BANNED = /(?<![\w-])(?:hover:|group-hover:|focus:)?(?:bg|text|border|ring|fill|stroke)-(?:white|black)(?![\w-])/

function walk(dir: string): string[] {
  return readdirSync(dir).flatMap(f => {
    const p = join(dir, f)
    return statSync(p).isDirectory() ? walk(p) : [p]
  })
}

describe('hard-coded colors', () => {
  it('has no bg-white / text-black style utilities outside the allowlist', () => {
    const root = join(__dirname, '..')
    const hits: string[] = []
    for (const f of walk(root)) {
      if (!/\.tsx?$/.test(f) || /\.test\.tsx?$/.test(f)) continue
      const rel = relative(root, f)
      if (ALLOWLIST.has(rel)) continue
      readFileSync(f, 'utf8').split('\n').forEach((line, i) => {
        if (BANNED.test(line)) hits.push(`${rel}:${i + 1}: ${line.trim().slice(0, 100)}`)
      })
    }
    expect(hits, 'use a token (bg-surface, text-ink, text-on-accent) instead').toEqual([])
  })

  it('has no literal #fff / #000 / white / black colors in CSS or inline styles', () => {
    const root = join(__dirname, '..')
    const LITERAL = /(?:color|background(?:-color)?|border(?:-color)?|fill|stroke)\s*:\s*(?:#fff(?:fff)?|#000(?:000)?|white|black)\b|:\s*['"](?:#fff(?:fff)?|#000(?:000)?|white|black)['"]/i
    const hits: string[] = []
    for (const f of walk(root)) {
      if (!/\.(css|tsx?)$/.test(f) || /\.test\.tsx?$/.test(f)) continue
      const rel = relative(root, f)
      if (ALLOWLIST.has(rel)) continue
      readFileSync(f, 'utf8').split('\n').forEach((line, i) => {
        if (LITERAL.test(line)) hits.push(`${rel}:${i + 1}: ${line.trim().slice(0, 100)}`)
      })
    }
    expect(hits, 'use a token (var(--on-accent), var(--surface)) instead').toEqual([])
  })

  it('every light token has a dark value', () => {
    const css = readFileSync(join(__dirname, '..', 'index.css'), 'utf8')
    const block = (re: RegExp) => [...(css.match(re)?.[1] ?? '').matchAll(/(--[a-z0-9-]+):/g)].map(m => m[1])
    const light = block(/:root \{([^}]*)\}/).filter(v => !/^--(font|radius)/.test(v))
    const dark = new Set(block(/:root\[data-theme="dark"\] \{([^}]*)\}/))
    expect(light.filter(v => !dark.has(v))).toEqual([])
  })
})
