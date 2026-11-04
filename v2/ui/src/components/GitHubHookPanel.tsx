import { useCallback, useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { AlertTriangle, KeyRound, Loader2, Power, RefreshCw } from 'lucide-react'
import { api } from '../api/client'
import type { GitHubHookDelivery, GitHubHookInfo } from '../types'
import { CopyButton, Pill, SecretReveal, When } from './admin'
import { errMsg, primaryBtnCls, quietBtnCls } from '../lib/adminStyles'
import { ConfirmButton } from './ui/ConfirmButton'
import { hookURL } from '../lib/githubHook'

// The project's inbound GitHub merge webhook (AMP #2479, settings UI #2485):
// GitHub posts pull_request events, signed with this project's secret, and a
// merged PR records the merge on the tickets that produced it. Project owners
// only; the secret is shown once, when it's generated or rotated.

function deliveryTone(d: GitHubHookDelivery): 'green' | 'red' | 'amber' | 'gray' {
  if (d.status >= 500) return 'red'
  if (d.status >= 400) return 'amber'
  return (d.task_ids?.length ?? 0) > 0 ? 'green' : 'gray'
}

function SetupSteps({ url, info }: { url: string; info: GitHubHookInfo }) {
  return (
    <ol className="m-0 pl-5 space-y-1.5 text-[12.5px] text-ink-2 leading-relaxed">
      <li>In the GitHub repository, open <b className="text-ink font-medium">Settings → Webhooks → Add webhook</b>.</li>
      <li className="min-w-0">
        <b className="text-ink font-medium">Payload URL</b>:{' '}
        <span className="inline-flex items-center gap-1.5 flex-wrap max-w-full">
          <code className="amp-num text-ink break-all">{url}</code>
          <CopyButton text={url} label="Copy URL" />
        </span>
      </li>
      <li><b className="text-ink font-medium">Content type</b>: <code className="amp-num text-ink">{info.setup?.content_type || 'application/json'}</code></li>
      <li><b className="text-ink font-medium">Secret</b>: the secret generated here.</li>
      <li><b className="text-ink font-medium">Which events</b>: choose "Let me select individual events" and tick only <b className="text-ink font-medium">{info.setup?.events || 'Pull requests'}</b>.</li>
    </ol>
  )
}

function Deliveries({ projectId, deliveries }: { projectId: number; deliveries: GitHubHookDelivery[] }) {
  const [all, setAll] = useState(false)
  if (deliveries.length === 0) {
    return <p className="m-0 text-[12.5px] text-ink-3">No deliveries yet. GitHub sends a ping when the webhook is added.</p>
  }
  const shown = all ? deliveries : deliveries.slice(0, 5)
  return (
    <div>
      <ul className="m-0 p-0 list-none divide-y divide-[var(--line)] border border-line">
        {shown.map(d => (
          <li key={d.delivery_id} className="px-3 py-2 flex flex-wrap items-center gap-x-3 gap-y-1 text-[12.5px]">
            <Pill tone={deliveryTone(d)} title={`HTTP ${d.status}`}>{d.status}</Pill>
            <span className="text-ink font-medium">{d.event}{d.action ? ` · ${d.action}` : ''}</span>
            <span className="flex-1 min-w-[12ch] text-ink-2 break-words">
              {d.result}
              {(d.task_ids?.length ?? 0) > 0 && (
                <> {d.task_ids!.map((id, i) => (
                  <span key={id}>{i > 0 && ', '}<Link to={`/project/${projectId}/board?focus=${id}`} className="text-ink underline underline-offset-2">#{id}</Link></span>
                ))}</>
              )}
            </span>
            <span className="text-[11.5px] text-ink-3 whitespace-nowrap"><When at={d.received_at} /></span>
          </li>
        ))}
      </ul>
      {deliveries.length > 5 && (
        <button type="button" className={`${quietBtnCls} mt-1.5`} onClick={() => setAll(a => !a)}>
          {all ? 'Show the latest 5' : `Show all ${deliveries.length}`}
        </button>
      )}
    </div>
  )
}

export function GitHubHookPanel({ projectId }: { projectId: number }) {
  const [info, setInfo] = useState<GitHubHookInfo | null>(null)
  // Held only in this component's state, from the generate/rotate answer
  // until "I've copied it": it can't be read back from the server.
  const [secret, setSecret] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(() => {
    api.getGitHubHook(projectId).then(i => { setInfo(i); setError(null) })
      .catch(e => setError(errMsg(e, 'Could not load the GitHub webhook')))
  }, [projectId])
  useEffect(load, [load])

  const rotate = async () => {
    setBusy(true); setError(null)
    try {
      const i = await api.rotateGitHubHookSecret(projectId)
      setSecret(i.secret ?? null)
      setInfo({ ...i, secret: undefined })
    } catch (e) {
      setError(errMsg(e, 'Could not generate the secret'))
    } finally {
      setBusy(false)
    }
  }

  const turnOff = async () => {
    setBusy(true); setError(null)
    try {
      await api.deleteGitHubHook(projectId)
      setSecret(null)
      load()
    } catch (e) {
      setError(errMsg(e, 'Could not turn the webhook off'))
    } finally {
      setBusy(false)
    }
  }

  if (!info) {
    return error
      ? <p className="m-0 text-[12.5px] text-danger-ink">{error}</p>
      : <Loader2 size={16} className="text-ink-3 animate-spin" aria-label="Loading" />
  }

  const url = hookURL(info)
  const hook = info.hook
  const deliveries = info.deliveries ?? []

  return (
    <div className="space-y-4">
      {error && <p className="m-0 text-[12.5px] text-danger-ink" role="alert">{error}</p>}

      {secret && (
        <SecretReveal title="GitHub webhook secret" secret={secret} onDone={() => setSecret(null)}
          note="Copy it now and paste it into GitHub: it's shown only when it is generated or rotated.">
          <SetupSteps url={url} info={info} />
        </SecretReveal>
      )}

      {!info.configured ? (
        <div className="space-y-3">
          <p className="m-0 text-[12.5px] text-ink-2 leading-relaxed">
            Not set up. Generate a secret, then add the webhook to the GitHub repository with it.
          </p>
          <button type="button" className={primaryBtnCls} disabled={busy} onClick={rotate}>
            {busy ? <Loader2 size={12} className="animate-spin" /> : <KeyRound size={12} />}Generate secret
          </button>
        </div>
      ) : (
        <>
          <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-[12.5px] text-ink-2">
            <span className="inline-flex items-center gap-1.5"><Pill tone="green">On</Pill></span>
            {hook && <span>Secret set <When at={hook.rotated_at} /></span>}
            <span>{info.encrypted ? 'Stored encrypted' : 'Stored unencrypted (the server has no encryption key)'}</span>
          </div>
          {hook && hook.rejected_count > 0 && (
            <p className="m-0 text-[12.5px] text-warning-ink flex items-start gap-1.5">
              <AlertTriangle size={13} className="mt-0.5 flex-shrink-0" />
              <span>
                {hook.rejected_count} request{hook.rejected_count === 1 ? ' was' : 's were'} refused for a missing or wrong signature
                {hook.last_rejected_at && <>, the last <When at={hook.last_rejected_at} /></>}. If GitHub sent them, its secret doesn't match this one.
              </span>
            </p>
          )}

          {!secret && (
            <div>
              <h3 className="amp-label m-0 mb-2">Set up in GitHub</h3>
              <SetupSteps url={url} info={info} />
            </div>
          )}

          <div>
            <div className="flex items-center gap-2 mb-2">
              <h3 className="amp-label m-0">Recent deliveries</h3>
              <button type="button" className={quietBtnCls} onClick={load} aria-label="Refresh deliveries"><RefreshCw size={12} /></button>
            </div>
            <Deliveries projectId={projectId} deliveries={deliveries} />
          </div>

          <div className="flex flex-wrap items-center gap-2 pt-1">
            <ConfirmButton label="Rotate secret" confirmLabel="Rotate: GitHub needs the new one" busy={busy}
              icon={<KeyRound size={12} />} onConfirm={rotate} />
            <ConfirmButton label="Turn off" confirmLabel="Turn it off" danger busy={busy}
              icon={<Power size={12} />} onConfirm={turnOff} />
          </div>
        </>
      )}
    </div>
  )
}
