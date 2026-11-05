import { describe, expect, it } from 'vitest'

// #128: the UI has one name for the agent that plans your work, "manager
// agent", always through <ManagerAgent />, which links to the guide page
// that explains it. The old names may not come back.
const OLD: RegExp[] = [/planner agent/i, /\bamp-manager\b/i]

const sources = import.meta.glob<string>('../**/*.{ts,tsx}', { query: '?raw', import: 'default', eager: true })

describe('one name for the manager agent', () => {
  const files = Object.entries(sources).filter(([path]) => !/\.test\.tsx?$/.test(path) && !path.endsWith('/ManagerAgent.tsx'))

  it('reads the UI source', () => {
    expect(files.length).toBeGreaterThan(50)
  })

  it.each(OLD.map(r => [r.source, r] as const))('nothing says %s', (_, re) => {
    expect(files.filter(([, src]) => re.test(src)).map(([path]) => path)).toEqual([])
  })
})
