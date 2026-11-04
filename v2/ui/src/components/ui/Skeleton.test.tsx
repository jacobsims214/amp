// The loading primitives: skeletons, the delayed BandLoader, and how both
// behave under prefers-reduced-motion (index.css).
import { act, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import * as Skeletons from './Skeleton'
import { Skeleton, SkeletonCard, SkeletonRegion, SkeletonText } from './Skeleton'
import { BandLoader } from '../brand/BandLoader'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

// Read as text: vitest does not process CSS, so `?raw` imports of it come back empty.
const css = readFileSync(resolve(process.cwd(), 'src/index.css'), 'utf8')
const skeletonSource = readFileSync(resolve(process.cwd(), 'src/components/ui/Skeleton.tsx'), 'utf8')

afterEach(() => { vi.useRealTimers() })

/** The rules inside every `@media (prefers-reduced-motion: reduce)` block. */
function reducedMotionCss(): string {
  const out: string[] = []
  const marker = '@media (prefers-reduced-motion: reduce)'
  let at = css.indexOf(marker)
  while (at >= 0) {
    let i = css.indexOf('{', at) + 1, depth = 1
    const start = i
    while (depth > 0 && i < css.length) { if (css[i] === '{') depth++; if (css[i] === '}') depth--; i++ }
    out.push(css.slice(start, i - 1))
    at = css.indexOf(marker, i)
  }
  return out.join('\n')
}
const ruleFor = (block: string, selector: string) => {
  const m = block.match(new RegExp(`${selector.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}\\s*\\{([^}]*)\\}`))
  return m?.[1] ?? null
}

describe('Skeleton primitives', () => {
  it('exports Skeleton, SkeletonText and SkeletonCard, styled only through CSS variables', () => {
    expect(typeof Skeletons.Skeleton).toBe('function')
    expect(typeof Skeletons.SkeletonText).toBe('function')
    expect(typeof Skeletons.SkeletonCard).toBe('function')
    expect(skeletonSource).not.toMatch(/#[0-9a-fA-F]{3,8}\b/)
    const skel = ruleFor(css, '.amp-skel')!
    expect(skel).toContain('var(--surface-3)')
    expect(skel).toContain('var(--radius)')
    expect(skel).not.toMatch(/#[0-9a-fA-F]{3,8}\b/)
  })

  it('renders shapes hidden from assistive tech inside a status region with aria-busy', () => {
    const { container } = render(
      <SkeletonRegion label="Loading tickets">
        <Skeleton w={40} h={8} /><SkeletonText lines={3} /><SkeletonCard />
      </SkeletonRegion>,
    )
    const region = screen.getByRole('status', { name: 'Loading tickets' })
    expect(region).toHaveAttribute('aria-busy', 'true')
    expect(region).toHaveTextContent('Loading tickets') // the sr-only label
    expect(container.querySelectorAll('.amp-skel')).toHaveLength(1 + 3 + 3)
    for (const el of container.querySelectorAll('.amp-skel')) expect(el.closest('[aria-hidden="true"]')).not.toBeNull()
  })

  it('stops the sweep under reduced motion', () => {
    const rm = reducedMotionCss()
    const skel = ruleFor(rm, '.amp-skel')
    expect(skel).toMatch(/animation:\s*none\s*!important/)
  })
})

describe('BandLoader', () => {
  it('shows nothing visible when the wait resolves before 250ms', () => {
    vi.useFakeTimers()
    const { container, unmount } = render(<BandLoader label="Loading the library" />)
    act(() => { vi.advanceTimersByTime(249) })
    expect(container.querySelector('.amp-band-loader')).toBeNull()
    expect(screen.queryByText('Loading the library')).toBeNull()
    // the wait ends: the loader unmounts before ever drawing
    unmount()
    expect(container.querySelector('.amp-band-loader')).toBeNull()
  })

  it('appears at 250ms with a visible caption, "Loading" when no label is given', () => {
    vi.useFakeTimers()
    const { container } = render(<BandLoader />)
    expect(screen.getByRole('status', { name: 'Loading' })).toBeInTheDocument()
    act(() => { vi.advanceTimersByTime(250) })
    expect(container.querySelector('.amp-band-loader')).not.toBeNull()
    expect(container.querySelector('.amp-band-loader-cap')).toHaveTextContent('Loading')
  })

  it('delay={0} draws at once', () => {
    render(<BandLoader delay={0} label="Preparing" />)
    expect(screen.getByText('Preparing')).toBeInTheDocument()
  })

  it('holds a lit band strip under reduced motion instead of four grey bars', () => {
    const seg = ruleFor(reducedMotionCss(), '.amp-band-loader-seg i')!
    expect(seg).toMatch(/animation:\s*none\s*!important/)
    expect(seg).toMatch(/background:\s*var\(--lamp\)/)
    // and each segment's --lamp is its band colour
    for (const [n, band] of [[1, 4], [2, 3], [3, 2], [4, 1]]) {
      expect(ruleFor(css, `.amp-band-loader-seg i:nth-child(${n})`)).toContain(`--lamp: var(--band-${band})`)
    }
  })
})
