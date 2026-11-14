import { useState } from 'react'
import { BrowserRouter, Routes, Route, Navigate, useParams } from 'react-router-dom'
import { Projects } from './pages/Projects'
import { MyWork } from './pages/MyWork'
import { Board } from './pages/Board'
import { Mission } from './pages/Mission'
import { ProjectMyWork } from './pages/ProjectMyWork'
import { ProjectLanding } from './pages/ProjectLanding'
import { FlightPlan } from './pages/FlightPlan'
import { DAGView } from './pages/DAG'
import { Report } from './pages/Report'
import { KB } from './pages/KB'
import { ProjectSettings } from './pages/ProjectSettings'
import { DayReports } from './pages/DayReports'
import { Users } from './pages/Users'
import { Orgs } from './pages/Orgs'
import { AdminAudit } from './pages/AdminAudit'
import { Access } from './pages/Access'
import { Trace } from './pages/Trace'
import { Portfolio } from './pages/Portfolio'
import { Artifacts } from './pages/Artifacts'
import { ArtifactView } from './pages/ArtifactView'
import { Session } from './pages/Session'
import { Sessions } from './pages/Sessions'
import { TicketPage } from './pages/TicketPage'
import { RunRedirect, TicketRedirect } from './pages/TicketRedirect'
import { TeamInboxRoute, TeamRequestRoute, RequesterNewRequestRoute, RequesterRequestsRoute, RequesterStatusRoute } from './pages/RequestRoutes'
import { RemoteProvider } from './remote/RemoteProvider'
import { AuthProvider } from './contexts/AuthContext'
import { AppShell } from './components/shell/AppShell'
import { SessionEndedGate } from './components/shell/SessionEnded'
import { LaunchSequence } from './components/brand/LaunchSequence'
import { shouldLaunch } from './lib/launch'

/** An unknown view under a project opens the project (My work or Mission, by the landing rule). */
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
  // Decided once, from the URL the app was opened at: only a cold start at "/"
  // plays the launch, once per session. Deep links (including the remote
  // session page, where on a phone you want the chat now) go straight in.
  const [launch] = useState(() => !window.location.pathname.startsWith('/sessions/') && shouldLaunch())
  return (
    <AuthProvider>
      {/* Once oauth2-proxy ends the session, one sign-in prompt replaces the app (#153). */}
      <SessionEndedGate>
      <RemoteProvider>
      {launch && <LaunchSequence />}
      <BrowserRouter>
        <Routes>
          {/* Standalone (no sidebar) so it works on a phone; served with a strict CSP. */}
          <Route path="/sessions/:sessionId" element={<Session />} />
          {/* The requester surfaces, deliberately OUTSIDE the app shell (AMP #3988).
              A requester is a customer — possibly in no org at all — and the shell is
              the team's frame: project tree, org admin, trace, agents, every internal
              name. Mounting these in their own tree is what makes "a requester route
              reveals no internal nav" structural rather than a matter of hiding
              things. Their frame is components/requests/RequesterChrome. */}
          <Route path="/requests" element={<RequesterRequestsRoute />} />
          <Route path="/requests/new" element={<RequesterNewRequestRoute />} />
          <Route path="/request/:publicRef" element={<RequesterStatusRoute />} />
          <Route element={<AppShell />}>
            <Route path="/" element={<MyWork />} />
            <Route path="/sessions" element={<Sessions />} />
            <Route path="/projects" element={<Projects />} />
            {/* Canonical links: a ticket by its ref (BA-34 or 34), a run by its id. */}
            <Route path="/t/:ref" element={<TicketRedirect />} />
            <Route path="/r/:runId" element={<RunRedirect />} />
            {/* No tab: My work when something waits on you here, else Mission (lib/landing.ts). */}
            <Route path="/project/:projectId" element={<ProjectLanding />} />
            <Route path="/project/:projectId/mission" element={<Mission />} />
            <Route path="/project/:projectId/my-work" element={<ProjectMyWork />} />
            <Route path="/project/:projectId/board" element={<Board />} />
            <Route path="/project/:projectId/plan" element={<FlightPlan />} />
            <Route path="/project/:projectId/dag" element={<DAGView />} />
            <Route path="/project/:projectId/ticket/:ticketId" element={<TicketPage />} />
            <Route path="/project/:projectId/report" element={<Report />} />
            <Route path="/project/:projectId/kb" element={<KB />} />
            <Route path="/project/:projectId/artifacts" element={<Artifacts />} />
            <Route path="/artifacts/:artifactId" element={<ArtifactView />} />
            <Route path="/project/:projectId/settings" element={<ProjectSettings />} />
            <Route path="/teams/:teamId/kb" element={<KB />} />
            {/* The team's workstream inbox (AMP #3988): the shared queue, then one
                request in it. Inside the shell on purpose — this is the team's own
                surface, and its rows are the team's work. */}
            <Route path="/teams/:teamId/requests" element={<TeamInboxRoute />} />
            <Route path="/teams/:teamId/requests/:ref" element={<TeamRequestRoute />} />
            <Route path="/orgs/:orgId/kb" element={<KB />} />
            <Route path="/reports" element={<DayReports />} />
            <Route path="/admin/users" element={<Users />} />
            <Route path="/admin/orgs" element={<Orgs />} />
            <Route path="/admin/audit" element={<AdminAudit />} />
            <Route path="/access" element={<Access />} />
            <Route path="/guide" element={<KB />} />
            <Route path="/trace" element={<Trace />} />
            <Route path="/portfolio" element={<Portfolio />} />
            <Route path="/project/:projectId/*" element={<ProjectFallback />} />
            <Route path="*" element={<NotFound />} />
          </Route>
        </Routes>
      </BrowserRouter>
      </RemoteProvider>
      </SessionEndedGate>
    </AuthProvider>
  )
}
