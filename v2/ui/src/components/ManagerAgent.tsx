import { Link } from 'react-router-dom'
import { CONNECT_AGENT_DOC } from '../lib/guide'

/**
 * "manager agent", the one name the UI uses for the agent that plans your
 * work (amp-manager in the AMP Harness and the plugins), linked to the guide
 * page that says what it is and how to set it up. Every empty state that
 * mentions it uses this (#128).
 */
export function ManagerAgent({ children = 'manager agent' }: { children?: React.ReactNode }) {
  return (
    <Link to={CONNECT_AGENT_DOC} className="underline underline-offset-2 hover:text-ink"
      title="The agent that plans your work (amp-manager). How to set it up">
      {children}
    </Link>
  )
}
