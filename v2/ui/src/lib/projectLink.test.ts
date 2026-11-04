import { describe, expect, it } from 'vitest'
import { projectLink, viewLink } from './projectLink'

describe('projectLink', () => {
  it('builds /project/:id[/<view>][?focus=]', () => {
    expect(projectLink(1)).toBe('/project/1')
    expect(projectLink(1, 'board')).toBe('/project/1/board')
    expect(projectLink(1, 'dag', 34)).toBe('/project/1/dag?focus=34')
  })
})

describe('viewLink (sidebar and view tabs)', () => {
  it('carries ?focus= to a view that honors it', () => {
    expect(viewLink(1, 'plan', '?focus=34')).toBe('/project/1/plan?focus=34')
    expect(viewLink('1', 'my-work', '?status=waiting&focus=34')).toBe('/project/1/my-work?focus=34')
  })

  it('drops everything else, and focus on views that ignore it', () => {
    expect(viewLink(1, 'plan', '?status=waiting&q=x')).toBe('/project/1/plan')
    expect(viewLink(1, 'settings', '?focus=34')).toBe('/project/1/settings')
    expect(viewLink(1, 'board', '?focus=nope')).toBe('/project/1/board')
  })
})
