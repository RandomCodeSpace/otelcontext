import { useMemo, useState, useSyncExternalStore } from 'react'
import { useInfiniteQuery, useQuery } from '@tanstack/react-query'
import { Pause, Play } from 'lucide-react'
import { useLocation, useSearch } from 'wouter'
import { apiFetch } from '@/lib/apiFetch'
import {
  SEVERITY_PILLS,
  countNewSince,
  filterBySeverity,
  severityCounts,
  type PillSeverity,
} from '@/lib/logRows'
import { getWsManager } from '@/lib/wsManager'
import { useDebouncedValue } from '@/hooks/useDebouncedValue'
import type { LogEntry, LogsResponse } from '@/types/api'
import LogList from './LogList'
import styles from './LogsView.module.css'

const PAGE_SIZE = 100

type Mode = 'live' | 'search' | 'context'

function searchPath(
  q: string,
  traceId: string,
  service: string,
  severity: PillSeverity | null,
  offset: number,
): string {
  const params = new URLSearchParams()
  if (q) params.set('search', q)
  if (traceId) params.set('trace_id', traceId)
  if (service) params.set('service_name', service)
  if (severity) params.set('severity', severity)
  params.set('limit', String(PAGE_SIZE))
  params.set('offset', String(offset))
  return `/api/logs?${params.toString()}`
}

/**
 * /logs — live tail over the /ws ring buffer with severity pills and a
 * server-backed search mode (GET /api/logs). Search results replace the
 * tail; "Back to live" returns. "Show context" on any row pivots to the
 * ±1min window around it (GET /api/logs/context).
 */
export default function LogsView() {
  const ws = getWsManager()
  const [, navigate] = useLocation()
  const urlParams = useSearch()
  // Deep-link params: ?trace= correlates a trace's logs, ?service= scopes a
  // server-side search to one service (the palette's "Search logs…" verb).
  const { traceId, serviceParam } = useMemo(() => {
    const params = new URLSearchParams(urlParams)
    return {
      traceId: params.get('trace') ?? '',
      serviceParam: params.get('service') ?? '',
    }
  }, [urlParams])

  const [severity, setSeverity] = useState<PillSeverity | null>(null)
  const [searchText, setSearchText] = useState('')
  const debouncedSearch = useDebouncedValue(searchText.trim(), 300)
  const [contextAnchor, setContextAnchor] = useState<LogEntry | null>(null)
  // Pause freezes a copy of the buffer (plus the appended total at that
  // moment) so the operator can read forensically while WS keeps filling.
  const [frozen, setFrozen] = useState<{
    logs: readonly LogEntry[]
    total: number
  } | null>(null)
  const paused = frozen !== null

  let mode: Mode = 'live'
  if (contextAnchor) mode = 'context'
  else if (debouncedSearch || traceId || serviceParam) mode = 'search'

  // ---- live tail. Renders are driven by the WsManager version counter
  // (one bump per 250ms flush tick), so filtering/counting per render IS
  // "evaluated on the flush tick" — never per keystroke or per frame.
  useSyncExternalStore(ws.subscribeLogs, ws.getLogsVersion)
  const appendedTotal = ws.getLogsTotal()
  const bufferLogs = frozen ? frozen.logs : ws.getLogs()
  const liveLogs = filterBySeverity(bufferLogs, severity)
  const counts = severityCounts(bufferLogs)

  const togglePause = () => {
    setFrozen((prev) =>
      prev === null
        ? { logs: [...ws.getLogs()], total: ws.getLogsTotal() }
        : null,
    )
  }

  // ---- search mode (server-backed, offset paging)
  const searchQuery = useInfiniteQuery({
    queryKey: ['logs', 'search', debouncedSearch, traceId, serviceParam, severity],
    queryFn: ({ pageParam, signal }) =>
      apiFetch<LogsResponse>(
        searchPath(debouncedSearch, traceId, serviceParam, severity, pageParam),
        { signal },
      ),
    initialPageParam: 0,
    getNextPageParam: (last, pages) => {
      const loaded = pages.reduce((n, p) => n + p.data.length, 0)
      return loaded < last.total && last.data.length > 0 ? loaded : undefined
    },
    enabled: mode === 'search',
  })
  const searchLogs = useMemo(
    () => searchQuery.data?.pages.flatMap((p) => p.data) ?? [],
    [searchQuery.data],
  )

  // ---- context mode (±1min window around an anchor row)
  const contextQuery = useQuery({
    queryKey: ['logs', 'context', contextAnchor?.timestamp],
    queryFn: ({ signal }) =>
      apiFetch<LogEntry[]>(
        `/api/logs/context?timestamp=${encodeURIComponent(contextAnchor!.timestamp)}`,
        { signal },
      ),
    enabled: contextAnchor !== null,
  })
  const contextLogs = useMemo(
    () =>
      [...(contextQuery.data ?? [])].sort((a, b) =>
        a.timestamp < b.timestamp ? -1 : 1,
      ),
    [contextQuery.data],
  )

  const backToLive = () => {
    setContextAnchor(null)
    setSearchText('')
    if (traceId || serviceParam) {
      // Drop the deep-link params without adding a history entry.
      navigate('/logs', { replace: true })
    }
  }

  const activeQuery = mode === 'context' ? contextQuery : searchQuery
  const logs =
    mode === 'live' ? liveLogs : mode === 'search' ? searchLogs : contextLogs
  const missedWhilePaused = frozen
    ? countNewSince(appendedTotal, frozen.total)
    : 0

  return (
    <div className={styles.page}>
      <div className={styles.toolbar}>
        <div
          className={styles.pills}
          role="group"
          aria-label="Filter by severity"
        >
          {SEVERITY_PILLS.map((sev) => (
            <button
              key={sev}
              type="button"
              className={`${styles.pill} ${severity === sev ? styles.pillActive : ''}`}
              aria-pressed={severity === sev}
              onClick={() => setSeverity((cur) => (cur === sev ? null : sev))}
            >
              {sev}
              {mode === 'live' && (
                <span className={styles.pillCount}>{counts[sev]}</span>
              )}
            </button>
          ))}
        </div>

        <div className={styles.searchWrap}>
          <input
            type="search"
            className={styles.search}
            aria-label="Search"
            placeholder="Search log bodies (server-side, last 24h)…"
            value={searchText}
            onChange={(e) => setSearchText(e.target.value)}
          />
        </div>

        {mode === 'live' ? (
          <button
            type="button"
            className={styles.toolButton}
            onClick={togglePause}
            aria-pressed={paused}
          >
            {paused ? <Play size={12} aria-hidden /> : <Pause size={12} aria-hidden />}
            {paused ? 'Resume' : 'Pause'}
          </button>
        ) : (
          <button
            type="button"
            className={styles.toolButton}
            onClick={backToLive}
          >
            ← Back to live
          </button>
        )}

        <span className={styles.modeNote} aria-live="polite">
          {mode === 'live' && !paused && `live · ${logs.length} in buffer`}
          {mode === 'live' && paused && 'paused'}
          {mode === 'search' &&
            (traceId
              ? `trace ${traceId.slice(0, 12)}…`
              : `${serviceParam ? `${serviceParam} · ` : ''}${searchQuery.data?.pages[0]?.total ?? '…'} matches`)}
          {mode === 'context' && 'surrounding ±1min'}
        </span>
      </div>

      {mode === 'live' && paused && (
        <div className={styles.pausedBanner}>
          Tail paused — {missedWhilePaused} new entries since
        </div>
      )}

      <div className={styles.listWrap}>
        {mode !== 'live' && activeQuery.isPending ? (
          <div className={styles.skeleton} aria-hidden="true">
            {Array.from({ length: 14 }, (_, i) => (
              <div key={i} className={styles.skeletonRow} />
            ))}
          </div>
        ) : mode !== 'live' && activeQuery.isError ? (
          <div className={styles.state} role="alert">
            <span className={styles.stateError}>
              {mode === 'context'
                ? 'GET /api/logs/context failed'
                : 'GET /api/logs failed'}
            </span>
            <span className={styles.stateHint}>
              {(activeQuery.error as Error).message}
            </span>
            <button
              type="button"
              className={styles.toolButton}
              onClick={() => activeQuery.refetch()}
            >
              Retry
            </button>
          </div>
        ) : logs.length === 0 ? (
          <div className={styles.state}>
            {mode === 'live' ? (
              <>
                <span>Waiting for logs…</span>
                <span className={styles.stateHint}>
                  Entries stream in live over /ws as they are ingested. No
                  data yet? Point an OTLP exporter at this host (gRPC :4317
                  or HTTP /v1/logs) — see the Connect popover in the top
                  bar.
                </span>
              </>
            ) : (
              <>
                <span>No matching logs</span>
                <span className={styles.stateHint}>
                  Keyword search is capped to the last 24 hours. Try a
                  different term or severity, or go back to the live tail.
                </span>
                <button
                  type="button"
                  className={styles.toolButton}
                  onClick={backToLive}
                >
                  Back to live
                </button>
              </>
            )}
          </div>
        ) : (
          <LogList
            logs={logs}
            live={mode === 'live' && !paused}
            appendedTotal={appendedTotal}
            highlightId={contextAnchor?.id}
            onShowContext={setContextAnchor}
          />
        )}
        {mode === 'search' && searchQuery.hasNextPage && (
          <button
            type="button"
            className={styles.toolButton}
            onClick={() => searchQuery.fetchNextPage()}
            disabled={searchQuery.isFetchingNextPage}
          >
            {searchQuery.isFetchingNextPage ? 'Loading…' : 'Load older'}
          </button>
        )}
      </div>
    </div>
  )
}
