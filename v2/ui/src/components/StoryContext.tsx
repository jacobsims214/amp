/**
 * "Story context" (#133): the parent story's description and acceptance
 * criteria, and the epic's description, as markdown, folded by default. The
 * planner writes them when planning; without this the UI only used them to
 * prefill edit forms, so a person deciding on a ticket couldn't see the goal
 * that explains it. Renders nothing when there's nothing to show.
 */
import { useId, useState } from 'react'
import { ChevronDown, ChevronRight } from 'lucide-react'
import { Markdown } from './ui/Markdown'

interface Props {
  story?: { description?: string; acceptance_criteria?: string } | null
  epic?: { name?: string; description?: string } | null
  projectId?: number
  /** the toggle's words */
  label?: string
}

export function StoryContext({ story, epic, projectId, label = 'Story context' }: Props) {
  const [open, setOpen] = useState(false)
  const id = useId()
  const parts = [
    { key: 'story', title: 'Story', text: story?.description?.trim() },
    { key: 'ac', title: 'Story acceptance criteria', text: story?.acceptance_criteria?.trim() },
    { key: 'epic', title: epic?.name ? `Epic: ${epic.name}` : 'Epic', text: epic?.description?.trim() },
  ].filter((p): p is { key: string; title: string; text: string } => !!p.text)
  if (parts.length === 0) return null
  return (
    <div data-testid="story-context">
      <button type="button" aria-expanded={open} aria-controls={id} onClick={() => setOpen(o => !o)}
        className="inline-flex items-center gap-1 text-[12px] text-ink-2 hover:text-ink cursor-pointer">
        {open ? <ChevronDown size={12} aria-hidden="true" /> : <ChevronRight size={12} aria-hidden="true" />}{label}
      </button>
      {open && (
        <div id={id} className="mt-2 grid gap-3 pl-3 border-l-2 border-line">
          {parts.map(p => (
            <div key={p.key}>
              <div className="amp-label mb-1">{p.title}</div>
              <Markdown size="compact" projectId={projectId}>{p.text}</Markdown>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
