import { describe, expect, it } from 'vitest'

// Markdown everywhere goes through the shared renderers (#133): only
// Markdown.tsx (house style, link handling, gfm) and SafeMarkdown.tsx (remote
// sessions) import react-markdown directly.
const sources = import.meta.glob<string>('../**/*.{ts,tsx}', { query: '?raw', import: 'default', eager: true })

describe('react-markdown imports', () => {
  it('only the shared renderers import react-markdown', () => {
    const importers = Object.entries(sources)
      .filter(([path, src]) => !/\.test\.tsx?$/.test(path) && /from ['"]react-markdown['"]/.test(src))
      .map(([path]) => path.replace(/^.*\//, ''))
      .sort()
    expect(importers).toEqual(['Markdown.tsx', 'SafeMarkdown.tsx'])
  })
})
