import { useEffect, useRef, useState } from 'react'
import { Check, Copy } from 'lucide-react'
import { endpoints } from './connectEndpoints'
import styles from './ConnectInline.module.css'

/**
 * The Connect popover's content, inlined — used by empty states so "what do
 * I do next" is answered in place: point an OTLP exporter or MCP agent here.
 */
export default function ConnectInline() {
  const [copied, setCopied] = useState<string | null>(null)
  const resetTimer = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(
    () => () => {
      if (resetTimer.current !== null) clearTimeout(resetTimer.current)
    },
    [],
  )

  const copy = async (key: string, value: string) => {
    try {
      await navigator.clipboard.writeText(value)
      setCopied(key)
      if (resetTimer.current !== null) clearTimeout(resetTimer.current)
      resetTimer.current = setTimeout(() => setCopied(null), 1500)
    } catch {
      /* clipboard unavailable — the value stays visible to select */
    }
  }

  return (
    <div className={styles.connect}>
      <p className={styles.lead}>
        No telemetry yet. Point an OTLP exporter (or an MCP agent) at this
        host:
      </p>
      <ul className={styles.list}>
        {endpoints().map((ep) => (
          <li key={ep.key} className={styles.item}>
            <span className={styles.label}>{ep.label}</span>
            <code className={styles.value}>{ep.value}</code>
            <button
              type="button"
              className={styles.copy}
              aria-label={`Copy ${ep.label}`}
              onClick={() => void copy(ep.key, ep.value)}
            >
              {copied === ep.key ? (
                <Check size={13} aria-hidden="true" />
              ) : (
                <Copy size={13} aria-hidden="true" />
              )}
            </button>
          </li>
        ))}
      </ul>
    </div>
  )
}
