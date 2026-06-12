import { useCallback, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import type { TriageQueryOptions } from '@/lib/triageVerbs'

// Run/cancel wrapper for the Inspector's on-demand MCP verbs. Unlike a plain
// useQuery, nothing fires on mount: the RPC starts on run() (or when the
// cache already holds an entry — a previous run within the 5-minute window,
// or a palette prefetch in flight). cancel() aborts the RPC via the query
// AbortSignal and drops the cache entry so the tab returns to a clean idle.

export type TriageToolStatus = 'idle' | 'loading' | 'error' | 'success'

export interface TriageTool<T> {
  status: TriageToolStatus
  data: T | undefined
  error: string | null
  /** Start (or re-run) the analysis. */
  run: () => void
  /** Abort the in-flight RPC and return to idle. */
  cancel: () => Promise<void>
}

export function useTriageTool<T>(options: TriageQueryOptions<T>): TriageTool<T> {
  const queryClient = useQueryClient()
  const [requested, setRequested] = useState(false)
  // A cache entry (settled or in-flight) re-enables the query without run():
  // tab flips inside the cache window render instantly, and a palette
  // prefetch is observed instead of duplicated.
  const cached = queryClient.getQueryState(options.queryKey) !== undefined
  const query = useQuery({ ...options, enabled: requested || cached })

  const run = useCallback(() => {
    setRequested(true)
    // Re-run on demand when a (possibly stale) result is already showing.
    if (queryClient.getQueryState(options.queryKey)?.status === 'success') {
      void query.refetch()
    }
  }, [query, queryClient, options.queryKey])

  const cancel = useCallback(async () => {
    setRequested(false)
    await queryClient.cancelQueries({ queryKey: options.queryKey })
    // Drop the aborted entry — otherwise its presence would keep the query
    // enabled and a focus refetch could fire the RPC uninvited.
    queryClient.removeQueries({ queryKey: options.queryKey })
  }, [queryClient, options.queryKey])

  let status: TriageToolStatus = 'idle'
  if (query.isFetching) status = 'loading'
  else if (query.isError) status = 'error'
  else if (query.isSuccess) status = 'success'

  return {
    status,
    data: query.data,
    error: query.error ? query.error.message : null,
    run,
    cancel,
  }
}
