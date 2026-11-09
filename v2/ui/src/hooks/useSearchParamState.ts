import { useCallback, useEffect, useMemo, useRef } from 'react'
import { useSearchParams } from 'react-router-dom'

/**
 * Page state kept in the query string, so a view can be shared, reloaded and
 * walked back through (convention: v2/docs/url-state.md).
 *
 * - A value equal to its default is left out of the URL.
 * - Every other query param is kept as it is.
 * - Writes replace the current history entry unless asked to push: typing and
 *   toggles replace, selection changes push (pass `{ push: true }`, or set
 *   `push` on the codec to make it the default for that key).
 */
export type ParamUpdates = Record<string, string | null | undefined>
export interface WriteOptions { push?: boolean }

/** `prev` with `updates` applied: a null, undefined or empty value removes the key. */
export function patchParams(prev: URLSearchParams, updates: ParamUpdates): URLSearchParams {
  const next = new URLSearchParams(prev)
  for (const [k, v] of Object.entries(updates)) {
    if (v == null || v === '') next.delete(k)
    else next.set(k, v)
  }
  return next
}

/**
 * Write several params in one navigation (one history entry at most). Does
 * nothing when the result equals the current URL, so it never pushes a
 * duplicate entry.
 */
export function usePatchSearchParams(): (updates: ParamUpdates, opts?: WriteOptions) => void {
  const [params, setParams] = useSearchParams()
  // The params as last written, so two writes before a re-render (or a write
  // from a timer set on an older render) build on each other instead of the
  // second undoing the first.
  const latest = useRef(params)
  useEffect(() => { latest.current = params }, [params])
  return useCallback((updates, opts) => {
    const prev = latest.current
    const next = patchParams(prev, updates)
    if (next.toString() === prev.toString()) return
    latest.current = next
    setParams(next, { replace: !opts?.push })
  }, [setParams])
}

export interface ParamCodec<T> {
  default: T
  /** Read the raw value (null when absent); return the default for anything unusable. */
  parse: (raw: string) => T
  serialize: (value: T) => string
  /** Push a history entry on every write unless the call says otherwise. */
  push?: boolean
}

/** One value kept in `?<key>=`. */
export function useSearchParamState<T>(key: string, codec: ParamCodec<T>): [T, (value: T, opts?: WriteOptions) => void] {
  const [params] = useSearchParams()
  const patch = usePatchSearchParams()
  const raw = params.get(key)
  const { default: def, parse, serialize, push } = codec
  // Codecs are usually literals; key the parse on the raw string, not the codec.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  const value = useMemo(() => (raw == null ? def : parse(raw)), [raw])
  const set = useCallback((v: T, opts?: WriteOptions) => {
    const s = serialize(v)
    patch({ [key]: s === serialize(def) ? null : s }, { push: opts?.push ?? push })
  }, [key, serialize, def, push, patch])
  return [value, set]
}

// ─── Common codecs ────────────────────────────────────────────────────────────

/** A positive integer id, or null. */
export const idParam: ParamCodec<number | null> = {
  default: null,
  parse: raw => { const n = Number(raw); return Number.isInteger(n) && n > 0 ? n : null },
  serialize: v => (v == null ? '' : String(v)),
}

/** A free-text value; empty is the default. */
export const textParam: ParamCodec<string> = { default: '', parse: raw => raw, serialize: v => v }

/** A flag written as `<key>=<on>` when it differs from its default. */
export function flagParam(def: boolean, on = '1', off = '0'): ParamCodec<boolean> {
  return {
    default: def,
    parse: raw => (raw === on ? true : raw === off ? false : def),
    serialize: v => (v ? on : off),
  }
}

/** One of a fixed set of values; anything else reads as the default. */
export function enumParam<T extends string>(values: readonly T[], def: T, push = false): ParamCodec<T> {
  return { default: def, parse: raw => (values.includes(raw as T) ? raw as T : def), serialize: v => v, push }
}

/** A calendar day, `YYYY-MM-DD`; anything else reads as the default. */
export function dayParam(def: string): ParamCodec<string> {
  return { default: def, parse: raw => (/^\d{4}-\d{2}-\d{2}$/.test(raw) && !Number.isNaN(Date.parse(raw)) ? raw : def), serialize: v => v }
}

/** A comma-separated set of positive integer ids, e.g. `open=12,34`. */
export const idListParam: ParamCodec<number[]> = {
  default: [],
  parse: raw => [...new Set(raw.split(',').map(Number).filter(n => Number.isInteger(n) && n > 0))],
  serialize: v => v.join(','),
}
