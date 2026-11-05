import { useCallback, useEffect, useRef, useState } from 'react'
import { AlertTriangle, KeyRound, Loader2, RefreshCw, Trash2 } from 'lucide-react'
import { api } from '../api/client'
import type { GitHubCredentialInfo } from '../types'
import { Pill, When } from './admin'
import { errMsg, primaryBtnCls, quietBtnCls } from '../lib/adminStyles'
import { inputCls, labelCls } from './CrudModal'
import { ConfirmButton } from './ui/ConfirmButton'
import { credentialProblem, httpStatus } from '../lib/githubCredential'

// The project's GitHub API token (AMP #3154 storage, #3155 API, this form
// #3161): the credential the repository-facts reader authenticates with, and
// therefore what lets a merge alert say whether a pull request targets the
// repository's default branch a rule may be holding.
//
// The token is WRITE-ONLY, and this file keeps it that way rather than
// relying on the server to. Three rules, each with a test:
//
//  1. NOTHING PREFILLS IT. The field starts empty, is never seeded from the
//     status answer (which has no token field to seed from), and is cleared
//     the moment a save returns — the value exists in this component's state
//     only between the paste and that answer.
//  2. IT TRAVELS IN A BODY, NEVER A URL. api.attach/rotateGitHubCredential
//     PUT/POST it in the request body; the only thing in the query string is
//     the host. A query string is written to every proxy and access log.
//  3. IT IS NEVER PERSISTED OR REPORTED. No localStorage, sessionStorage,
//     cookie or telemetry anywhere on this path: nothing here writes storage
//     at all, and the tests assert both an empty storage and that no request
//     carried the token outside its body.
//
// Owner-only: the settings page renders this panel only for a project owner,
// and the API refuses anyone else (403) — which is reported as the permission
// error it is, with the pasted value kept so a retry needs no re-paste.

export function GitHubCredentialPanel({ projectId }: { projectId: number }) {
  const [info, setInfo] = useState<GitHubCredentialInfo | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  // The token: pasted, saved, gone. Nothing ever reads it back (rule 1 above).
  const [token, setToken] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const field = useRef<HTMLInputElement>(null)

  const load = useCallback(() => {
    api.getGitHubCredential(projectId)
      .then(i => { setInfo(i); setLoadError(null) })
      .catch(e => setLoadError(errMsg(e, 'Could not load the GitHub token')))
  }, [projectId])
  useEffect(load, [load])

  const save = async () => {
    const value = token.trim()
    if (!value) {
      setError('Paste the token first.')
      setNotice(null)
      field.current?.focus()
      return
    }
    // Attaching and replacing are different calls, not one call with a flag:
    // the API refuses a rotation of nothing (404), so a page whose status went
    // stale can't quietly re-attach a token someone else just removed.
    const rotate = !!info?.configured
    // The label is metadata, not a secret: a replacement carries the existing
    // one through rather than silently dropping a label set over the API.
    const body = { token: value, label: info?.credential?.label ?? '', host: info?.host ?? '' }
    setBusy(true); setError(null); setNotice(null)
    try {
      const next = rotate
        ? await api.rotateGitHubCredential(projectId, body)
        : await api.attachGitHubCredential(projectId, body)
      setInfo(next)
      setToken('')  // stored: the page keeps no copy
      setNotice(rotate
        ? 'Token replaced. AMP stores it encrypted and can never show it again.'
        : 'Token saved. AMP stores it encrypted and can never show it again.')
    } catch (e) {
      const gone = httpStatus(e) === 404
      setError(httpStatus(e) === 403
        ? 'Only project owners can attach or replace this project’s GitHub token.'
        : gone && rotate
          ? 'There was no token left to replace, so nothing changed. Reload to attach a new one.'
          : errMsg(e, 'Could not save the token'))
      if (gone && rotate) load()
    } finally {
      setBusy(false)
    }
  }

  const remove = async () => {
    setBusy(true); setError(null); setNotice(null)
    try {
      const r = await api.deleteGitHubCredential(projectId)
      setInfo(prev => prev ? { ...prev, configured: false, credential: undefined, host: r?.host ?? prev.host } : prev)
      setToken('')
      setNotice('Token removed. AMP no longer reads this repository with it.')
    } catch (e) {
      setError(httpStatus(e) === 403
        ? 'Only project owners can remove this project’s GitHub token.'
        : errMsg(e, 'Could not remove the token'))
    } finally {
      setBusy(false)
    }
  }

  if (loadError && !info) return <p className="m-0 text-[12.5px] text-danger-ink" role="alert">{loadError}</p>
  if (!info) return <Loader2 size={16} className="text-ink-3 animate-spin" aria-label="Loading" />

  const credential = info.credential
  const problem = credentialProblem(info)
  // Without an encryption key the server refuses to store a token at all, so
  // the field and its button are disabled with the reason beside them rather
  // than failing on save.
  const canStore = info.encrypted

  return (
    <div className="space-y-4">
      {error && <p className="m-0 text-[12.5px] text-danger-ink" role="alert">{error}</p>}
      {notice && <p className="m-0 text-[12.5px] text-success-ink" role="status">{notice}</p>}
      {problem && (
        <p className="m-0 text-[12.5px] text-warning-ink flex items-start gap-1.5" role="alert">
          <AlertTriangle size={13} className="mt-0.5 flex-shrink-0" />
          <span>{problem}</span>
        </p>
      )}

      {info.configured ? (
        <>
          <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-[12.5px] text-ink-2">
            <Pill tone="green">On</Pill>
            <span>for <b className="text-ink font-medium">{credential?.host || info.host}</b></span>
            {credential?.label && <span>labelled <b className="text-ink font-medium">{credential.label}</b></span>}
            {credential && (
              <span>{credential.rotated_at ? 'Replaced' : 'Attached'} <When at={credential.rotated_at ?? credential.created_at} /></span>
            )}
            <span>{info.encrypted ? 'Stored encrypted' : 'Stored, but this server has no key to open it'}</span>
          </div>
          <p className="m-0 text-[12.5px] text-ink-2 leading-relaxed">
            AMP reads this repository's default branch and rules with this token. It can be replaced or
            removed here; it can never be shown again.
          </p>
        </>
      ) : (
        <p className="m-0 text-[12.5px] text-ink-2 leading-relaxed">
          No token attached. A public repository is read without one; attaching a token lets a private
          repository be read, and is what makes a merge alert able to say where a pull request is going.
        </p>
      )}

      <div className="pt-1">
        <label className={labelCls} htmlFor="github-credential-token">Token</label>
        <input id="github-credential-token" ref={field} type="password" value={token}
          autoComplete="off" spellCheck={false} disabled={busy || !canStore}
          placeholder={info.configured ? 'Paste the new token' : 'ghp_… or a fine-grained token'}
          aria-describedby="github-credential-hint" className={`${inputCls} max-w-[52ch]`}
          onChange={e => setToken(e.target.value)} />
        <p id="github-credential-hint" className="m-0 mt-1 text-[11.5px] text-ink-3">
          {!canStore
            ? 'Until the server has an encryption key, there is nowhere safe to put a token: nothing is stored.'
            : info.configured
              ? 'The token in use now stops being used the moment the new one is saved. Needs read access to the repository.'
              : 'Needs read access to the repository. It is sent once, stored encrypted, and never shown again.'}
        </p>
      </div>

      <div className="flex flex-wrap items-center gap-2 pt-1">
        <button type="button" className={primaryBtnCls} disabled={busy || !canStore} onClick={save}>
          {busy ? <Loader2 size={12} className="animate-spin" /> : <KeyRound size={12} />}
          {info.configured ? 'Replace token' : 'Save token'}
        </button>
        {info.configured && (
          <ConfirmButton label="Remove token" confirmLabel="Remove it" danger busy={busy}
            icon={<Trash2 size={12} />} onConfirm={remove} />
        )}
        <button type="button" className={quietBtnCls} onClick={load} aria-label="Refresh the token status"><RefreshCw size={12} /></button>
      </div>
    </div>
  )
}
