// Tiny search-param helpers shared by the Inspector (?service=) and the
// Investigation Trail (?trail=). wouter's useSearch returns the search string
// without the leading "?"; these helpers keep all param surgery in one place.

/** Read a single query param. Empty values read as null. */
export function readParam(search: string, key: string): string | null {
  const params = new URLSearchParams(
    search.startsWith('?') ? search.slice(1) : search,
  )
  const value = params.get(key)
  return value ? value : null
}

/**
 * Build an href for `navigate()`: applies `updates` on top of the current
 * search string (null deletes a key), preserving unrelated params.
 */
export function buildHref(
  path: string,
  search: string,
  updates: Record<string, string | null>,
): string {
  const params = new URLSearchParams(
    search.startsWith('?') ? search.slice(1) : search,
  )
  for (const [key, value] of Object.entries(updates)) {
    if (value === null || value === '') params.delete(key)
    else params.set(key, value)
  }
  const qs = params.toString()
  return qs ? `${path}?${qs}` : path
}
