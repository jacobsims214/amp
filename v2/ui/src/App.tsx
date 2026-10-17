import { BrowserRouter, Routes, Route, Navigate, useParams } from 'react-router-dom'
import { Projects } from './pages/Projects'
import { MyWork } from './pages/MyWork'
import { Board } from './pages/Board'
import { Mission } from './pages/Mission'
import { FlightPlan } from './pages/FlightPlan'
import { DAGView } from './pages/DAG'
import { Report } from './pages/Report'
import { KB } from './pages/KB'
import { ProjectSettings } from './pages/ProjectSettings'
import { DayReports } from './pages/DayReports'
import { Users } from './pages/Users'
import { Orgs } from './pages/Orgs'
import { Access } from './pages/Access'
import { AuthProvider } from './contexts/AuthContext'
import { AppShell } from './components/shell/AppShell'

/** An unknown view under a project lands on its Mission page. */
function ProjectFallback() {
  const { projectId } = useParams()
  return <Navigate to={`/project/${projectId}`} replace />
}

function NotFound() {
  return (
    <div className="px-7 py-10">
      <span className="amp-label">404</span>
      <h1 className="amp-heading m-0 mt-2 text-[22px] text-ink">Nothing here</h1>
      <p className="text-[13px] text-ink-2 mt-2">That page doesn't exist. <a href="/" className="underline">Go to My work</a>.</p>
    </div>
  )
}

export default function App() {
  return (
    <AuthProvider>
      <BrowserRouter>
        <Routes>
          <Route element={<AppShell />}>
            <Route path="/" element={<MyWork />} />
            <Route path="/projects" element={<Projects />} />
            <Route path="/project/:projectId" element={<Mission />} />
            <Route path="/project/:projectId/board" element={<Board />} />
            <Route path="/project/:projectId/plan" element={<FlightPlan />} />
            <Route path="/project/:projectId/dag" element={<DAGView />} />
            <Route path="/project/:projectId/report" element={<Report />} />
            <Route path="/project/:projectId/kb" element={<KB />} />
            <Route path="/project/:projectId/settings" element={<ProjectSettings />} />
            <Route path="/teams/:teamId/kb" element={<KB />} />
            <Route path="/orgs/:orgId/kb" element={<KB />} />
            <Route path="/reports" element={<DayReports />} />
            <Route path="/admin/users" element={<Users />} />
            <Route path="/admin/orgs" element={<Orgs />} />
            <Route path="/access" element={<Access />} />
            <Route path="/project/:projectId/*" element={<ProjectFallback />} />
            <Route path="*" element={<NotFound />} />
          </Route>
        </Routes>
      </BrowserRouter>
    </AuthProvider>
  )
}
