import { useEffect, useState } from 'react'

export type Theme = 'dark' | 'light'
const KEY = 'oc-theme'

function readInitial(): Theme {
  if (typeof window === 'undefined') return 'dark'
  try {
    const v = window.localStorage.getItem(KEY)
    return v === 'light' || v === 'dark' ? v : 'dark'
  } catch {
    return 'dark'
  }
}

/**
 * Single source of truth for theme. Held once at the app root and fed into the
 * design-system ThemeProvider via `mode={theme}` so the provider can no longer
 * clobber a persisted preference on mount.
 */
export function useTheme() {
  const [theme, setTheme] = useState<Theme>(readInitial)

  useEffect(() => {
    document.documentElement.setAttribute('data-theme', theme)
    try {
      window.localStorage.setItem(KEY, theme)
    } catch {
      /* storage blocked (private mode) — attribute already set */
    }
  }, [theme])

  const toggle = () => setTheme((value) => (value === 'dark' ? 'light' : 'dark'))
  return { theme, toggle }
}
