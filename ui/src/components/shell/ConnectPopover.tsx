import { useEffect, useRef, useState } from 'react'
import * as DropdownMenu from '@radix-ui/react-dropdown-menu'
import { Cable, Check, Copy } from 'lucide-react'
import styles from './ConnectPopover.module.css'

interface Endpoint {
  key: string
  label: string
  value: string
}

// Derived from the page origin, so the copied values work wherever the
// operator is browsing from (recomposes what TopNav/SettingsDrawer never
// surfaced: the actual connect strings for agents and OTLP exporters).
function endpoints(): Endpoint[] {
  const { origin, hostname } = window.location
  return [
    { key: 'mcp', label: 'MCP URL', value: `${origin}/mcp` },
    { key: 'grpc', label: 'OTLP gRPC', value: `${hostname}:4317` },
    { key: 'http', label: 'OTLP HTTP', value: `${origin}/v1/` },
  ]
}

/** Pulse-bar popover with copyable MCP/OTLP endpoints. */
export default function ConnectPopover() {
  const [copied, setCopied] = useState<string | null>(null)
  const resetTimer = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(
    () => () => {
      if (resetTimer.current !== null) clearTimeout(resetTimer.current)
    },
    [],
  )

  const copy = async (ep: Endpoint) => {
    try {
      await navigator.clipboard.writeText(ep.value)
      setCopied(ep.key)
      if (resetTimer.current !== null) clearTimeout(resetTimer.current)
      resetTimer.current = setTimeout(() => setCopied(null), 1500)
    } catch {
      /* clipboard unavailable (http origin / permissions) — value stays visible to select */
    }
  }

  return (
    <DropdownMenu.Root>
      <DropdownMenu.Trigger asChild>
        <button type="button" className={styles.trigger}>
          <Cable size={15} aria-hidden="true" />
          <span className={styles.triggerLabel}>Connect</span>
        </button>
      </DropdownMenu.Trigger>
      <DropdownMenu.Portal>
        <DropdownMenu.Content
          className={styles.content}
          align="end"
          sideOffset={8}
        >
          {endpoints().map((ep) => (
            <DropdownMenu.Item
              key={ep.key}
              className={styles.item}
              // preventDefault keeps the menu open so several values can be
              // copied in one visit.
              onSelect={(event) => {
                event.preventDefault()
                void copy(ep)
              }}
            >
              <span className={styles.itemLabel}>{ep.label}</span>
              <code className={styles.itemValue}>{ep.value}</code>
              {copied === ep.key ? (
                <Check size={13} className={styles.copiedIcon} aria-hidden="true" />
              ) : (
                <Copy size={13} className={styles.copyIcon} aria-hidden="true" />
              )}
            </DropdownMenu.Item>
          ))}
        </DropdownMenu.Content>
      </DropdownMenu.Portal>
    </DropdownMenu.Root>
  )
}
