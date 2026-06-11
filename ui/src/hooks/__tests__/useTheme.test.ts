import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, renderHook } from '@testing-library/react'
import { useTheme } from '../useTheme'

// matchMedia mock with controllable prefers-color-scheme and change events.
type Listener = () => void

function mockMatchMedia(prefersLight: boolean) {
  const listeners = new Set<Listener>()
  const mql = {
    get matches() {
      return state.prefersLight
    },
    media: '(prefers-color-scheme: light)',
    addEventListener: (_: string, cb: Listener) => listeners.add(cb),
    removeEventListener: (_: string, cb: Listener) => listeners.delete(cb),
  }
  const state = {
    prefersLight,
    set(next: boolean) {
      state.prefersLight = next
      listeners.forEach((cb) => cb())
    },
  }
  vi.stubGlobal(
    'matchMedia',
    vi.fn((query: string) => {
      // The hook only queries prefers-color-scheme; reuse the same mql.
      void query
      return mql as unknown as MediaQueryList
    }),
  )
  return state
}

beforeEach(() => {
  window.localStorage.clear()
  document.documentElement.removeAttribute('data-theme')
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('useTheme', () => {
  it('defaults to dark when nothing is stored and the system prefers dark', () => {
    mockMatchMedia(false)
    const { result } = renderHook(() => useTheme())
    expect(result.current.theme).toBe('dark')
    expect(document.documentElement.getAttribute('data-theme')).toBe('dark')
  })

  it('honors prefers-color-scheme: light when no preference is stored', () => {
    mockMatchMedia(true)
    const { result } = renderHook(() => useTheme())
    expect(result.current.theme).toBe('light')
  })

  it('does not write localStorage until the user explicitly toggles', () => {
    mockMatchMedia(true)
    renderHook(() => useTheme())
    expect(window.localStorage.getItem('oc-theme')).toBeNull()
  })

  it('a stored preference wins over the system scheme', () => {
    mockMatchMedia(true)
    window.localStorage.setItem('oc-theme', 'dark')
    const { result } = renderHook(() => useTheme())
    expect(result.current.theme).toBe('dark')
  })

  it('toggle persists to the existing oc-theme key and updates <html>', () => {
    mockMatchMedia(false)
    const { result } = renderHook(() => useTheme())
    act(() => result.current.toggle())
    expect(result.current.theme).toBe('light')
    expect(window.localStorage.getItem('oc-theme')).toBe('light')
    expect(document.documentElement.getAttribute('data-theme')).toBe('light')
  })

  it('follows live system scheme changes while unset', () => {
    const media = mockMatchMedia(false)
    const { result } = renderHook(() => useTheme())
    expect(result.current.theme).toBe('dark')
    act(() => media.set(true))
    expect(result.current.theme).toBe('light')
  })

  it('ignores system scheme changes once a preference is stored', () => {
    const media = mockMatchMedia(false)
    window.localStorage.setItem('oc-theme', 'dark')
    const { result } = renderHook(() => useTheme())
    act(() => media.set(true))
    expect(result.current.theme).toBe('dark')
  })

  it('survives blocked localStorage (private mode)', () => {
    mockMatchMedia(false)
    const setItem = vi
      .spyOn(Storage.prototype, 'setItem')
      .mockImplementation(() => {
        throw new Error('blocked')
      })
    const { result } = renderHook(() => useTheme())
    act(() => result.current.toggle())
    expect(result.current.theme).toBe('light')
    setItem.mockRestore()
  })
})
