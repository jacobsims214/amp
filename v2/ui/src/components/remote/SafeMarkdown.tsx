/**
 * Markdown for untrusted agent and tool text on the remote session page
 * (protocol 6.4 "Rendering", threat model T-UI-4):
 *  - no raw HTML: react-markdown (without rehype-raw) shows HTML in the source
 *    as literal text, never as elements (the tests pin this);
 *  - no images: an image becomes a text placeholder with its URL, so nothing
 *    is fetched (images are a prompt-injection exfiltration channel);
 *  - links only for http(s) and mailto, always showing the full URL, opened
 *    in a new tab without referrer or opener.
 */
import ReactMarkdown, { type Components } from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { safeUrl } from '../../lib/safeUrl'


const nodeText = (children: React.ReactNode): string =>
  Array.isArray(children) ? children.map(nodeText).join('')
    : typeof children === 'string' || typeof children === 'number' ? String(children) : ''

const components: Components = {
  h1: ({ children }) => <h3 className="text-[14px] font-semibold text-ink mt-3 mb-1 first:mt-0">{children}</h3>,
  h2: ({ children }) => <h4 className="text-[13.5px] font-semibold text-ink mt-3 mb-1 first:mt-0">{children}</h4>,
  h3: ({ children }) => <h5 className="text-[13px] font-semibold text-ink mt-3 mb-1 first:mt-0">{children}</h5>,
  p: ({ children }) => <p className="text-[14px] leading-6 text-ink mb-2 last:mb-0">{children}</p>,
  code: ({ children, className }) => className?.includes('language-')
    ? <pre className="p-2.5 text-xs leading-relaxed overflow-x-auto mb-2 font-mono bg-surface-2 border border-line text-ink"><code>{children}</code></pre>
    : <code className="px-1 py-px text-[0.85em] font-mono bg-surface-2 text-ink border border-line break-words">{children}</code>,
  ul: ({ children }) => <ul className="text-[14px] leading-6 text-ink list-disc pl-5 mb-2 space-y-0.5">{children}</ul>,
  ol: ({ children }) => <ol className="text-[14px] leading-6 text-ink list-decimal pl-5 mb-2 space-y-0.5">{children}</ol>,
  blockquote: ({ children }) => <blockquote className="pl-3 my-2 border-l-[3px] border-line-str text-ink-2">{children}</blockquote>,
  table: ({ children }) => <div className="overflow-x-auto mb-2"><table className="text-[13px] border-collapse">{children}</table></div>,
  th: ({ children }) => <th className="text-left px-2 py-1 bg-surface-2 border border-line">{children}</th>,
  td: ({ children }) => <td className="px-2 py-1 border border-line">{children}</td>,
  a: ({ href, children }) => {
    const url = safeUrl(href ?? '')
    const label = nodeText(children)
    if (!url) return <span className="text-ink">{children}{href ? <span className="font-mono text-[12px] text-ink-3"> ({href})</span> : null}</span>
    return (
      <a href={url} target="_blank" rel="noopener noreferrer nofollow" className="text-link underline underline-offset-2 break-all">
        {label && label !== url ? <>{children} <span className="font-mono text-[12px]">({url})</span></> : <span className="font-mono text-[12.5px]">{url}</span>}
      </a>
    )
  },
  img: ({ src, alt }) => (
    <span className="inline-flex items-baseline gap-1 px-1 border border-dashed border-line-str text-[12px] text-ink-2" data-blocked-image="">
      [image not loaded{alt ? `: ${alt}` : ''}]{typeof src === 'string' && src ? <span className="font-mono break-all">{src}</span> : null}
    </span>
  ),
}

export function SafeMarkdown({ children }: { children: string }) {
  return (
    <div className="min-w-0 break-words">
      <ReactMarkdown remarkPlugins={[remarkGfm]} urlTransform={safeUrl} components={components}>
        {children}
      </ReactMarkdown>
    </div>
  )
}
