import { type ReactNode, useEffect } from 'react'
import { getWsManager } from '@/lib/wsManager'
import type { Theme } from '@/hooks/useTheme'
import PulseBar from './PulseBar'
import styles from './Shell.module.css'

interface ShellProps {
  theme: Theme
  onToggleTheme: () => void
  /** Opens the ⌘K palette — wired to the pulse-bar button. */
  onOpenPalette?: () => void
  children: ReactNode
}

/**
 * App shell: the System Pulse bar on top, the single-page canvas below. There
 * is no navigation rail — the Service Map IS the app (the flow map folded into
 * the home; logs/traces are MCP-tool surfaces; everything else is the ⌘K
 * palette), so the canvas fills the full width.
 */
export default function Shell({
  theme,
  onToggleTheme,
  onOpenPalette,
  children,
}: Readonly<ShellProps>) {
  // The /ws singleton lives for the app lifetime — started once here,
  // never stopped (start() is idempotent under StrictMode remounts).
  useEffect(() => {
    getWsManager().start()
  }, [])

  // The home route is the Constellation canvas — it owns its own Health Core,
  // so the Shell no longer hangs a vitals hero on `/`. Every route keeps the
  // always-on slim strip (PulseBar) at the top.

  return (
    <div className={`${styles.shell} graticule`}>
      <PulseBar
        theme={theme}
        onToggleTheme={onToggleTheme}
        onOpenPalette={onOpenPalette}
      />
      <main className={styles.main}>{children}</main>
    </div>
  )
}
