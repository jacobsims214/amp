import ReactMarkdown, { type Components } from 'react-markdown'

/**
 * Markdown in the house style: KB documents ('doc') and ticket text
 * ('compact', which agents write as markdown sections).
 */
export function Markdown({ children, size = 'doc' }: { children: string; size?: 'doc' | 'compact' }) {
  const doc = size === 'doc'
  const body = doc ? 'text-sm leading-7' : 'text-[13px] leading-6'
  const gap = doc ? 'mb-4' : 'mb-2.5'
  const components: Components = {
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
    a: ({ children, href }) => <a href={href} className="text-ink underline underline-offset-2 hover:text-accent">{children}</a>,
    hr: () => <hr className="border-line my-6" />,
    table: ({ children }) => <div className={`overflow-x-auto ${gap}`}><table className="w-full text-sm border-collapse">{children}</table></div>,
    th: ({ children }) => <th className="amp-label text-left px-3 py-2 bg-surface-2 border-b border-line">{children}</th>,
    td: ({ children }) => <td className="px-3 py-2 text-ink-2 text-sm border-b border-line">{children}</td>,
  }
  return <div className="min-w-0 break-words"><ReactMarkdown components={components}>{children}</ReactMarkdown></div>
}
