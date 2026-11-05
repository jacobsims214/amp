/**
 * `/project/:id` with no tab: open My work when something waits on the viewer
 * in this project, otherwise Mission (lib/landing.ts). The URL is replaced
 * with the chosen tab, so Back skips this hop and the tab shows as current.
 */
import { useEffect } from 'react'
import { Navigate, useLocation, useNavigate, useParams } from 'react-router-dom'
import { BandLoader } from '../components/brand/BandLoader'
import { fixedTab, landingPath, landingTab } from '../lib/landing'
import { projectCounts, refreshWaitCounts, useWaitCounts } from '../lib/waitCounts'

export function ProjectLanding() {
  const { projectId = '' } = useParams()
  const { search } = useLocation()
  const navigate = useNavigate()
  // Counts the sidebar already fetched: pick the tab on the first render, no loader.
  const cached = useWaitCounts()
  const warm = cached !== null

  useEffect(() => {
    let live = true
    // Old ticket links and ?approve= don't depend on the counts.
    const fixed = fixedTab(search)
    if (fixed) {
      navigate(landingPath(projectId, search, fixed), { replace: true })
      return
    }
    refreshWaitCounts().then(c => {
      if (live && !warm) navigate(landingPath(projectId, search, landingTab(search, projectCounts(c, projectId)?.waiting)), { replace: true })
    })
    return () => { live = false }
  }, [projectId, search, navigate, warm])

  if (cached) return <Navigate replace to={landingPath(projectId, search, landingTab(search, projectCounts(cached, projectId)?.waiting))} />
  return <div className="flex items-center justify-center h-full"><BandLoader label="Opening project" /></div>
}
