import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom'
import { describe, expect, it } from 'vitest'
import { Markdown } from './Markdown'
import { classifyHref } from '../../lib/links'

function Where() {
  const loc = useLocation()
  return <div data-testid="where">{loc.pathname}{loc.search}</div>
}

const inRouter = (ui: React.ReactNode, path = '/') => render(
  <MemoryRouter initialEntries={[path]}>
    <Routes>
      <Route path="*" element={<>{ui}<Where /></>} />
    </Routes>
  </MemoryRouter>,
)

describe('classifyHref', () => {
  const origin = 'https://amp.example'
  it('treats root-relative paths and same-origin URLs as internal', () => {
    expect(classifyHref('/project/3/kb', origin)).toEqual({ kind: 'internal', to: '/project/3/kb' })
    expect(classifyHref('https://amp.example/project/158/board?focus=34#x', origin)).toEqual({ kind: 'internal', to: '/project/158/board?focus=34#x' })
  })
  it('treats other http(s) and mailto as external', () => {
    expect(classifyHref('https://github.com/x', origin)).toEqual({ kind: 'external', href: 'https://github.com/x' })
    expect(classifyHref('mailto:a@b.example', origin)).toEqual({ kind: 'external', href: 'mailto:a@b.example' })
  })
  it('blocks protocol-relative, relative, script and data URLs', () => {
    for (const h of ['//evil.example/x', '/\\evil.example', 'javascript:alert(1)', 'data:text/html,x', 'foo/bar', '', undefined]) {
      expect(classifyHref(h, origin)).toEqual({ kind: 'blocked' })
    }
  })
})

describe('Markdown links', () => {
  it('opens external links in a new tab without opener or referrer', () => {
    render(<Markdown>{'[PR](https://github.com/x/y/pull/1) and https://github.com/x'}</Markdown>)
    const links = screen.getAllByRole('link')
    expect(links).toHaveLength(2)
    expect(links[0]).toHaveAttribute('href', 'https://github.com/x/y/pull/1')
    expect(links[1]).toHaveAttribute('href', 'https://github.com/x')
    for (const a of links) {
      expect(a).toHaveAttribute('target', '_blank')
      expect(a.getAttribute('rel')).toContain('noopener')
      expect(a.getAttribute('rel')).toContain('noreferrer')
    }
  })

  it('routes internal links in-app', async () => {
    inRouter(<Markdown>{'[kb](/project/3/kb)'}</Markdown>)
    const a = screen.getByRole('link', { name: 'kb' })
    expect(a).not.toHaveAttribute('target')
    await userEvent.click(a)
    expect(screen.getByTestId('where')).toHaveTextContent('/project/3/kb')
  })

  it('renders an absolute URL on this origin as an in-app link to its path and query', async () => {
    inRouter(<Markdown>{`[t](${window.location.origin}/project/158/board?focus=34)`}</Markdown>)
    const a = screen.getByRole('link', { name: 't' })
    expect(a).toHaveAttribute('href', '/project/158/board?focus=34')
    expect(a).not.toHaveAttribute('target')
    await userEvent.click(a)
    expect(screen.getByTestId('where')).toHaveTextContent('/project/158/board?focus=34')
  })

  it('drops javascript: links but keeps their text', () => {
    const { container } = render(<Markdown>{'[x](javascript:alert(1))'}</Markdown>)
    expect(container.querySelector('a')).toBeNull()
    expect(container.textContent).toContain('x')
  })

  it('never renders raw HTML as elements', () => {
    const { container } = render(<Markdown>{'hi <img src="https://attacker.example/x.png" onerror="alert(1)"> <script>alert(2)</script>'}</Markdown>)
    for (const tag of ['img', 'script']) expect(container.querySelector(tag)).toBeNull()
    expect(container.querySelector('[onerror]')).toBeNull()
    expect(container.textContent).toContain('<script>alert(2)</script>')
  })

  it('does not load images in compact (ticket) text', () => {
    const { container } = render(<Markdown size="compact">{'![a](https://evil.example/p.png)'}</Markdown>)
    expect(container.querySelector('img')).toBeNull()
    expect(container.querySelector('[data-blocked-image]')).not.toBeNull()
    expect(container.textContent).toContain('https://evil.example/p.png')
  })

  it('renders outside a Router without throwing', () => {
    render(<Markdown>{'[kb](/project/3/kb) and #34'}</Markdown>)
    expect(screen.getByRole('link', { name: 'kb' })).toHaveAttribute('href', '/project/3/kb')
  })
})

describe('Markdown ticket refs', () => {
  it('links #34 to the ticket on the Board', async () => {
    inRouter(<Markdown projectId={7}>{'blocked on #34'}</Markdown>)
    const a = screen.getByRole('link', { name: '#34' })
    expect(a).toHaveAttribute('href', '/project/7/board?focus=34')
    expect(a).toHaveAttribute('data-ticket-ref', '34')
    await userEvent.click(a)
    expect(screen.getByTestId('where')).toHaveTextContent('/project/7/board?focus=34')
  })

  it('uses the project from the route when no projectId is given', () => {
    render(
      <MemoryRouter initialEntries={['/project/9/mission']}>
        <Routes><Route path="/project/:projectId/mission" element={<Markdown>{'see #5'}</Markdown>} /></Routes>
      </MemoryRouter>,
    )
    expect(screen.getByRole('link', { name: '#5' })).toHaveAttribute('href', '/project/9/board?focus=5')
  })

  it('leaves inline and fenced code alone', () => {
    const { container } = render(<Markdown projectId={7}>{'`#34`\n\n```\nfix #34\n```'}</Markdown>)
    expect(container.querySelector('a')).toBeNull()
    expect(container.textContent).toContain('#34')
  })

  it('does not nest a ticket link inside an existing link', () => {
    render(<Markdown projectId={7}>{'[x](https://a.example/#34) and https://a.example/#34'}</Markdown>)
    const links = screen.getAllByRole('link')
    expect(links).toHaveLength(2)
    for (const a of links) expect(a).toHaveAttribute('href', 'https://a.example/#34')
  })

  it('ignores C# and character references', () => {
    const { container } = render(<Markdown projectId={7}>{'C# and C#34 and a&#34; and x/#34'}</Markdown>)
    expect(container.querySelector('a')).toBeNull()
  })

  it('renders #34 as plain text without a project', () => {
    const { container } = inRouter(<Markdown>{'blocked on #34'}</Markdown>)
    expect(container.querySelector('a')).toBeNull()
    expect(container.textContent).toContain('blocked on #34')
  })

  it('leaves headings unaffected', () => {
    render(<Markdown projectId={7}>{'# Title\n\nbody'}</Markdown>)
    expect(screen.getByRole('heading', { name: 'Title' })).toBeInTheDocument()
    expect(screen.queryByRole('link')).toBeNull()
  })
})
