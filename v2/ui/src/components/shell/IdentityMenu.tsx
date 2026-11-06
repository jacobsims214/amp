import { useEffect, useId, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { ChevronUp, KeyRound, LogIn, LogOut } from 'lucide-react'
import { motion } from 'framer-motion'
import type { Me } from '../../types'
import { Avatar } from '../ui/Avatar'

// The sidebar footer (#124): who you are, as a small menu. It opens on
// click with the identity's details and, with sign-in on, Sign in or Sign
// out; in local mode it says sign-in is off instead.

/** oauth2-proxy's sign-in, then back to this page (as AuthProvider does). */
function signInHref(): string {
  return `/oauth2/start?rd=${encodeURIComponent(window.location.pathname + window.location.search)}`
}

export function IdentityMenu({
  me,
  roleLabel,
  collapsed = false,
  shouldReduceMotion = false,
}: {
  me: Me
  roleLabel: string
  collapsed?: boolean
  shouldReduceMotion?: boolean
}) {
  const [open, setOpen] = useState(false)
  const root = useRef<HTMLDivElement>(null)
  const menuId = useId()

  useEffect(() => {
    if (!open) return
    const onDown = (e: MouseEvent) => { if (!root.current?.contains(e.target as Node)) setOpen(false) }
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') setOpen(false) }
    document.addEventListener('mousedown', onDown)
    document.addEventListener('keydown', onKey)
    return () => { document.removeEventListener('mousedown', onDown); document.removeEventListener('keydown', onKey) }
  }, [open])

  const local = !!me.local_dev
  const signedIn = me.user_id != null
  const itemCls = 'flex items-center gap-2 px-3 py-2 text-[13px] text-ink hover:bg-surface-3 no-underline'

  return (
    <div ref={root} className="relative mt-auto border-t border-line">
      {open && (
        <div id={menuId} role="group" aria-label="Your account"
          className="absolute bottom-full left-2 right-2 mb-1 z-50 bg-surface border border-line shadow-lg py-1">
          <div className="px-3 py-2 grid gap-0.5 border-b border-line">
            <span className="text-[13px] font-semibold text-ink truncate">{me.name || me.email}</span>
            <span className="amp-num text-[11px] text-ink-3 truncate">{me.email}</span>
            <span className="text-[11.5px] text-ink-2">{roleLabel}{me.kind === 'agent' ? ' · agent account' : ''}{me.via_token ? ' · signed in with an API token' : ''}</span>
          </div>
          {local && (
            <p className="m-0 px-3 py-2 text-[12px] text-ink-2 leading-normal border-b border-line">
              Local mode — sign-in is off. Every request is this Local Dev user, an AMP admin.
            </p>
          )}
          {signedIn && (
            <Link to="/access" className={itemCls} onClick={() => setOpen(false)}><KeyRound size={14} aria-hidden="true" />Agents & tokens</Link>
          )}
          {!local && signedIn && !me.via_token && (
            // oauth2-proxy clears the session and sends you to sign in again.
            <a href="/oauth2/sign_out?rd=%2F" className={itemCls}><LogOut size={14} aria-hidden="true" />Sign out</a>
          )}
          {!local && !signedIn && (
            <a href={signInHref()} className={itemCls}><LogIn size={14} aria-hidden="true" />Sign in</a>
          )}
        </div>
      )}
      <button type="button" onClick={() => setOpen(o => !o)} aria-expanded={open} aria-controls={open ? menuId : undefined}
        aria-label={`Account: ${me.name || me.email}`}
        className={`w-full flex items-center text-left bg-transparent border-0 cursor-pointer hover:bg-surface-3 ${collapsed ? 'justify-center px-2 py-3' : 'gap-2.5 px-4 py-3'}`}>
        <Avatar name={me.name || me.email} kind={me.kind} />
        {!collapsed && (
          <span className="min-w-0 flex-1">
            <span className="block text-[13px] text-ink truncate">{me.name || me.email}</span>
            <span className="block amp-num text-[10.5px] text-ink-3 truncate">{roleLabel} · {me.email}</span>
          </span>
        )}
        {!collapsed && (
          <motion.span
            animate={{ rotate: open ? 0 : 180 }}
            transition={{ duration: shouldReduceMotion ? 0 : 0.2, ease: [0.4, 0, 0.2, 1] }}
          >
            <ChevronUp size={14} className="shrink-0 text-ink-3" aria-hidden="true" />
          </motion.span>
        )}
      </button>
    </div>
  )
}
