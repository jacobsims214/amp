import { BrowserRouter, Routes, Route } from 'react-router-dom'
import { Projects } from './pages/Projects'
import { Board } from './pages/Board'
import { DAGView } from './pages/DAG'
import { Report } from './pages/Report'
import { KB } from './pages/KB'
import { ProjectSettings } from './pages/ProjectSettings'
import { DayReports } from './pages/DayReports'
import { Users } from './pages/Users'
import { Orgs } from './pages/Orgs'
import { AuthProvider } from './contexts/AuthContext'

export default function App() {
  return (
    <AuthProvider>
      <BrowserRouter>
        <Routes>
          <Route path="/" element={<Projects />} />
          <Route path="/project/:projectId" element={<Board />} />
          <Route path="/project/:projectId/dag" element={<DAGView />} />
          <Route path="/project/:projectId/report" element={<Report />} />
          <Route path="/project/:projectId/kb" element={<KB />} />
          <Route path="/project/:projectId/settings" element={<ProjectSettings />} />
          <Route path="/teams/:teamId/kb" element={<KB />} />
          <Route path="/orgs/:orgId/kb" element={<KB />} />
          <Route path="/reports" element={<DayReports />} />
          <Route path="/admin/users" element={<Users />} />
          <Route path="/admin/orgs" element={<Orgs />} />
        </Routes>
      </BrowserRouter>
    </AuthProvider>
  )
}
