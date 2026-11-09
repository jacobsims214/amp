import { useMemo, type ReactNode } from 'react'
import ReactMarkdown, { type Components, type Options } from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { ExternalLink } from 'lucide-react'
import { Link, useInRouterContext, useParams } from 'react-router-dom'
import { classifyHref } from '../../lib/links'
import remarkTicketRefs from './remarkTicketRefs'

const LINK = 'text-ink underline underline-offset-2 hover:text-accent'

/**
 * A markdown link: AMP routes open in-app (react-router, so the SPA keeps its
 * state; /sessions/, /api/ and /home get a full page load), other http(s)/mailto links open in a new tab without opener or
 * referrer, and anything else (javascript:, data:, bare relative paths) is
 * shown as its text only.
 */
function MarkdownLink({ href, ticketRef, children }: { href?: string; ticketRef?: string; children: ReactNode }) {
  const inRouter = useInRouterContext()
  const target = classifyHref(href)
  const data = ticketRef ? { 'data-ticket-ref': ticketRef } : {}
  if (target.kind === 'blocked') return <span className="text-ink">{children}</span>
  if (target.kind === 'internal') {
    // fullLoad paths (/sessions/, /api/, /home) need a real page load.
    return inRouter && !target.fullLoad
      ? <Link to={target.to} className={LINK} {...data}>{children}</Link>
      : <a href={target.to} className={LINK} {...data}>{children}</a>
  }
  return (
    <a href={target.href} target="_blank" rel="noopener noreferrer" title={target.href} className={LINK}>
      {children}<ExternalLink size={11} aria-hidden="true" className="inline-block align-baseline ml-0.5 text-ink-3" />
    </a>
  )
}

/**
 * Markdown in the house style: KB documents ('doc') and ticket text
 * ('compact', which agents write as markdown sections).
 *
 * Ticket refs like "#34" link to the ticket on the Board when a project is
 * known (the `projectId` prop, or the current /project/:projectId route).
 * Compact (ticket) text never loads images: agents write it, and a remote
 * image is an exfiltration channel (as in SafeMarkdown).
 */
export function Markdown({ children, size = 'doc', projectId }: { children: string; size?: 'doc' | 'compact'; projectId?: number | string }) {
  const routeProjectId = useParams().projectId
  const project = projectId ?? routeProjectId
  const doc = size === 'doc'
  const body = doc ? 'text-sm leading-7' : 'text-[13px] leading-6'
  const gap = doc ? 'mb-4' : 'mb-2.5'
  // Stable between renders: new element types would remount the subtree and wipe DOM-applied marks (e.g. the Library's match highlight).
  const components: Components = useMemo(() => ({
    h1: ({ children }) => doc
      ? <h1 className="amp-heading text-[22px] text-ink mt-8 mb-3 pb-2 border-b border-line first:mt-0">{children}</h1>
      : <h3 className="text-[14px] font-semibold text-ink mt-4 mb-1.5 first:mt-0">{children}</h3>,
    h2: ({ children }) => doc
      ? <h2 className="text-[17px] font-semibold text-ink mt-7 mb-2">{children}</h2>
      : <h4 className="font-label text-[11.5px] font-semibold uppercase tracking-[0.14em] text-ink mt-4 mb-1 first:mt-0">{children}</h4>,
    h3: ({ children }) => <h3 className="font-label text-[12px] font-semibold uppercase tracking-[0.12em] text-ink mt-4 mb-1 first:mt-0">{children}</h3>,
    p: ({ children }) => <p className={`${body} text-ink-2 ${gap} last:mb-0`}>{children}</p>,
    code: ({ children, className }) => className?.includes('language-')
      ? <pre className={`p-3 text-xs leading-relaxed overflow-x-auto ${gap} font-mono bg-surface-2 border border-line text-ink`}><code>{children}</code></pre>
      : <code className="px-1 py-px text-[0.85em] font-mono bg-surface-2 text-ink border border-line break-words">{children}</code>,
    ul: ({ children }) => <ul className={`${body} text-ink-2 list-disc list-outside ${gap} space-y-1 pl-5`}>{children}</ul>,
    ol: ({ children }) => <ol className={`${body} text-ink-2 list-decimal list-outside ${gap} space-y-1 pl-5`}>{children}</ol>,
    li: ({ children }) => <li>{children}</li>,
    blockquote: ({ children }) => <blockquote className={`pl-4 my-3 ${body} text-ink-2 border-l-[3px] border-ink`}>{children}</blockquote>,
    strong: ({ children }) => <strong className="text-ink font-semibold">{children}</strong>,
    a: ({ children, href, ...rest }) => {
      const ref = (rest as Record<string, unknown>)['data-ticket-ref']
      return <MarkdownLink href={href} ticketRef={typeof ref === 'string' ? ref : undefined}>{children}</MarkdownLink>
    },
    ...(doc ? {} : {
      img: ({ src, alt }) => (
        <span className="px-1 border border-dashed border-line-str text-[12px] text-ink-2 break-all" data-blocked-image="">
          [image not loaded{alt ? `: ${alt}` : ''}]{typeof src === 'string' && src ? <span className="font-mono"> {src}</span> : null}
        </span>
      ),
    } satisfies Components),
    hr: () => <hr className="border-line my-6" />,
    table: ({ children }) => <div className={`overflow-x-auto ${gap}`}><table className="w-full text-sm border-collapse">{children}</table></div>,
    th: ({ children }) => <th className="amp-label text-left px-3 py-2 bg-surface-2 border-b border-line">{children}</th>,
    td: ({ children }) => <td className="px-3 py-2 text-ink-2 text-sm border-b border-line">{children}</td>,
  }), [doc, body, gap])
  const plugins: Options['remarkPlugins'] = project != null && project !== ''
    ? [remarkGfm, [remarkTicketRefs, { projectId: project }]]
    : [remarkGfm]
  return <div className="min-w-0 break-words"><ReactMarkdown remarkPlugins={plugins} components={components}>{children}</ReactMarkdown></div>
}
