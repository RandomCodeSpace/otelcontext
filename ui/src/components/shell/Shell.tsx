import { useEffect, type ComponentType, type ReactNode } from 'react'
import * as Tooltip from '@radix-ui/react-tooltip'
import { Link, useRoute } from 'wouter'
import { LayoutDashboard, Network, Terminal } from 'lucide-react'
import { getWsManager } from '@/lib/wsManager'
import type { Theme } from '@/hooks/useTheme'
import PulseBar from './PulseBar'
import styles from './Shell.module.css'

interface NavEntry {
  href: string
  label: string
  Icon: ComponentType<{ size?: number | string; 'aria-hidden'?: boolean }>
}

const NAV_ITEMS: readonly NavEntry[] = [
  { href: '/map', label: 'Service Map', Icon: Network },
  { href: '/dashboard', label: 'Dashboard', Icon: LayoutDashboard },
  { href: '/mcp', label: 'MCP Console', Icon: Terminal },
]

function NavLink({
  entry,
  variant,
}: Readonly<{ entry: NavEntry; variant: 'rail' | 'tab' }>) {
  const [active] = useRoute(entry.href)
  const { href, label, Icon } = entry

  const link = (
    <Link
      href={href}
      className={`${styles.navItem} ${active ? styles.navItemActive : ''}`}
      aria-current={active ? 'page' : undefined}
      aria-label={label}
    >
      <Icon size={18} aria-hidden />
      <span className={styles.navLabel}>{label}</span>
    </Link>
  )

  // Rail icons lose their label below xl — tooltip carries it instead.
  if (variant === 'rail') {
    return (
      <Tooltip.Root>
        <Tooltip.Trigger asChild>{link}</Tooltip.Trigger>
        <Tooltip.Portal>
          <Tooltip.Content className={styles.tooltip} side="right" sideOffset={6}>
            {label}
          </Tooltip.Content>
        </Tooltip.Portal>
      </Tooltip.Root>
    )
  }
  return link
}

interface ShellProps {
  theme: Theme
  onToggleTheme: () => void
  children: ReactNode
}

/**
 * Responsive app shell: System Pulse bar on top; navigation as a bottom
 * tab bar below 768px, a 56px icon rail from 768px, and a labeled 200px
 * rail from 1440px — all CSS-only breakpoints (see styles/tokens.css).
 * Hidden variants use display:none, so only one nav is in the a11y tree.
 */
export default function Shell({
  theme,
  onToggleTheme,
  children,
}: Readonly<ShellProps>) {
  // The /ws singleton lives for the app lifetime — started once here,
  // never stopped (start() is idempotent under StrictMode remounts).
  useEffect(() => {
    getWsManager().start()
  }, [])

  return (
    <Tooltip.Provider delayDuration={300}>
      <div className={styles.shell}>
        <PulseBar theme={theme} onToggleTheme={onToggleTheme} />
        <div className={styles.body}>
          <nav className={styles.rail} aria-label="Primary">
            {NAV_ITEMS.map((entry) => (
              <NavLink key={entry.href} entry={entry} variant="rail" />
            ))}
          </nav>
          <main className={styles.main}>{children}</main>
        </div>
        <nav className={styles.tabbar} aria-label="Primary">
          {NAV_ITEMS.map((entry) => (
            <NavLink key={entry.href} entry={entry} variant="tab" />
          ))}
        </nav>
      </div>
    </Tooltip.Provider>
  )
}
