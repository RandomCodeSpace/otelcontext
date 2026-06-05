import { useCallback, useEffect, useRef, useState } from 'react'

// GET /ready (Kubernetes-style readiness probe). Returns 200 when ready, 503
// when a check fails — BOTH carry the same parseable JSON body, so we read it
// regardless of status rather than throwing on 503. Check values are strings:
// "ok" | "skipped" | "not running" | "saturated NN%" | an error message.
export interface ReadyResponse {
  ready: boolean
  checks: Record<string, string>
}

export function useReady(pollInterval = 30_000) {
  const [ready, setReady] = useState<ReadyResponse | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const timerRef = useRef<ReturnType<typeof setInterval>>(undefined)

  const load = useCallback(async () => {
    try {
      const res = await fetch('/ready')
      // 200 and 503 both return the breakdown body; only a non-JSON / network
      // failure is a real fetch error.
      const data = (await res.json()) as ReadyResponse
      setReady({ ready: Boolean(data?.ready), checks: data?.checks ?? {} })
      setError(null)
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'fetch failed')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    load()
    timerRef.current = setInterval(load, pollInterval)
    return () => clearInterval(timerRef.current)
  }, [load, pollInterval])

  return { ready, loading, error, reload: load }
}
