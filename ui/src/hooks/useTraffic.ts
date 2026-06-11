import { useCallback } from 'react'
import { useQuery } from '@tanstack/react-query'
import { apiFetch } from '../lib/apiFetch'
import type { TrafficPoint } from '../types/api'

// GET /api/metrics/traffic returns a bare JSON array of TrafficPoint over the
// last ~30 minutes (one point per minute bucket): { timestamp, count,
// error_count }. TanStack Query adapter keeping the legacy return shape
// ({ points, loading, error, reload }); the shared ['metrics-traffic'] cache
// key lets other surfaces (Triage feed sparkline) OBSERVE traffic data
// without adding their own polling.
export function useTraffic(pollInterval = 30_000) {
  const { data, isPending, error, refetch } = useQuery({
    queryKey: ['metrics-traffic'],
    queryFn: async ({ signal }) => {
      const points = await apiFetch<TrafficPoint[]>('/api/metrics/traffic', {
        signal,
      })
      return Array.isArray(points) ? points : []
    },
    refetchInterval: pollInterval > 0 ? pollInterval : false,
  })

  const reload = useCallback(() => {
    void refetch()
  }, [refetch])

  return {
    points: data ?? null,
    loading: isPending,
    error: error ? error.message : null,
    reload,
  }
}
