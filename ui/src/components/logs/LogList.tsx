import { useCallback, useEffect, useRef, useState } from 'react'
import { useVirtualizer } from '@tanstack/react-virtual'
import ToneBadge from '@/components/common/ToneBadge'
import { useMediaQuery } from '@/hooks/useMediaQuery'
import {
  countNewSince,
  formatLogTime,
  normalizeSeverity,
  severityTone,
} from '@/lib/logRows'
import type { LogEntry } from '@/types/api'
import LogDetail from './LogDetail'
import styles from './LogsView.module.css'

/** Fixed row heights — no per-row measurement on the scroll hot path. */
const ROW_H = 28
const ROW_H_TOUCH = 44
const DETAIL_H = 252
/** How close to the bottom (in rows) still counts as "following". */
const FOLLOW_SLACK_ROWS = 2

// Row severity → xs left-border class (the collapsed gutter).
const SEV_BORDER: Record<string, string> = {
  ERROR: styles.sevError,
  WARN: styles.sevWarn,
  INFO: styles.sevInfo,
  DEBUG: styles.sevDebug,
}

interface LogListProps {
  logs: readonly LogEntry[]
  /** live = auto-follow + "N new" pill; search/context are static lists. */
  live: boolean
  /** WsManager's monotonic appended total (live mode only). */
  appendedTotal?: number
  /** Row id to tint (the context-mode anchor). */
  highlightId?: number
  onShowContext: (log: LogEntry) => void
}

/**
 * Virtualized log list (fixed row heights, overscan 8). In live mode the
 * list pins to the bottom; scrolling up pauses follow and shows a
 * "N new" pill that jumps back down — it never yanks the scroll position.
 * A row click expands the entry in place (the expanded index is the only
 * one with a non-default size — still no DOM measurement).
 */
export default function LogList({
  logs,
  live,
  appendedTotal = 0,
  highlightId,
  onShowContext,
}: Readonly<LogListProps>) {
  const touch = useMediaQuery('(max-width: 767px), (pointer: coarse)')
  const rowH = touch ? ROW_H_TOUCH : ROW_H

  const scrollRef = useRef<HTMLDivElement | null>(null)
  const [expandedId, setExpandedId] = useState<number | null>(null)
  const [following, setFollowing] = useState(true)
  // appendedTotal reading taken when follow paused — pill = delta since.
  const [pillBase, setPillBase] = useState(0)

  const expandedIndex =
    expandedId === null ? -1 : logs.findIndex((l) => l.id === expandedId)

  const virtualizer = useVirtualizer({
    count: logs.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: (i) => (i === expandedIndex ? rowH + DETAIL_H : rowH),
    overscan: 8,
    // Sensible viewport before the first ResizeObserver tick (also what
    // jsdom tests render against — it never fires one).
    initialRect: { width: 800, height: 600 },
  })

  // Size function changed (expansion/row height) — remeasure indices.
  useEffect(() => {
    virtualizer.measure()
  }, [virtualizer, expandedIndex, rowH])

  // Pin to bottom on new data while following (live mode only).
  useEffect(() => {
    if (!live || !following || logs.length === 0) return
    virtualizer.scrollToIndex(logs.length - 1, { align: 'end' })
  }, [live, following, logs, virtualizer])

  const handleScroll = useCallback(() => {
    if (!live) return
    const el = scrollRef.current
    if (!el) return
    const distFromBottom = el.scrollHeight - el.scrollTop - el.clientHeight
    const nearBottom = distFromBottom <= rowH * FOLLOW_SLACK_ROWS
    if (following && !nearBottom) {
      // User scrolled up: stop following, remember where the count starts.
      setPillBase(appendedTotal)
      setFollowing(false)
    } else if (!following && nearBottom) {
      setFollowing(true)
    }
  }, [live, rowH, appendedTotal, following])

  const jumpToLive = useCallback(() => {
    const el = scrollRef.current
    if (el) el.scrollTop = el.scrollHeight
    setFollowing(true)
  }, [])

  const toggleExpand = useCallback(
    (id: number) => {
      setExpandedId((prev) => (prev === id ? null : id))
      // Reading an entry means the user left the live edge on purpose.
      if (live) setFollowing(false)
    },
    [live],
  )

  const newCount = countNewSince(appendedTotal, pillBase)
  const showPill = live && !following && newCount > 0

  return (
    <>
      <div
        ref={scrollRef}
        className={styles.scroll}
        onScroll={handleScroll}
        role="list"
        aria-label="Log entries"
      >
        <div
          className={styles.inner}
          style={{ height: virtualizer.getTotalSize() }}
        >
          {virtualizer.getVirtualItems().map((item) => {
            const log = logs[item.index]
            const expanded = log.id === expandedId
            const sev = normalizeSeverity(log.severity)
            return (
              <div
                key={item.key}
                role="listitem"
                className={styles.virtualRow}
                style={{ transform: `translateY(${item.start}px)` }}
              >
                <button
                  type="button"
                  className={[
                    styles.row,
                    SEV_BORDER[sev],
                    expanded ? styles.rowExpanded : '',
                    log.id === highlightId ? styles.rowHighlight : '',
                  ]
                    .filter(Boolean)
                    .join(' ')}
                  aria-expanded={expanded}
                  onClick={() => toggleExpand(log.id)}
                >
                  <span className={styles.time}>
                    {formatLogTime(log.timestamp)}
                  </span>
                  <span className={styles.sevCell}>
                    <ToneBadge tone={severityTone(log.severity)}>
                      {sev}
                    </ToneBadge>
                  </span>
                  <span className={styles.service}>{log.service_name}</span>
                  <span className={styles.body}>{log.body}</span>
                </button>
                {expanded && (
                  <LogDetail log={log} onShowContext={onShowContext} />
                )}
              </div>
            )
          })}
        </div>
      </div>
      {showPill && (
        <button type="button" className={styles.newPill} onClick={jumpToLive}>
          ↓ {newCount} new
        </button>
      )}
    </>
  )
}
