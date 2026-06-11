import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useInfiniteQuery, useQuery } from '@tanstack/react-query'
import { useVirtualizer } from '@tanstack/react-virtual'
import { useLocation, useSearch } from 'wouter'
import ToneBadge from '@/components/common/ToneBadge'
import { useDebouncedValue } from '@/hooks/useDebouncedValue'
import { useMediaQuery } from '@/hooks/useMediaQuery'
import { apiFetch } from '@/lib/apiFetch'
import { formatMs } from '@/lib/format'
import {
  durationBarFrac,
  parseTraceFilters,
  statusLabel,
  statusTone,
  traceFiltersToSearch,
  visibleP99Ms,
  type TraceFilters,
} from '@/lib/traceRows'
import type { TracesResponse } from '@/types/api'
import TraceDetail from './TraceDetail'
import styles from './TracesView.module.css'

const PAGE_SIZE = 50
const ROW_H = 32
const ROW_H_TOUCH = 56

const STATUS_OPTIONS = [
  { value: '', label: 'All statuses' },
  { value: 'ERROR', label: 'Errors' },
  { value: 'OK', label: 'OK' },
  { value: 'UNSET', label: 'Unset' },
] as const

function tracesPath(filters: TraceFilters, offset: number): string {
  const params = new URLSearchParams()
  if (filters.service) params.set('service_name', filters.service)
  if (filters.status) params.set('status', filters.status)
  if (filters.q) params.set('search', filters.q)
  params.set('limit', String(PAGE_SIZE))
  params.set('offset', String(offset))
  return `/api/traces?${params.toString()}`
}

/**
 * /traces — virtualized trace table with URL-param filters and
 * cursor-style server paging; selecting a row opens the time-positioned
 * waterfall (master-detail ~55/45 on lg+, full-screen push below).
 */
export default function TracesView() {
  const urlSearch = useSearch()
  const [, navigate] = useLocation()
  const filters = useMemo(() => parseTraceFilters(urlSearch), [urlSearch])

  const setFilters = useCallback(
    (next: TraceFilters, { push = false } = {}) => {
      const qs = traceFiltersToSearch(next)
      navigate(qs ? `/traces?${qs}` : '/traces', { replace: !push })
    },
    [navigate],
  )

  // Search input is local state; the debounced value lands in the URL.
  const [searchText, setSearchText] = useState(filters.q ?? '')
  const debouncedSearch = useDebouncedValue(searchText.trim(), 300)
  useEffect(() => {
    if (debouncedSearch === (filters.q ?? '')) return
    setFilters({ ...filters, q: debouncedSearch || undefined })
    // eslint-disable-next-line react-hooks/exhaustive-deps -- only the debounced text drives this write-back
  }, [debouncedSearch])

  const servicesQuery = useQuery({
    queryKey: ['metadata', 'services'],
    queryFn: ({ signal }) =>
      apiFetch<string[]>('/api/metadata/services', { signal }),
  })
  const services = useMemo(() => {
    const list = servicesQuery.data ?? []
    // Keep a deep-linked service visible even before metadata loads.
    return filters.service && !list.includes(filters.service)
      ? [filters.service, ...list]
      : list
  }, [servicesQuery.data, filters.service])

  const listQuery = useInfiniteQuery({
    queryKey: ['traces', filters.service, filters.status, filters.q],
    queryFn: ({ pageParam, signal }) =>
      apiFetch<TracesResponse>(tracesPath(filters, pageParam), { signal }),
    initialPageParam: 0,
    getNextPageParam: (last, pages) => {
      const loaded = pages.reduce((n, p) => n + p.traces.length, 0)
      return loaded < last.total && last.traces.length > 0
        ? loaded
        : undefined
    },
  })
  const traces = useMemo(
    () => listQuery.data?.pages.flatMap((p) => p.traces) ?? [],
    [listQuery.data],
  )
  const p99Ms = useMemo(() => visibleP99Ms(traces), [traces])

  const touch = useMediaQuery('(max-width: 767px), (pointer: coarse)')
  const rowH = touch ? ROW_H_TOUCH : ROW_H
  const scrollRef = useRef<HTMLDivElement | null>(null)
  const virtualizer = useVirtualizer({
    count: traces.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => rowH,
    overscan: 8,
    // Sensible viewport before the first ResizeObserver tick (also what
    // jsdom tests render against — it never fires one).
    initialRect: { width: 800, height: 600 },
  })
  useEffect(() => {
    virtualizer.measure()
  }, [virtualizer, rowH])

  const selectTrace = useCallback(
    (traceId: string | undefined) => {
      setFilters({ ...filters, trace: traceId }, { push: true })
    },
    [filters, setFilters],
  )

  const hasFilters = Boolean(filters.service || filters.status || filters.q)

  return (
    <div className={styles.page}>
      <div className={styles.toolbar}>
        <select
          className={styles.select}
          aria-label="Filter by service"
          value={filters.service ?? ''}
          onChange={(e) =>
            setFilters({ ...filters, service: e.target.value || undefined })
          }
        >
          <option value="">All services</option>
          {services.map((s) => (
            <option key={s} value={s}>
              {s}
            </option>
          ))}
        </select>

        <select
          className={styles.select}
          aria-label="Filter by status"
          value={filters.status ?? ''}
          onChange={(e) =>
            setFilters({ ...filters, status: e.target.value || undefined })
          }
        >
          {STATUS_OPTIONS.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </select>

        <input
          type="search"
          className={styles.search}
          aria-label="Search"
          placeholder="Search by trace ID…"
          value={searchText}
          onChange={(e) => setSearchText(e.target.value)}
        />
      </div>

      <div
        className={`${styles.split} ${filters.trace ? styles.splitOpen : ''}`}
      >
        <div className={styles.master}>
          <div className={styles.theadRow} aria-hidden="true">
            <span className={styles.cellStatus}>Status</span>
            <span className={styles.cellService}>Service</span>
            <span className={styles.cellOp}>Operation</span>
            <span className={styles.cellSpans}>Spans</span>
            <span className={styles.cellDuration}>Duration</span>
          </div>

          {listQuery.isPending ? (
            <div className={styles.skeleton} aria-hidden="true">
              {Array.from({ length: 12 }, (_, i) => (
                <div key={i} className={styles.skeletonRow} />
              ))}
            </div>
          ) : listQuery.isError ? (
            <div className={styles.state} role="alert">
              <span className={styles.stateError}>GET /api/traces failed</span>
              <span className={styles.stateHint}>
                {(listQuery.error as Error).message}
              </span>
              <button
                type="button"
                className={styles.toolButton}
                onClick={() => listQuery.refetch()}
              >
                Retry
              </button>
            </div>
          ) : traces.length === 0 ? (
            <div className={styles.state}>
              <span>No traces found</span>
              {hasFilters ? (
                <button
                  type="button"
                  className={styles.toolButton}
                  onClick={() => {
                    setSearchText('')
                    setFilters({ trace: filters.trace })
                  }}
                >
                  Clear filters
                </button>
              ) : (
                <span className={styles.stateHint}>
                  Traces appear once an OTLP exporter sends spans to this
                  host (gRPC :4317 or HTTP /v1/traces) — see the Connect
                  popover in the top bar.
                </span>
              )}
            </div>
          ) : (
            <>
              <div
                ref={scrollRef}
                className={styles.scroll}
                role="list"
                aria-label="Traces"
              >
                <div
                  className={styles.inner}
                  style={{ height: virtualizer.getTotalSize() }}
                >
                  {virtualizer.getVirtualItems().map((item) => {
                    const t = traces[item.index]
                    const active = t.trace_id === filters.trace
                    const frac = durationBarFrac(t.duration_ms, p99Ms)
                    const isErr = statusTone(t.status) === 'crit'
                    return (
                      <div
                        key={item.key}
                        role="listitem"
                        className={styles.virtualRow}
                        style={{ transform: `translateY(${item.start}px)` }}
                      >
                        <button
                          type="button"
                          className={`${styles.row} ${active ? styles.rowActive : ''}`}
                          aria-pressed={active}
                          onClick={() => selectTrace(t.trace_id)}
                        >
                          <span className={styles.cellStatus}>
                            <ToneBadge tone={statusTone(t.status)}>
                              {statusLabel(t.status)}
                            </ToneBadge>
                          </span>
                          <span className={styles.cellService}>
                            {t.service_name}
                          </span>
                          <span className={styles.cellOp}>{t.operation}</span>
                          <span className={styles.cellSpans}>
                            {t.span_count}
                          </span>
                          <span className={styles.cellDuration}>
                            <span className={styles.durationText}>
                              {formatMs(t.duration_ms)}
                            </span>
                            <span
                              className={styles.durationTrack}
                              aria-hidden="true"
                            >
                              <span
                                className={`${styles.durationFill} ${isErr ? styles.durationFillErr : ''}`}
                                style={{ width: `${frac * 100}%` }}
                              />
                            </span>
                          </span>
                        </button>
                      </div>
                    )
                  })}
                </div>
              </div>
              {listQuery.hasNextPage && (
                <button
                  type="button"
                  className={`${styles.toolButton} ${styles.loadMore}`}
                  onClick={() => listQuery.fetchNextPage()}
                  disabled={listQuery.isFetchingNextPage}
                >
                  {listQuery.isFetchingNextPage
                    ? 'Loading…'
                    : `Load more (${traces.length} of ${listQuery.data?.pages[0]?.total ?? '?'})`}
                </button>
              )}
            </>
          )}
        </div>

        <div className={styles.detailPane}>
          {filters.trace && (
            <TraceDetail
              traceId={filters.trace}
              onClose={() => selectTrace(undefined)}
            />
          )}
        </div>
      </div>
    </div>
  )
}
