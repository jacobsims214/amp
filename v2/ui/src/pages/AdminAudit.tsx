/**
 * Audit log (global admins): every entry of the whole AMP, with the chain
 * check of every chain, the filters and the export; and the instance
 * webhooks that stream every audit entry to a SIEM, including the
 * account-level ones (sign-ins, accounts, tokens) no org owns.
 */
import { useState } from 'react'
import { Link } from 'react-router-dom'
import { AuditLogPanel } from '../components/AuditLogPanel'
import { StoppedStreamWarning, WebhooksPanel } from '../components/WebhooksPanel'
import type { Webhook } from '../types'
import { PageHeader, Panel } from '../components/ui/PageHeader'
import { useAuth } from '../hooks/useAuth'
import { BandLoader } from '../components/brand/BandLoader'

export function AdminAudit() {
  const { loading, isAdmin } = useAuth()
  // The instance webhooks, as the SIEM panel loads them: a stopped stream
  // is shown at the top of the page, not only in that panel.
  const [siemHooks, setSiemHooks] = useState<Webhook[]>([])
  if (loading) return <BandLoader />
  if (!isAdmin) {
    return (
      <div className="flex flex-col min-h-full">
        <PageHeader crumb="Admin" title="Audit log" />
        <p className="m-7 amp-panel px-4 py-4 text-[13.5px] text-ink-2">
          The whole audit log is for global admins. Org owners and admins read their org's in <Link to="/admin/orgs" className="underline">Teams & orgs</Link>, under Audit log.
        </p>
      </div>
    )
  }
  return (
    <div className="flex flex-col min-h-full">
      <PageHeader crumb="Admin" title="Audit log"
        meta="Who changed access, settings and integrations, from where, and what was refused. Every entry is hash-chained." />
      <div className="p-4 sm:p-7 space-y-6 max-w-[1200px] w-full">
        <StoppedStreamWarning hooks={siemHooks} siem />
        <Panel label="Every entry" top>
          <div className="p-4"><AuditLogPanel verify /></div>
        </Panel>
        <Panel label="Stream to a SIEM">
          <div className="p-4 space-y-3">
            <p className="m-0 text-xs text-ink-2 leading-relaxed max-w-[75ch]">
              Instance webhooks receive every audit entry as it is written, including sign-ins, accounts and API tokens,
              which belong to no org, with their request context (IP, user agent, session) unless you turn it off. Delivery is at
              least once: a crash can repeat an entry, so de-duplicate on the envelope id. Deliveries are signed with the webhook's
              secret and retried for hours, and an instance webhook is never turned off; dropped deliveries are recorded in the
              audit log. Use the auth header for collectors that want a token, like Splunk HEC or Datadog.
            </p>
            <WebhooksPanel instance canManage onHooks={setSiemHooks} />
          </div>
        </Panel>
      </div>
    </div>
  )
}
