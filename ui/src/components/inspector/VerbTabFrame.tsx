import type { ReactNode } from 'react'
import type { TriageToolStatus } from '@/hooks/useTriageTool'
import styles from './ServiceInspector.module.css'

// Shared idle/loading/error scaffolding for the on-demand MCP verb tabs
// ("Why" / "Impact"): nothing runs until the operator asks, skeleton +
// cancel while the RPC is in flight, inline error with retry. Success
// rendering is the caller's children.

interface VerbTabFrameProps {
  status: TriageToolStatus
  error: string | null
  /** The verb, e.g. "Run root-cause analysis". */
  runLabel: string
  /** One idle-state sentence explaining what the verb does. */
  hint: string
  onRun: () => void
  onCancel: () => void
  children: ReactNode
}

function Skeleton() {
  return (
    <div aria-hidden="true" data-testid="verb-skeleton">
      <div className={`${styles.skeleton} ${styles.skeletonCause}`} />
      <div className={`${styles.skeleton} ${styles.skeletonCause}`} />
      <div className={`${styles.skeleton} ${styles.skeletonCause}`} />
    </div>
  )
}

export default function VerbTabFrame({
  status,
  error,
  runLabel,
  hint,
  onRun,
  onCancel,
  children,
}: Readonly<VerbTabFrameProps>) {
  if (status === 'idle') {
    return (
      <div className={styles.tabBody}>
        <p className={styles.quiet}>{hint}</p>
        <button type="button" className={styles.runButton} onClick={onRun}>
          {runLabel}
        </button>
      </div>
    )
  }

  if (status === 'loading') {
    return (
      <div className={styles.tabBody} aria-busy="true">
        <Skeleton />
        <button
          type="button"
          className={styles.stateAction}
          onClick={onCancel}
        >
          Cancel
        </button>
      </div>
    )
  }

  if (status === 'error') {
    return (
      <div className={styles.statePanel} role="alert">
        <p>{error ?? 'The analysis failed.'}</p>
        <button type="button" className={styles.stateAction} onClick={onRun}>
          Retry
        </button>
      </div>
    )
  }

  return <div className={styles.tabBody}>{children}</div>
}
