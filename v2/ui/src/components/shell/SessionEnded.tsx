import { useEffect, useRef, useState, type KeyboardEvent, type ReactNode } from 'react'
import { LogIn } from 'lucide-react'
import { signInAgainUrl, useSessionEnded } from '../../lib/session'

/**
 * Once oauth2-proxy has ended the session (#153), one prompt to sign in again
 * sits on top of the app. The pages stay mounted underneath (inert while the
 * prompt is up), so a half-written comment or ticket edit is still there:
 * "Stay on this page" puts the prompt away as a banner so the text can be
 * copied before signing in. Nothing more reaches AMP meanwhile: the API
 * client and the relay transport refuse to send, and the event streams close
 * (lib/session.ts).
 */
export function SessionEndedGate({ children }: { children: ReactNode }) {
  const ended = useSessionEnded()
  const [stayed, setStayed] = useState(false)
  const modal = ended && !stayed
  return (
    <>
      {/* Always the same wrapper, so the pages never remount when the session ends. */}
      <div inert={modal}>{children}</div>
      {modal && <SessionEnded onStay={() => setStayed(true)} />}
      {ended && stayed && <SessionEndedBanner />}
    </>
  )
}

export function SessionEnded({ onStay }: { onStay?: () => void }) {
  const signIn = useRef<HTMLAnchorElement>(null)
  const box = useRef<HTMLElement>(null)
  useEffect(() => { signIn.current?.focus() }, [])
  // Keep Tab inside the prompt (the page behind it is inert).
  const trap = (e: KeyboardEvent) => {
    if (e.key !== 'Tab' || !box.current) return
    const items = [...box.current.querySelectorAll<HTMLElement>('a[href], button')]
    if (items.length === 0) return
    const first = items[0], last = items[items.length - 1]
    if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus() }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus() }
  }
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center p-6" style={{ background: 'color-mix(in srgb, var(--bg) 70%, transparent)' }}>
      <section ref={box} onKeyDown={trap} role="alertdialog" aria-modal="true" aria-labelledby="session-ended-title" aria-describedby="session-ended-body"
        className="amp-panel border-l-[3px] px-6 py-5 grid gap-3 max-w-[30rem] shadow-lg" style={{ borderLeftColor: 'var(--warning)' }}>
        <span className="amp-label" style={{ color: 'var(--warning-ink)' }}>Signed out</span>
        <h1 id="session-ended-title" className="m-0 text-[19px] font-semibold text-ink">Your session ended</h1>
        <p id="session-ended-body" className="m-0 text-[13.5px] text-ink-2 leading-normal">
          Your sign-in expired or was ended, so AMP can't load or save anything until you sign in again. You'll come back to this page.
          Anything you were writing is still on the page: stay to copy it first.
        </p>
        <div className="flex items-center gap-2">
          <a ref={signIn} href={signInAgainUrl()} className="amp-btn amp-btn-primary amp-btn-sm inline-flex items-center gap-1.5">
            <LogIn size={14} aria-hidden="true" />Sign in again
          </a>
          {onStay && <button type="button" className="amp-btn amp-btn-sm" onClick={onStay}>Stay on this page</button>}
        </div>
      </section>
    </div>
  )
}

/** The prompt put away: the page is usable to copy from, and sign-in is one click. */
function SessionEndedBanner() {
  return (
    <div role="region" aria-label="Session ended"
      className="fixed top-0 inset-x-0 z-50 flex items-center justify-center gap-3 px-4 py-2 text-[13px] text-ink border-b"
      style={{ background: 'var(--surface)', borderColor: 'var(--warning)' }}>
      <span>Your session ended. Nothing can be saved until you sign in again.</span>
      <a href={signInAgainUrl()} className="amp-btn amp-btn-primary amp-btn-sm inline-flex items-center gap-1.5">
        <LogIn size={14} aria-hidden="true" />Sign in again
      </a>
    </div>
  )
}
