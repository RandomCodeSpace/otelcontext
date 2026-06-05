import { useCallback, useEffect, useRef, useState } from 'react'
import type { TrafficPoint } from '../types/api'

// GET /api/metrics/traffic returns a bare JSON array of TrafficPoint over the
// last ~30 minutes (one point per minute bucket): { timestamp, count,
// error_count }. Mirrors the useSystemGraph / useDashboard polling pattern:
// useCallback load + setInterval + clearInterval cleanup.
export function useTraffic(pollInterval = 30_000) {
  const [points, setPoints] = useState<TrafficPoint[] | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const timerRef = useRef<ReturnType<typeof setInterval>>(undefined)

  const load = useCallback(async () => {
    try {
      const res = await fetch('/api/metrics/traffic')
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      const data = (await res.json()) as TrafficPoint[]
      setPoints(Array.isArray(data) ? data : [])
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

  return { points, loading, error, reload: load }
}
