import { BrowserRouter, Routes, Route } from 'react-router-dom'
import { Projects } from './pages/Projects'
import { Board } from './pages/Board'
import { DAGView } from './pages/DAG'
import { Report } from './pages/Report'
import { KB } from './pages/KB'
import { Users } from './pages/Users'
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
          <Route path="/admin/users" element={<Users />} />
        </Routes>
      </BrowserRouter>
    </AuthProvider>
  )
}
