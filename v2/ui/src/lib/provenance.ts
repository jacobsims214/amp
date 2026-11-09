/**
 * Provenance for a decision doc (docs/specs/human-decision-docs.md §3): who
 * wrote each line of the shown version, and each option. A version's author
 * is fixed by its write path (the drafted v1 is the agent's, an edit in AMP
 * is the person's), so blame walks the versions in order: a line kept from
 * the previous version keeps its author (lineDiff's "same"), and a line a
 * version added is that version's author's. Options are compared whole: an
 * option whose label or consequence changed is the version's author's.
 */
import type { AskOption, DecisionDocVersion } from '../types'
import { lineDiff } from './textDiff'

export type Author = DecisionDocVersion['author_kind']

export interface BlameLine {
  text: string
  author: Author
  /** the version that introduced the line */
  version: number
}

export interface OptionBlame {
  author: Author
  version: number
}

export interface Blame {
  lines: BlameLine[]
  /** one per option of the last version, by index */
  options: OptionBlame[]
}

const sameOption = (a: AskOption, b: AskOption) => a.label === b.label && a.consequence === b.consequence

/** Blame for the last of versions (oldest first). */
export function blame(versions: DecisionDocVersion[]): Blame {
  let lines: BlameLine[] = []
  let options: (OptionBlame & { option: AskOption })[] = []
  let prevText: string | null = null
  for (const v of versions) {
    const by = { author: v.author_kind, version: v.version }
    if (prevText === null) {
      lines = v.text.replace(/\r\n/g, '\n').split('\n').map(text => ({ text, ...by }))
    } else {
      const next: BlameLine[] = []
      let i = 0 // index into the previous version's lines
      for (const d of lineDiff(prevText, v.text)) {
        if (d.kind === 'same') next.push(lines[i++] ?? { text: d.text, ...by })
        else if (d.kind === 'del') i++
        else next.push({ text: d.text, ...by })
      }
      lines = next
    }
    prevText = v.text
    const prev = options
    options = (v.ask?.options ?? []).map(option => {
      const kept = prev.find(p => sameOption(p.option, option))
      return kept ? { ...kept, option } : { ...by, option }
    })
  }
  return { lines, options: options.map(({ author, version }) => ({ author, version })) }
}

/** Consecutive lines by one author, so each run renders as one block. */
export function blameRuns(lines: BlameLine[]): { author: Author; version: number; text: string }[] {
  const runs: { author: Author; version: number; text: string }[] = []
  for (const l of lines) {
    const last = runs[runs.length - 1]
    if (last && last.author === l.author) last.text += '\n' + l.text
    else runs.push({ author: l.author, version: l.version, text: l.text })
  }
  return runs
}
