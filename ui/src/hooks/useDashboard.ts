import { useCallback } from 'react';
import { useQuery } from '@tanstack/react-query';
import { apiFetch } from '../lib/apiFetch';
import type { DashboardStats, RepoStats } from '../types/api';

// TanStack Query adapter. Keeps the legacy return shape
// ({ dashboard, stats, loading, error, reload }) for existing consumers.
// The two endpoints become independent cache entries, so any other
// surface (e.g. the Pulse bar) polling the same keys shares one request.
export function useDashboard(pollInterval = 30_000) {
  const refetchInterval = pollInterval > 0 ? pollInterval : false;

  const dash = useQuery({
    queryKey: ['metrics-dashboard'],
    queryFn: ({ signal }) =>
      apiFetch<DashboardStats>('/api/metrics/dashboard', { signal }),
    refetchInterval,
  });
  const stats = useQuery({
    queryKey: ['stats'],
    queryFn: ({ signal }) => apiFetch<RepoStats>('/api/stats', { signal }),
    refetchInterval,
  });

  // refetch is referentially stable in TanStack v5; destructured so the
  // dependency array doesn't have to carry the whole query result objects.
  const { refetch: refetchDashboard } = dash;
  const { refetch: refetchStats } = stats;
  const reload = useCallback(() => {
    void refetchDashboard();
    void refetchStats();
  }, [refetchDashboard, refetchStats]);

  return {
    dashboard: dash.data ?? null,
    stats: stats.data ?? null,
    loading: dash.isPending || stats.isPending,
    error: dash.error?.message ?? stats.error?.message ?? null,
    reload,
  };
}
