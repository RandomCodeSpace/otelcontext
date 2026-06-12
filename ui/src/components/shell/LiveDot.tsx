import { useSyncExternalStore } from 'react'
import { getWsManager, type WsManager } from '@/lib/wsManager'
import styles from './LiveDot.module.css'

interface LiveDotProps {
  /** Injectable for tests; defaults to the app singleton. */
  manager?: WsManager
}

/**
 * Three-state live indicator driven by the wsManager status store:
 * ok (connected) / warn pulsing (connecting/reconnecting, attempt count
 * in the accessible name) / crit (offline).
 */
export default function LiveDot({ manager }: Readonly<LiveDotProps>) {
  const ws = manager ?? getWsManager()
  const snap = useSyncExternalStore(ws.subscribeStatus, ws.getStatusSnapshot)

  let tone: 'ok' | 'warn' | 'crit'
  let label: string
  switch (snap.status) {
    case 'connected':
      tone = 'ok'
      label = 'live'
      break
    case 'connecting':
      tone = 'warn'
      label = 'connecting'
      break
    case 'reconnecting':
      tone = 'warn'
      label = `reconnecting (attempt ${snap.attempt})`
      break
    default:
      tone = 'crit'
      label = 'offline'
  }

  return (
    <span
      role="status"
      aria-label={label}
      title={label}
      className={`${styles.dot} ${styles[tone]}`}
    />
  )
}
