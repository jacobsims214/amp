import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

/** The content of the document's `<meta name="viewport">`, or null when it has none. */
const viewportMeta = (html: string): string | null =>
  html.match(/<meta\s+name="viewport"\s+content="([^"]*)"/i)?.[1] ?? null

/**
 * `viewport-fit=cover` is what makes `env(safe-area-inset-*)` non-zero on a real device: without
 * it iOS/Android letterbox the page inside the safe area and every inset resolves to 0px. The
 * session page's safe-area padding (audit #3026 F6) is inert without it, and devtools emulation
 * can't show the difference (it reports 0 either way), so this is the check that catches a
 * regression — the on-device confirmation lives on #3066.
 */
describe('the app document opts into the safe-area insets (#4958)', () => {
  const html = readFileSync(resolve(process.cwd(), 'index.html'), 'utf8')

  it('index.html sets viewport-fit=cover', () => {
    const content = viewportMeta(html)
    expect(content, 'index.html has a viewport meta').not.toBeNull()
    expect(content).toContain('viewport-fit=cover')
    // The rest of the tag stays what it was: cover alone would break the phone layout.
    expect(content).toContain('width=device-width')
    expect(content).toContain('initial-scale=1')
  })

  it('catches a meta that dropped it', () => {
    expect(viewportMeta('<meta name="viewport" content="width=device-width, initial-scale=1.0" />')).not.toContain('viewport-fit=cover')
    expect(viewportMeta('<meta name="viewport" content="width=device-width, initial-scale=1.0, viewport-fit=cover" />')).toContain('viewport-fit=cover')
    expect(viewportMeta('<meta charset="UTF-8" />')).toBeNull()
  })

  it.each(['public/home/index.html', 'public/home/harness/index.html'])('%s keeps it too', file => {
    expect(viewportMeta(readFileSync(resolve(process.cwd(), file), 'utf8'))).toContain('viewport-fit=cover')
  })
})
