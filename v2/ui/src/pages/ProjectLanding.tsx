/**
 * `/project/:id` with no tab: open My work when something waits on the viewer
 * in this project, otherwise Mission (lib/landing.ts). The URL is replaced
 * with the chosen tab, so Back skips this hop and the tab shows as current.
 */
import { useEffect } from 'react'
import { useLocation, useNavigate, useParams } from 'react-router-dom'
import { BandLoader } from '../components/brand/BandLoader'
import { fixedTab, landingPath, landingTab } from '../lib/landing'
import { projectCounts, refreshWaitCounts } from '../lib/waitCounts'

export function ProjectLanding() {
  const { projectId = '' } = useParams()
  const { search } = useLocation()
  const navigate = useNavigate()

  useEffect(() => {
    let live = true
    // Old ticket links and ?approve= don't depend on the counts.
    const fixed = fixedTab(search)
    if (fixed) {
      navigate(landingPath(projectId, search, fixed), { replace: true })
      return
    }
    refreshWaitCounts().then(c => {
      if (live) navigate(landingPath(projectId, search, landingTab(search, projectCounts(c, projectId)?.waiting)), { replace: true })
    })
    return () => { live = false }
  }, [projectId, search, navigate])

  return <div className="flex items-center justify-center h-full"><BandLoader label="Opening project" /></div>
}
