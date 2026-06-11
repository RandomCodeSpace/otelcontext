import type { ReactNode } from 'react'
import styles from './ToneBadge.module.css'

export type BadgeTone = 'ok' | 'warn' | 'crit' | 'unknown'

/**
 * Status badge: full-strength --{tone} text on the translucent --{tone}-bg
 * fill (≥4.5:1 in both themes — the audit's subtle-badge fix). Saturation
 * is reserved for status, so this is the only colored chip in the tables.
 */
export default function ToneBadge({
  tone,
  children,
}: Readonly<{ tone: BadgeTone; children: ReactNode }>) {
  return <span className={`${styles.badge} ${styles[tone]}`}>{children}</span>
}
