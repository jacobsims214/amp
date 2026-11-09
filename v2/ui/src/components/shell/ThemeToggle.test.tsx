import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import { ThemeToggle } from './ThemeToggle'
import { useThemeStore } from '../../lib/themeStore'

const matchMediaMock = (matches: boolean) =>
  vi.fn().mockImplementation(query => ({
    matches,
    media: query,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
    dispatchEvent: vi.fn(),
  }))

describe('ThemeToggle', () => {
  beforeEach(() => {
    localStorage.clear()
    document.documentElement.classList.remove('dark')
    delete document.documentElement.dataset.theme
    useThemeStore.setState({ mode: 'system', resolved: 'light' })
    window.matchMedia = matchMediaMock(false) as unknown as typeof window.matchMedia
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('renders three theme buttons', () => {
    render(<ThemeToggle />)
    expect(screen.getByRole('button', { name: /Light theme/i })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /Dark theme/i })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /System theme/i })).toBeInTheDocument()
  })

  it('toggles to dark mode and persists', async () => {
    render(<ThemeToggle />)
    await userEvent.click(screen.getByRole('button', { name: /Dark theme/i }))
    expect(useThemeStore.getState().mode).toBe('dark')
    expect(document.documentElement.classList.contains('dark')).toBe(true)
    expect(document.documentElement.dataset.theme).toBe('dark')
    expect(JSON.parse(localStorage.getItem('amp-theme')!).state.mode).toBe('dark')
  })

  it('toggles to light mode and persists', async () => {
    window.matchMedia = matchMediaMock(true) as unknown as typeof window.matchMedia
    useThemeStore.setState({ mode: 'dark', resolved: 'dark' })
    document.documentElement.classList.add('dark')
    document.documentElement.dataset.theme = 'dark'

    render(<ThemeToggle />)
    await userEvent.click(screen.getByRole('button', { name: /Light theme/i }))
    expect(useThemeStore.getState().mode).toBe('light')
    expect(document.documentElement.classList.contains('dark')).toBe(false)
    expect(document.documentElement.dataset.theme).toBe('light')
  })

  it('reflects system preference', async () => {
    window.matchMedia = matchMediaMock(true) as unknown as typeof window.matchMedia
    render(<ThemeToggle />)
    await userEvent.click(screen.getByRole('button', { name: /System theme/i }))
    expect(useThemeStore.getState().mode).toBe('system')
    expect(useThemeStore.getState().resolved).toBe('dark')
    expect(document.documentElement.classList.contains('dark')).toBe(true)
  })
})
