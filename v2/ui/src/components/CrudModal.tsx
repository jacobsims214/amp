import { useEffect, useId, useRef, useState } from 'react'
import { Loader2, X } from 'lucide-react'
import { composing, useDialogFocus } from '../hooks/useDialogFocus'

interface CrudModalProps {
  title: string
  onClose: () => void
  /** omit for a read-only / immediate-action modal: the footer then shows only "Close" */
  onSave?: () => Promise<void> | void
  saving?: boolean
  saveLabel?: string
  /**
   * Whether closing would lose something. Default: anything was typed or
   * picked in the dialog since it opened.
   */
  dirty?: boolean
  children: React.ReactNode
}

/** Input types where Enter means "save the dialog" (#128). */
const SUBMIT_ON_ENTER = new Set(['text', 'search', 'email', 'url', 'tel', 'number', 'password', ''])

function submitsOnEnter(e: React.KeyboardEvent): boolean {
  const el = e.target as HTMLElement
  return e.key === 'Enter' && !e.shiftKey && !e.altKey && !e.ctrlKey && !e.metaKey && !composing(e.nativeEvent)
    && !e.defaultPrevented && el instanceof HTMLInputElement && SUBMIT_ON_ENTER.has(el.type)
    && !el.hasAttribute('list') // Enter may be picking a suggestion from its datalist
}

/** Change events from controls that save as they change (an owners picker): nothing to discard. */
const appliesNow = (target: EventTarget) => target instanceof Element && target.closest('[data-applies-now]') != null

/**
 * A dialog with a form. Enter in a one-line field saves; Escape or a click
 * outside closes it, and when something was typed asks first, in the
 * dialog's own footer (never a browser confirm). Tab stays inside, and focus
 * returns to the button that opened it when it closes.
 */
export function CrudModal({ title, onClose, onSave, saving = false, saveLabel = 'Save', dirty, children }: CrudModalProps) {
  const titleId = useId()
  const [typed, setTyped] = useState(false)
  const [confirming, setConfirming] = useState(false)
  const keepRef = useRef<HTMLButtonElement>(null)
  const panelRef = useRef<HTMLDivElement>(null)
  useDialogFocus(panelRef)
  // A dialog without Save applies its changes as they're made: nothing to lose.
  const changed = dirty ?? (onSave ? typed : false)

  // Escape and a click outside: close, or ask first when there's something to lose.
  const requestClose = () => {
    if (confirming) setConfirming(false)
    else if (changed) setConfirming(true)
    else onClose()
  }
  const requestRef = useRef(requestClose)
  useEffect(() => { requestRef.current = requestClose })
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape' || e.defaultPrevented || composing(e)) return
      e.preventDefault()
      requestRef.current()
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [])
  useEffect(() => { if (confirming) keepRef.current?.focus() }, [confirming])

  const save = () => { if (onSave && !saving) { setConfirming(false); void onSave() } }

  return (
    <>
      {/* Backdrop */}
      <div
        className="fixed inset-0 z-40"
        style={{ background: 'var(--scrim)' }}
        onClick={requestClose}
      />
      {/* Panel */}
      <div className="fixed inset-0 z-50 flex items-center justify-center p-4 pointer-events-none">
        <div
          ref={panelRef} role="dialog" aria-modal="true" aria-labelledby={titleId}
          className="w-full max-w-lg pointer-events-auto flex flex-col rounded-2xl shadow-2xl"
          style={{
            background: 'var(--surface)',
            border: '1px solid var(--line)',
            boxShadow: '0 12px 32px color-mix(in srgb, var(--ink) 14%, transparent), 0 0 0 1px color-mix(in srgb, var(--link) 8%, transparent)',
            maxHeight: '90vh',
          }}
          onKeyDown={e => { if (onSave && submitsOnEnter(e)) { e.preventDefault(); save() } }}
        >
          {/* Header */}
          <div className="flex items-center justify-between px-6 py-4 border-b border-line">
            <h2 id={titleId} className="text-sm font-semibold text-ink tracking-tight">{title}</h2>
            <button
              type="button"
              onClick={onClose}
              aria-label="Close"
              title="Close"
              className="p-1.5 rounded-lg hover:bg-surface-3 transition-colors cursor-pointer"
            >
              <X size={14} className="text-ink-2" />
            </button>
          </div>
          {/* Body */}
          <div className="flex-1 overflow-y-auto px-6 py-5 space-y-4" onChangeCapture={e => { if (!appliesNow(e.target)) setTyped(true) }}>
            {children}
          </div>
          {/* Footer */}
          {confirming ? (
            <div role="alertdialog" aria-label="Discard changes?" className="flex flex-wrap items-center justify-end gap-2 px-6 py-4 border-t border-line">
              <span className="mr-auto text-[13px] text-ink">Discard what you've entered?</span>
              <button ref={keepRef} type="button" onClick={() => setConfirming(false)} className="amp-btn">Keep editing</button>
              <button type="button" onClick={onClose} className="amp-btn amp-btn-danger">Discard</button>
            </div>
          ) : (
            <div className="flex items-center justify-end gap-2 px-6 py-4 border-t border-line">
              <span className="mr-auto text-[11px] text-ink-3 hidden sm:inline">{onSave ? 'Enter saves · Esc closes' : 'Esc closes'}</span>
              <button
                type="button"
                onClick={onClose}
                className="px-4 py-2 text-xs font-medium text-ink-2 hover:text-ink hover:bg-surface-3 rounded-lg transition-colors cursor-pointer"
              >
                {onSave ? 'Cancel' : 'Close'}
              </button>
              {onSave && (
                <button
                  type="button"
                  onClick={save}
                  disabled={saving}
                  className="amp-btn amp-btn-primary"
                >
                  {saving && <Loader2 size={11} className="animate-spin" />}
                  {saveLabel}
                </button>
              )}
            </div>
          )}
        </div>
      </div>
    </>
  )
}

/* ─── Shared form field styles ────────────────────────────────────────────── */
export const inputCls = 'amp-input'

export const labelCls = 'amp-label block mb-2'

/** A field's inline error, tied to its input by id (aria-describedby). */
export function FieldError({ id, children }: { id: string; children?: React.ReactNode }) {
  if (!children) return null
  return <p id={id} className="text-xs text-danger-ink mt-1.5">{children}</p>
}
