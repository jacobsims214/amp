import { describe, expect, it } from 'vitest'

// KB decisions/human-work-ux.md §5: copy that was retired when human work moved
// to the project's My work tab. None of it may come back in the UI source.
// Tests are skipped because they assert these strings are absent.
const RETIRED: RegExp[] = [
  /to do myself/i,
  /agents wait on these/i,
  /waiting on a person/i,
  /done, not merged/i,
  /approve in flight plan/i,
  /merged after one round of changes/i,
  /ticket drawer or my work/i,
  /review[- ]and[- ]merge/i,
]

const sources = import.meta.glob<string>('../**/*.{ts,tsx}', { query: '?raw', import: 'default', eager: true })

describe('retired human-work copy', () => {
  const files = Object.entries(sources).filter(([path]) => !/\.test\.tsx?$/.test(path))

  it('reads the UI source', () => {
    expect(files.length).toBeGreaterThan(50)
  })

  it.each(RETIRED.map(r => [r.source, r] as const))('nothing says %s', (_, re) => {
    const hits = files.filter(([, src]) => re.test(src)).map(([path]) => path)
    expect(hits).toEqual([])
  })
})
