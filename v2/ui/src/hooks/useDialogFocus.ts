import { useEffect, useRef, useState, type RefObject } from 'react'

const FOCUSABLE = 'a[href], button:not([disabled]), input:not([disabled]):not([type="hidden"]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'

/** Open dialogs, innermost last: only the top one keeps Tab inside it. */
const openDialogs: RefObject<HTMLElement | null>[] = []

/**
 * Focus for a modal dialog (#128):
 * - remembers what had focus when the dialog first rendered (the button that
 *   opened it; read during render, before an autoFocus field takes focus)
 *   and gives focus back to it when the dialog closes, or to the page's
 *   main content when that button is gone (an empty state's button);
 * - moves focus into the dialog once, when it opens, if nothing in it has
 *   focus yet (to `initial`, else its first focusable element);
 * - keeps Tab and Shift+Tab inside the dialog on top.
 *
 * Under StrictMode, React runs effects twice in development, with a pretend
 * unmount between. The panel is still in the page then, so nothing is
 * restored, and the second run doesn't move focus again: an autoFocus field
 * keeps it.
 */
export function useDialogFocus(panel: RefObject<HTMLElement | null>, initial?: RefObject<HTMLElement | null>) {
  const [opener] = useState(() => (typeof document !== 'undefined' ? document.activeElement : null))
  const focused = useRef(false)

  useEffect(() => {
    const el = panel.current
    if (el && !focused.current) {
      focused.current = true
      if (!el.contains(document.activeElement)) (initial?.current ?? el.querySelector<HTMLElement>(FOCUSABLE))?.focus()
    }
    openDialogs.push(panel)
    const onKey = (e: KeyboardEvent) => {
      const box = panel.current
      if (e.key !== 'Tab' || !box || openDialogs[openDialogs.length - 1] !== panel) return
      const items = [...box.querySelectorAll<HTMLElement>(FOCUSABLE)]
      if (items.length === 0) { e.preventDefault(); return }
      const first = items[0], last = items[items.length - 1]
      const active = document.activeElement
      if (!box.contains(active)) { e.preventDefault(); first.focus() }
      else if (e.shiftKey && active === first) { e.preventDefault(); last.focus() }
      else if (!e.shiftKey && active === last) { e.preventDefault(); first.focus() }
    }
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('keydown', onKey)
      const i = openDialogs.lastIndexOf(panel)
      if (i >= 0) openDialogs.splice(i, 1)
      // A real close has taken the panel out of the page by now; StrictMode's pretend unmount hasn't.
      if (el?.isConnected) return
      const back = opener as HTMLElement | null
      if (back?.isConnected && typeof back.focus === 'function') back.focus()
      else if (!document.activeElement || document.activeElement === document.body) document.getElementById('main-content')?.focus()
    }
  }, [panel, initial, opener])
}

/** A key that belongs to an IME composition (Safari sends the confirming Enter with keyCode 229). */
export const composing = (e: KeyboardEvent) => e.isComposing || e.keyCode === 229
