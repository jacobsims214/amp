import { describe, expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { MemoryRouter } from 'react-router-dom'
import { PlanApprovalBanner, PlanApprovalView, type ApproveSelection, type PlanApprovalBannerProps, type PlanApprovalViewProps } from './PlanApprovalBanner'
import type { Project, Task } from '../types'
import type { Person } from '../lib/people'

const now = Date.parse('2026-10-22T12:00:00Z')
const people = new Map<number, Person>([
  [7, { id: 7, name: 'Jo Person', email: 'jo@example.com' }],
  [9, { id: 9, name: 'Bob Builder', email: 'bob@example.com' }],
  [10, { id: 10, name: 'Carol Coder', email: 'carol@example.com' }],
])
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
    <PlanApprovalBanner projectId={3} project={project()} awaiting={[]} people={people} meId={7} myAccess="member"
      onApprove={() => {}} approving={false} error={null} now={now} {...p} />
  </MemoryRouter>,
)
// The view, called as a function (no DOM in these tests), with ticks and a confirm step.
const view = (p: Partial<PlanApprovalViewProps>) => PlanApprovalView({
  projectId: 3, project: project(), awaiting: [], people, meId: 7, myAccess: 'member', onApprove: () => {}, approving: false, error: null, now,
  ticks: { untickedYours: new Set(), tickedOthers: new Set() }, onToggle: () => {}, confirming: false, onConfirming: () => {}, ...p,
})
const html = (el: unknown) => renderToStaticMarkup(<MemoryRouter>{el as React.ReactElement}</MemoryRouter>)
// A button in a rendered element tree whose text contains label.
type El = { type?: unknown; props?: { children?: unknown; onClick?: () => void } }
const textOf = (n: unknown): string => typeof n === 'string' || typeof n === 'number' ? String(n)
  : Array.isArray(n) ? n.map(textOf).join('') : n && typeof n === 'object' ? textOf((n as El).props?.children) : ''
function findButton(n: unknown, label: string): { props: { onClick: () => void } } | null {
  if (Array.isArray(n)) { for (const c of n) { const f = findButton(c, label); if (f) return f } return null }
  if (!n || typeof n !== 'object') return null
  const e = n as El
  if (e.type === 'button' && textOf(e).includes(label) && e.props?.onClick) return e as { props: { onClick: () => void } }
  return findButton(e.props?.children, label)
}
// The rendered text: tags dropped, entities decoded.
const text = (html: string) => html.replace(/<[^>]+>/g, '').replace(/&#x27;/g, "'").replace(/&quot;/g, '"').replace(/&amp;/g, '&').replace(/\s+/g, ' ')

const mine = (id: number, over: Partial<Task> = {}) => task(id, { owner_user_id: 7, ...over })
const bobs = (id: number, over: Partial<Task> = {}) => task(id, { owner_user_id: 9, created_by: 9, ...over })
const carols = (id: number, over: Partial<Task> = {}) => task(id, { owner_user_id: 10, created_by: 10, ...over })

describe('PlanApprovalBanner (Flight plan)', () => {
  it('lists what waits, who planned it and for which agent', () => {
    const t = text(render({ awaiting: [mine(11), mine(12, { performer: 'human', created_by: null })] }))
    expect(t).toContain('2 of your tickets wait for your approval')
    expect(t).toContain('Ticket 11')
    expect(t).toContain('planned by Jo Person')
    expect(t).toContain('for amp-worker-frontend')
    expect(t).toContain('planned by an agent')
    expect(t).toContain('for a person')
    expect(t).toContain('1h ago')
    expect(t).toContain("Agents can't start a ticket until a person approves it")
  })

  it('groups Yours and Others\' with owners, and ticks your own by default', () => {
    const out = render({ awaiting: [mine(11), bobs(12), task(13, { created_by: 10 })] })
    const t = text(out)
    expect(t).toContain('Yours · 1')
    expect(t).toContain("Others' · 2")
    expect(t).toContain('owner Bob Builder')
    expect(t).toContain('owner Carol Coder (unassigned, planned by them)')
    expect(t).toContain('1 of your tickets waits for your approval')
    expect(t).toContain("2 of other people's wait for their owners")
    // yours ticked, others' not
    expect(out).toMatch(/<input type="checkbox"[^>]*aria-label="Approve #11 \(yours\)" checked=""/)
    expect(out).not.toMatch(/aria-label="#12[^"]*" [^>]*checked=""/)
    expect(t).toContain('Approve selected (1)')
  })

  it('approves the ticked tickets as of the newest of them, verbatim', () => {
    let got: ApproveSelection | null = null
    const awaiting = [mine(11, { planned_at: '2026-10-22T11:00:00.123456Z' }), mine(12, { planned_at: '2026-10-22T10:00:00Z' }), bobs(13, { planned_at: '2026-10-22T11:30:00Z' })]
    const button = findButton(view({ awaiting, onApprove: s => { got = s } }), 'Approve selected')
    expect(button).toBeTruthy()
    button!.props.onClick()
    expect(got).toEqual({ taskIds: [11, 12], asOf: '2026-10-22T11:00:00.123456Z', onBehalf: false, shown: [11, 12, 13] })
  })

  it('leaves out your own tickets you unticked', () => {
    let got: ApproveSelection | null = null
    const awaiting = [mine(11, { planned_at: '2026-10-22T11:00:00Z' }), mine(12, { planned_at: '2026-10-22T10:00:00Z' })]
    findButton(view({ awaiting, ticks: { untickedYours: new Set([11]), tickedOthers: new Set() }, onApprove: s => { got = s } }), 'Approve selected (1)')!.props.onClick()
    expect(got).toMatchObject({ taskIds: [12], asOf: '2026-10-22T10:00:00Z', onBehalf: false })
  })

  it('disables Approve when nothing is ticked', () => {
    const out = html(view({ awaiting: [mine(11)], ticks: { untickedYours: new Set([11]), tickedOthers: new Set() } }))
    expect(out).toMatch(/<button[^>]*disabled=""[^>]*>.*Approve selected \(0\)/)
  })

  it("for a Member: others' tickets can't be ticked, and says why", () => {
    const out = render({ awaiting: [mine(11), bobs(12)] })
    expect(out).toMatch(/<input type="checkbox"[^>]*disabled=""[^>]*aria-label="#12 is Bob Builder&#x27;s: only a project lead can approve it on their behalf"/)
    expect(text(out)).toContain("Only project leads, owners and admins can approve someone else's ticket")
    // even a stale tick on someone else's ticket isn't sent
    let got: ApproveSelection | null = null
    findButton(view({ awaiting: [mine(11), bobs(12)], ticks: { untickedYours: new Set(), tickedOthers: new Set([12]) }, onApprove: s => { got = s } }), 'Approve selected')!.props.onClick()
    expect(got).toMatchObject({ taskIds: [11], onBehalf: false })
  })

  it('for a lead: ticking others\' tickets turns the button into "Approve on behalf of", which asks to confirm first', () => {
    let got: ApproveSelection | null = null
    let confirming = false
    const awaiting = [mine(11), bobs(12), carols(13)]
    const ticks = { untickedYours: new Set<number>(), tickedOthers: new Set([12, 13]) }
    const el = view({ awaiting, myAccess: 'lead', ticks, onApprove: s => { got = s }, onConfirming: on => { confirming = on } })
    expect(text(html(el))).toContain('Approve on behalf of Bob Builder and Carol Coder (3)')
    findButton(el, 'Approve on behalf of')!.props.onClick()
    expect(confirming).toBe(true)
    expect(got).toBeNull()

    const asking = view({ awaiting, myAccess: 'lead', ticks, confirming: true, onApprove: s => { got = s } })
    const t = text(html(asking))
    expect(t).toContain("You're approving 2 tickets that aren't yours, on behalf of Bob Builder and Carol Coder, along with 1 ticket of your own")
    expect(t).toContain('records that you approved on their behalf')
    expect(findButton(asking, 'Approve on behalf of')).toBeNull()
    findButton(asking, 'Yes, approve on their behalf')!.props.onClick()
    expect(got).toMatchObject({ taskIds: [11, 12, 13], onBehalf: true })
  })

  it('cancelling the confirm approves nothing', () => {
    let confirming = true
    let called = false
    const asking = view({ awaiting: [bobs(12)], myAccess: 'owner', ticks: { untickedYours: new Set(), tickedOthers: new Set([12]) }, confirming: true,
      onConfirming: on => { confirming = on }, onApprove: () => { called = true } })
    findButton(asking, 'Cancel')!.props.onClick()
    expect(confirming).toBe(false)
    expect(called).toBe(false)
  })

  it('with none of yours waiting, says so and leaves others\' unticked', () => {
    const t = text(render({ awaiting: [bobs(12)], myAccess: 'lead' }))
    expect(t).toContain('None of your tickets wait for approval')
    expect(t).toContain('Approve selected (0)')
    expect(t).toContain("tick one to approve it on its owner's behalf")
  })

  it('shows who approved last and when', () => {
    expect(text(render({ awaiting: [mine(11)] }))).toMatch(/Last approved .+ by Jo Person\./)
  })

  it('says when the plan was never approved', () => {
    const t = text(render({ awaiting: [mine(11)], project: project({ plan_approved_at: undefined, plan_approved_by: undefined }) }))
    expect(t).toContain('This plan has never been approved.')
  })

  it('credits the upgrade for a grandfathered approval with no person', () => {
    expect(text(render({ project: project({ plan_approved_by: undefined }) }))).toContain('the AMP upgrade')
  })

  it('says when tickets came in after the page loaded', () => {
    const t = text(render({ awaiting: [mine(11)], notice: '2 tickets were added or changed since this page loaded' }))
    expect(t).toContain('2 tickets were added or changed since this page loaded')
  })

  it('with nothing waiting: a quiet approved line, no button', () => {
    const out = render({ awaiting: [] })
    expect(text(out)).toContain('Plan approved')
    expect(out).not.toContain('<button')
  })

  it('says agents can already start when the project does not require approval', () => {
    expect(text(render({ awaiting: [mine(11)], project: project({ require_plan_approval: false }) }))).toContain("doesn't require approval")
  })

  it('shows the refusal or failure from approving', () => {
    expect(text(render({ awaiting: [mine(11)], error: 'tickets 12 are not yours: send on_behalf: true (project leads, owners and admins only)' })))
      .toContain('send on_behalf: true')
  })

  it('links ticket ids to the board drawer when there is no drawer on the page', () => {
    expect(render({ awaiting: [mine(11)] })).toContain('href="/project/3/board?task=11"')
  })
})

describe('PlanApprovalBanner (board, compact)', () => {
  it('shows a few of your tickets, "and N more of yours", and a link to the Flight plan', () => {
    const out = render({ variant: 'compact', awaiting: [11, 12, 13, 14, 15].map(id => mine(id)) })
    expect(out).toContain('href="/project/3/plan#approve"')
    expect(text(out)).toContain('and 2 more of yours')
    expect(text(out)).not.toContain('Ticket 14')
    expect(render({ variant: 'compact', awaiting: [] })).toBe('')
  })

  it('offers "Approve yours (N)" only when it lists all of yours', () => {
    const many = text(render({ variant: 'compact', awaiting: [11, 12, 13, 14].map(id => mine(id)) }))
    expect(many).not.toContain('Approve yours')
    expect(many).toContain('Review all 4 of yours in Flight plan')
    const few = text(render({ variant: 'compact', awaiting: [mine(11), mine(12)] }))
    expect(few).toContain('Approve yours (2)')
    expect(few).toContain('Review in Flight plan')
  })

  it("approves only your own, never others', even for a lead", () => {
    let got: ApproveSelection | null = null
    const awaiting = [mine(11, { planned_at: '2026-10-22T11:00:00.5Z' }), bobs(12)]
    const el = view({ variant: 'compact', awaiting, myAccess: 'lead', onApprove: s => { got = s } })
    const t = text(html(el))
    expect(t).not.toContain('Ticket 12')
    expect(t).not.toContain('on behalf')
    expect(t).toContain("1 other person's ticket waits for its owner")
    findButton(el, 'Approve yours')!.props.onClick()
    expect(got).toEqual({ taskIds: [11], asOf: '2026-10-22T11:00:00.5Z', onBehalf: false, shown: [11] })
  })

  it("with only others' tickets waiting: one line and a link, no Approve", () => {
    const out = render({ variant: 'compact', awaiting: [bobs(12), carols(13)] })
    expect(text(out)).toContain("2 tickets wait for their owners' approval. None of them are yours.")
    expect(out).toContain('href="/project/3/plan#approve"')
    expect(out).not.toContain('<button')
  })
})
