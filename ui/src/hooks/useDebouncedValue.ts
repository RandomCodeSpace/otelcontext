import { useEffect, useState } from 'react'

/**
 * Debounce a fast-changing value (search inputs → server filters). The
 * returned value trails `value` by `delayMs`; intermediate values that are
 * replaced before the delay elapses never land.
 */
export function useDebouncedValue<T>(value: T, delayMs: number): T {
  const [debounced, setDebounced] = useState(value)

  useEffect(() => {
    const timer = setTimeout(() => setDebounced(value), delayMs)
    return () => clearTimeout(timer)
  }, [value, delayMs])

  return debounced
}
