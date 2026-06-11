import { useEffect } from 'react'
import { ChevronRight } from 'lucide-react'
import type { TrailFrame } from '@/lib/trail'
import styles from './TrailBar.module.css'

interface TrailBarProps {
  frames: readonly TrailFrame[]
  /** Chip tap — pop back to that frame. */
  onPopTo: (index: number) => void
  /** Backspace — pop the top frame. */
  onPopOne: () => void
}

/** True when the key event originates from an editable element. */
function isEditableTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false
  return (
    target.isContentEditable ||
    target.tagName === 'INPUT' ||
    target.tagName === 'TEXTAREA' ||
    target.tagName === 'SELECT'
  )
}

/** Shorten long ids (trace ids) for chip display. */
function chipLabel(frame: TrailFrame): string {
  if (frame.id.length <= 16) return frame.id
  return `${frame.id.slice(0, 8)}…`
}

/**
 * Investigation Trail — breadcrumb chips pinned bottom-left (above the xs
 * tab bar). Renders nothing at depth 0. Backspace pops globally unless an
 * editable element has focus.
 */
export default function TrailBar({
  frames,
  onPopTo,
  onPopOne,
}: Readonly<TrailBarProps>) {
  const depth = frames.length

  useEffect(() => {
    if (depth === 0) return
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key !== 'Backspace' || isEditableTarget(event.target)) return
      event.preventDefault()
      onPopOne()
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [depth, onPopOne])

  if (depth === 0) return null

  return (
    <nav className={styles.bar} aria-label="Investigation trail">
      <ol className={styles.list}>
        {frames.map((frame, i) => {
          const current = i === depth - 1
          return (
            <li key={`${i}-${frame.kind}-${frame.id}`} className={styles.item}>
              {i > 0 && (
                <ChevronRight size={11} className={styles.sep} aria-hidden="true" />
              )}
              <button
                type="button"
                className={`${styles.chip} ${current ? styles.chipCurrent : ''}`}
                aria-current={current ? 'true' : undefined}
                title={frame.id}
                onClick={() => onPopTo(i)}
              >
                {frame.kind === 'trace' && (
                  <span className={styles.kind}>trace</span>
                )}
                <span className={styles.id}>{chipLabel(frame)}</span>
              </button>
            </li>
          )
        })}
      </ol>
    </nav>
  )
}
