/**
 * Canonical, project-independent links (v2/docs/url-state.md):
 *
 * - `/t/<ref>`: a ticket. `BA-34`, `ba-34` and `34` all open ticket 34
 *   (ids are global; the code is for people). It lands on the ticket's
 *   project Board with the drawer open, keeping every other param, so
 *   `/t/BA-34?tab=details` opens the Details tab and `?run=12` the run. A
 *   code that isn't the ticket's project's opens it anyway, with a one-line
 *   notice in the drawer (`?ref=`).
 * - `/r/<run id>`: a run. It opens the run's ticket with `?run=<id>`: the
 *   drawer scrolls to that run and highlights it.
 *
 * Both replace the history entry, so Back skips the hop. A ticket or run
 * that doesn't exist or isn't visible says so instead of showing a blank board.
 */
import { useEffect, useState } from 'react'
import { Link, useLocation, useNavigate, useParams } from 'react-router-dom'
import { SearchX } from 'lucide-react'
import { api } from '../api/client'
import { BandLoader } from '../components/brand/BandLoader'
import { loadProject } from '../lib/projectCache'
import { parseTicketRef, ticketLanding } from '../lib/ticketRef'

function NotVisible({ what }: { what: string }) {
  return (
    <div className="px-7 py-10 max-w-[640px]">
      <span className="amp-label">Not found</span>
      <h1 className="amp-heading m-0 mt-2 text-[22px] text-ink flex items-center gap-2.5">
        <SearchX size={18} className="text-ink-3" aria-hidden="true" />
        {what} was not found, or you can't see it
      </h1>
      <p className="text-[13px] text-ink-2 mt-2">
        It may have been deleted, or it's in a project you don't have access to. Ask its project's owner to add you,
        or <Link to="/projects" className="underline">open a project you can see</Link>.
      </p>
    </div>
  )
}

export function TicketRedirect() {
  const { ref = '' } = useParams()
  const { search } = useLocation()
  const navigate = useNavigate()
  const parsed = parseTicketRef(ref)
  const [missing, setMissing] = useState(false)

  useEffect(() => {
    if (!parsed) return
    let live = true
    api.getTask(parsed.id)
      .then(async t => {
        // A code that isn't the ticket's project's still opens the ticket (ids
        // are global, and codes get renamed), but the drawer says so: ?ref=
        // carries the ref as typed.
        let extra: Record<string, string> = {}
        if (parsed.code) {
          const project = await loadProject(t.project_id)
          if (project && project.code.toUpperCase() !== parsed.code) extra = { ref: `${parsed.code}-${parsed.id}` }
        }
        if (live) navigate(ticketLanding(t.project_id, t.id, search, extra), { replace: true })
      })
      .catch(() => { if (live) setMissing(true) })
    return () => { live = false }
  }, [parsed?.id, parsed?.code, search, navigate]) // eslint-disable-line react-hooks/exhaustive-deps

  if (!parsed || missing) return <NotVisible what={`Ticket ${parsed ? ref.toUpperCase() : `"${ref}"`}`} />
  return <div className="flex items-center justify-center h-full"><BandLoader label="Opening ticket" /></div>
}

export function RunRedirect() {
  const { runId = '' } = useParams()
  const { search } = useLocation()
  const navigate = useNavigate()
  const id = Number(runId)
  const valid = Number.isSafeInteger(id) && id > 0
  const [missing, setMissing] = useState(false)

  useEffect(() => {
    if (!valid) return
    let live = true
    api.getRun(id)
      .then(v => { if (live) navigate(ticketLanding(v.project_id, v.task_id, search, { run: String(id) }), { replace: true }) })
      .catch(() => { if (live) setMissing(true) })
    return () => { live = false }
  }, [id, valid, search, navigate])

  if (!valid || missing) return <NotVisible what={`Run ${runId}`} />
  return <div className="flex items-center justify-center h-full"><BandLoader label="Opening run" /></div>
}
