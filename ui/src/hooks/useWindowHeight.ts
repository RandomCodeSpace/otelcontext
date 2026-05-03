import { useEffect, useState } from 'react'

/**
 * Tracks `window.innerHeight` with a `resize` listener. SSR-safe — returns
 * `fallback` on the first render when `window` is unavailable.
 *
 * Used by views that render a fixed-height canvas/chart and want it to fill
 * the visible viewport instead of being capped at an arbitrary px value.
 */
export function useWindowHeight(fallback = 800): number {
  const [h, setH] = useState<number>(() => {
    if (typeof window === 'undefined') return fallback
    return window.innerHeight
  })

  useEffect(() => {
    if (typeof window === 'undefined') return
    const update = () => setH(window.innerHeight)
    window.addEventListener('resize', update)
    return () => window.removeEventListener('resize', update)
  }, [])

  return h
}
