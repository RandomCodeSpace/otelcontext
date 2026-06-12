import { Fragment, useMemo } from 'react'
import { Link } from 'wouter'
import { parseAttributes } from '@/lib/logRows'
import type { LogEntry } from '@/types/api'
import styles from './LogsView.module.css'

interface LogDetailProps {
  log: LogEntry
  onShowContext: (log: LogEntry) => void
}

/**
 * Expanded in-place row detail: full body, attributes (parsed lazily —
 * only when this component mounts, never on the tail's render hot path),
 * the stored AI insight when present, surrounding-context jump and the
 * trace cross-link.
 */
export default function LogDetail({
  log,
  onShowContext,
}: Readonly<LogDetailProps>) {
  const attributes = useMemo(
    () => parseAttributes(log.attributes_json),
    [log.attributes_json],
  )

  return (
    <div className={styles.detail}>
      <pre className={styles.detailBody}>{log.body}</pre>

      {attributes && (
        <>
          <p className={styles.detailLabel}>Attributes</p>
          <div className={styles.attrTable}>
            {attributes.map(([key, value], i) => (
              <Fragment key={`${i}-${key}`}>
                <span className={styles.attrKey}>{key}</span>
                <span className={styles.attrVal}>{value}</span>
              </Fragment>
            ))}
          </div>
        </>
      )}

      {log.ai_insight && (
        <>
          <p className={styles.detailLabel}>AI insight</p>
          <div className={styles.insight}>{log.ai_insight}</div>
        </>
      )}

      <div className={styles.detailActions}>
        <button
          type="button"
          className={styles.toolButton}
          onClick={() => onShowContext(log)}
        >
          Show context
        </button>
        {log.trace_id && (
          <Link
            href={`/traces?trace=${encodeURIComponent(log.trace_id)}`}
            className={styles.toolButton}
          >
            Open trace
          </Link>
        )}
      </div>
    </div>
  )
}
