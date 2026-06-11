import { useCallback } from 'react';
import { useQuery } from '@tanstack/react-query';
import { apiFetchWithResponse } from '../lib/apiFetch';
import type { SystemGraphResponse } from '../types/api';

// TanStack Query adapter. Keeps the legacy return shape
// ({ graph, cache, loading, error, reload }) so existing consumers
// (ServicesView, DashboardView) are untouched, while gaining request
// dedup, AbortSignal cancellation, and hidden-tab polling pause from
// the shared query client.
export function useSystemGraph(pollInterval = 60_000) {
  const { data, isPending, error, refetch } = useQuery({
    queryKey: ['system-graph'],
    queryFn: async ({ signal }) => {
      const { data: graph, response } =
        await apiFetchWithResponse<SystemGraphResponse>('/api/system/graph', {
          signal,
        });
      return { graph, cache: response.headers.get('X-Cache') ?? '' };
    },
    refetchInterval: pollInterval > 0 ? pollInterval : false,
  });

  const reload = useCallback(() => {
    void refetch();
  }, [refetch]);

  return {
    graph: data?.graph ?? null,
    cache: data?.cache ?? '',
    loading: isPending,
    error: error ? error.message : null,
    reload,
  };
}
