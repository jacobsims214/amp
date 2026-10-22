import { describe, expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { MemoryRouter } from 'react-router-dom'
import { PlanApprovalBanner, type PlanApprovalBannerProps } from './PlanApprovalBanner'
import type { Project, Task } from '../types'
import type { Person } from '../lib/people'

const now = Date.parse('2026-10-22T12:00:00Z')
const people = new Map<number, Person>([[7, { id: 7, name: 'Jo Person', email: 'jo@example.com' }]])
const project = (over: Partial<Project> = {}): Project => ({
  id: 3, name: 'P', code: 'P', description: '', state: 'active', created_at: '', updated_at: '',
  require_plan_approval: true, plan_approved_at: '2026-10-21T15:30:00Z', plan_approved_by: 7, ...over,
})
const task = (id: number, over: Partial<Task> = {}): Task => ({
  id, project_id: 3, epic_id: 1, story_id: 1, name: `Ticket ${id}`, description: '', acceptance_criteria: '',
  state: 'backlog', priority: '1', dependency_ids: [], created_by: 7, assigned_to: 'amp-worker-frontend',
  created_at: '2026-10-22T11:00:00Z', planned_at: '2026-10-22T11:00:00Z', updated_at: '', ...over,
})

const render = (p: Partial<PlanApprovalBannerProps>) => renderToStaticMarkup(
  <MemoryRouter>
    <PlanApprovalBanner projectId={3} project={project()} awaiting={[]} people={people}
      onApprove={() => {}} approving={false} error={null} now={now} {...p} />
  </MemoryRouter>,
)
// The rendered text: tags dropped, entities decoded.
const text = (html: string) => html.replace(/<[^>]+>/g, '').replace(/&#x27;/g, "'").replace(/&quot;/g, '"').replace(/&amp;/g, '&').replace(/\s+/g, ' ')

describe('PlanApprovalBanner', () => {
  it('lists what waits, who planned it and for which agent, with one Approve button', () => {
    const html = render({ awaiting: [task(11), task(12, { performer: 'human', created_by: null })] })
    const t = text(html)
    expect(t).toContain('2 tickets wait for your approval')
    expect(t).toContain('Ticket 11')
    expect(t).toContain('planned by Jo Person')
    expect(t).toContain('for amp-worker-frontend')
    expect(t).toContain('planned by an agent')
    expect(t).toContain('for a person')
    expect(t).toContain('1h ago')
    expect(t).toContain("Agents can't start them until a person approves")
    expect(html.match(/Approve plan/g)).toHaveLength(1)
    expect(html).toContain('id="approve"')
  })

  it('shows who approved last and when', () => {
    const t = text(render({ awaiting: [task(11)] }))
    expect(t).toMatch(/Last approved .+ by Jo Person\./)
  })

  it('says when the plan was never approved', () => {
    const t = text(render({ awaiting: [task(11)], project: project({ plan_approved_at: undefined, plan_approved_by: undefined }) }))
    expect(t).toContain('This plan has never been approved.')
  })

  it('credits the upgrade for a grandfathered approval with no person', () => {
    const t = text(render({ project: project({ plan_approved_by: undefined }) }))
    expect(t).toContain('the AMP upgrade')
  })

  it('on the board: a few tickets, a link to the Flight plan, and nothing when nothing waits', () => {
    const many = [11, 12, 13, 14, 15].map(id => task(id))
    const html = render({ variant: 'compact', awaiting: many })
    expect(html).toContain('href="/project/3/plan#approve"')
    expect(text(html)).toContain('and 2 more')
    expect(text(html)).not.toContain('Ticket 14')
    expect(render({ variant: 'compact', awaiting: [] })).toBe('')
  })

  it('on the Flight plan with nothing waiting: a quiet approved line, no button', () => {
    const html = render({ awaiting: [] })
    expect(text(html)).toContain('Plan approved')
    expect(html).not.toContain('Approve plan')
  })

  it('says agents can already start when the project does not require approval', () => {
    const t = text(render({ awaiting: [task(11)], project: project({ require_plan_approval: false }) }))
    expect(t).toContain("doesn't require approval")
  })

  it('shows the refusal or failure from approving', () => {
    expect(text(render({ awaiting: [task(11)], error: 'Plan approval is done by a person in the AMP web UI' })))
      .toContain('Plan approval is done by a person')
  })

  it('links ticket ids to the board drawer when there is no drawer on the page', () => {
    expect(render({ awaiting: [task(11)] })).toContain('href="/project/3/board?task=11"')
  })
})
