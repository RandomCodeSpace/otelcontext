import { Fragment, useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link } from 'wouter'
import ToneBadge from '@/components/common/ToneBadge'
import { apiFetch } from '@/lib/apiFetch'
import { formatMs } from '@/lib/format'
import { formatLogTime, parseAttributes, severityTone } from '@/lib/logRows'
import { statusLabel, statusTone } from '@/lib/traceRows'
import { buildWaterfall } from '@/lib/waterfall'
import type { Span, Trace } from '@/types/api'
import Waterfall from './Waterfall'
import styles from './TracesView.module.css'

interface TraceDetailProps {
  traceId: string
  /** Clears the ?trace= param (back affordance below lg). */
  onClose: () => void
}

function SpanSection({ span, trace }: Readonly<{ span: Span; trace: Trace }>) {
  // Attributes parse lazily — only for the one selected span.
  const attributes = useMemo(
    () => parseAttributes(span.attributes_json),
    [span.attributes_json],
  )
  const spanLogs = useMemo(
    () => (trace.logs ?? []).filter((l) => l.span_id === span.span_id),
    [trace.logs, span.span_id],
  )

  return (
    <>
      <div className={styles.section}>
        <p className={styles.sectionLabel}>
          Span · {span.operation_name} · {span.service_name} ·{' '}
          {formatMs(span.duration / 1000)}
        </p>
        {attributes ? (
          <div className={styles.attrTable}>
            {attributes.map(([key, value], i) => (
              <Fragment key={`${i}-${key}`}>
                <span className={styles.attrKey}>{key}</span>
                <span className={styles.attrVal}>{value}</span>
              </Fragment>
            ))}
          </div>
        ) : (
          <span className={styles.stateHint}>No attributes recorded.</span>
        )}
      </div>
      <div className={styles.section}>
        <p className={styles.sectionLabel}>
          Correlated logs ({spanLogs.length})
        </p>
        {spanLogs.length === 0 ? (
          <span className={styles.stateHint}>
            No logs reference this span.
          </span>
        ) : (
          spanLogs.map((l) => (
            <div key={l.id} className={styles.miniLog}>
              <ToneBadge tone={severityTone(l.severity)}>
                {l.severity}
              </ToneBadge>
              <span>{formatLogTime(l.timestamp)}</span>
              <span className={styles.miniLogBody}>{l.body}</span>
            </div>
          ))
        )}
      </div>
    </>
  )
}

/**
 * Right pane / full-screen detail: fetches the full trace (spans + logs)
 * and renders the time-positioned waterfall. Clicking a span reveals its
 * lazily-parsed attributes and the trace's correlated logs.
 */
export default function TraceDetail({
  traceId,
  onClose,
}: Readonly<TraceDetailProps>) {
  const [selectedSpanId, setSelectedSpanId] = useState<string | null>(null)

  const query = useQuery({
    queryKey: ['trace', traceId],
    queryFn: ({ signal }) =>
      apiFetch<Trace>(`/api/traces/${encodeURIComponent(traceId)}`, {
        signal,
      }),
  })

  const layout = useMemo(
    () => buildWaterfall(query.data?.spans ?? []),
    [query.data],
  )
  const selectedSpan =
    layout.rows.find((r) => r.span.span_id === selectedSpanId)?.span ?? null

  if (query.isPending) {
    return (
      <div className={styles.skeleton} aria-hidden="true">
        {Array.from({ length: 10 }, (_, i) => (
          <div key={i} className={styles.skeletonRow} />
        ))}
      </div>
    )
  }

  if (query.isError) {
    return (
      <div className={styles.state} role="alert">
        <span className={styles.stateError}>
          GET /api/traces/{traceId.slice(0, 12)}… failed
        </span>
        <span className={styles.stateHint}>
          {(query.error as Error).message}
        </span>
        <button
          type="button"
          className={styles.toolButton}
          onClick={() => query.refetch()}
        >
          Retry
        </button>
        <button type="button" className={styles.toolButton} onClick={onClose}>
          Close
        </button>
      </div>
    )
  }

  const trace = query.data

  return (
    <div>
      <div className={styles.detailHeader}>
        <button
          type="button"
          className={`${styles.toolButton} ${styles.backButton}`}
          onClick={onClose}
        >
          ← Back
        </button>
        <ToneBadge tone={statusTone(trace.status)}>
          {statusLabel(trace.status)}
        </ToneBadge>
        <span className={styles.detailTitle}>
          {trace.operation || trace.trace_id}
        </span>
        <span className={styles.detailMeta}>
          {trace.span_count} spans · {formatMs(trace.duration_ms)}
        </span>
        <Link
          href={`/logs?trace=${encodeURIComponent(trace.trace_id)}`}
          className={styles.toolButton}
        >
          Open in Logs
        </Link>
      </div>

      {layout.rows.length === 0 ? (
        <div className={styles.state}>
          <span>No spans stored for this trace</span>
          <span className={styles.stateHint}>
            Spans may have been sampled out (SAMPLING_RATE) or purged by
            retention.
          </span>
        </div>
      ) : (
        <Waterfall
          layout={layout}
          selectedSpanId={selectedSpanId}
          onSelect={(span) =>
            setSelectedSpanId((cur) =>
              cur === span.span_id ? null : span.span_id,
            )
          }
        />
      )}

      {selectedSpan && <SpanSection span={selectedSpan} trace={trace} />}
    </div>
  )
}
