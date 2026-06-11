import { useCallback, useEffect, useState } from 'react'

export type Theme = 'dark' | 'light'
// Existing storage key kept verbatim — zero migration for current users.
const KEY = 'oc-theme'
const LIGHT_QUERY = '(prefers-color-scheme: light)'

function readStored(): Theme | null {
  if (typeof window === 'undefined') return null
  try {
    const v = window.localStorage.getItem(KEY)
    return v === 'light' || v === 'dark' ? v : null
  } catch {
    return null
  }
}

function systemTheme(): Theme {
  if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') {
    return 'dark' // dark-first product default
  }
  return window.matchMedia(LIGHT_QUERY).matches ? 'light' : 'dark'
}

/**
 * Single source of truth for theme. Stored preference (oc-theme) wins;
 * when unset, prefers-color-scheme is honored — live, so an OS scheme
 * flip retints the UI until the user explicitly toggles. Persisting only
 * happens on an explicit toggle, never on mount.
 */
export function useTheme() {
  const [stored, setStored] = useState<Theme | null>(readStored)
  const [system, setSystem] = useState<Theme>(systemTheme)
  const theme: Theme = stored ?? system

  // Track the OS scheme while no explicit preference exists.
  useEffect(() => {
    if (typeof window.matchMedia !== 'function') return
    const mql = window.matchMedia(LIGHT_QUERY)
    const update = () => setSystem(mql.matches ? 'light' : 'dark')
    mql.addEventListener('change', update)
    return () => mql.removeEventListener('change', update)
  }, [])

  useEffect(() => {
    document.documentElement.setAttribute('data-theme', theme)
  }, [theme])

  const toggle = useCallback(() => {
    const next: Theme = theme === 'dark' ? 'light' : 'dark'
    setStored(next)
    try {
      window.localStorage.setItem(KEY, next)
    } catch {
      /* storage blocked (private mode) — state still flips */
    }
  }, [theme])

  return { theme, toggle }
}
