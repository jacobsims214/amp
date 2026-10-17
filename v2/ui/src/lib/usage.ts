import type { Usage } from '../types'

/** 1234 → "1.2k", 4_500_000 → "4.5M". */
export function tokens(n?: number | null): string {
  if (n == null) return '—'
  if (n < 1000) return String(n)
  if (n < 1_000_000) return `${(n / 1000).toFixed(n < 10_000 ? 1 : 0)}k`
  return `${(n / 1_000_000).toFixed(n < 10_000_000 ? 2 : 1)}M`
}

export function usd(n?: number | null): string {
  if (n == null) return '—'
  return n < 1 ? `$${n.toFixed(3)}` : `$${n.toFixed(2)}`
}

/** "41k in · 5.1k out" */
export function usageLine(u?: Usage | null): string {
  if (!u || !u.runs) return 'no runs reported'
  return `${tokens(u.input_tokens)} in · ${tokens(u.output_tokens)} out`
}
