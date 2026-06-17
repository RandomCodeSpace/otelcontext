import { useCallback, useState } from 'react'
import * as Dialog from '@radix-ui/react-dialog'
import { Command } from 'cmdk'
import { Copy, Orbit, Radar, SunMoon, Waypoints } from 'lucide-react'
import { useLocation } from 'wouter'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useInvestigation } from '@/hooks/useInvestigation'
import { apiFetch } from '@/lib/apiFetch'
import {
  PALETTE_ACTIONS,
  ROOT_PAGE,
  escapeBehavior,
  pagePlaceholder,
  pickAction,
  serviceCommand,
  type PaletteActionId,
  type PalettePage,
} from '@/lib/palette'
import { impactQueryOptions, rootCauseQueryOptions } from '@/lib/triageVerbs'
import { endpoints } from '@/components/shell/connectEndpoints'
import styles from './palette.module.css'

// ⌘K command palette (cmdk inside our own Radix dialog so Escape can back
// out of the service picker instead of closing). Sections: Navigate /
// Actions (the MCP triage verbs, two-step with a service picker) /
// Services / Utilities. Lazy-loaded via PaletteHost — costs nothing until
// the first open.

const NAV_COMMANDS = [
  { href: '/', label: 'Service Map', Icon: Orbit },
] as const

const ACTION_ICONS: Record<PaletteActionId, typeof Radar> = {
  'root-cause': Radar,
  impact: Waypoints,
}

interface CommandPaletteProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  onToggleTheme: () => void
}

export default function CommandPalette({
  open,
  onOpenChange,
  onToggleTheme,
}: Readonly<CommandPaletteProps>) {
  const [page, setPage] = useState<PalettePage>(ROOT_PAGE)
  const [search, setSearch] = useState('')
  const [, navigate] = useLocation()
  const { openService } = useInvestigation()
  const queryClient = useQueryClient()

  const servicesQuery = useQuery({
    queryKey: ['metadata', 'services'],
    queryFn: ({ signal }) =>
      apiFetch<string[]>('/api/metadata/services', { signal }),
    enabled: open,
  })
  const services = servicesQuery.data ?? []

  const backToRoot = useCallback(() => {
    setPage(ROOT_PAGE)
    setSearch('')
  }, [])

  const handleOpenChange = useCallback(
    (next: boolean) => {
      if (!next) {
        setPage(ROOT_PAGE)
        setSearch('')
      }
      onOpenChange(next)
    },
    [onOpenChange],
  )

  const close = useCallback(() => handleOpenChange(false), [handleOpenChange])

  const goTo = useCallback(
    (href: string) => {
      navigate(href)
      close()
    },
    [navigate, close],
  )

  const runOnService = useCallback(
    (action: PaletteActionId, service: string) => {
      const cmd = serviceCommand(action, service)
      // Start the RPC now — the inspector tab observes the in-flight
      // prefetch through the shared query key (lib/triageVerbs).
      if (cmd.prefetch === 'root_cause_analysis') {
        void queryClient.prefetchQuery(rootCauseQueryOptions(cmd.service))
      } else {
        void queryClient.prefetchQuery(impactQueryOptions(cmd.service))
      }
      openService(cmd.service, cmd.tab)
      close()
    },
    [queryClient, openService, close],
  )

  const copyMcpUrl = useCallback(() => {
    const mcp = endpoints().find((e) => e.key === 'mcp')
    if (mcp) {
      navigator.clipboard?.writeText(mcp.value).catch(() => {
        /* clipboard unavailable (http origin / permissions) */
      })
    }
    close()
  }, [close])

  return (
    <Dialog.Root open={open} onOpenChange={handleOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className={styles.overlay} />
        <Dialog.Content
          className={styles.content}
          aria-describedby={undefined}
          onEscapeKeyDown={(event) => {
            if (escapeBehavior(page) === 'back') {
              event.preventDefault()
              backToRoot()
            }
          }}
        >
          <Dialog.Title className={styles.srOnly}>Command palette</Dialog.Title>
          <Command label="Command palette" className={styles.command}>
            <Command.Input
              className={styles.input}
              placeholder={pagePlaceholder(page)}
              value={search}
              onValueChange={setSearch}
              autoFocus
            />
            <Command.List className={styles.list}>
              <Command.Empty className={styles.empty}>
                No matches.
              </Command.Empty>

              {page.id === 'root' && (
                <>
                  <Command.Group heading="Navigate" className={styles.group}>
                    {NAV_COMMANDS.map(({ href, label, Icon }) => (
                      <Command.Item
                        key={href}
                        className={styles.item}
                        value={`go ${label}`}
                        onSelect={() => goTo(href)}
                      >
                        <Icon size={14} aria-hidden="true" />
                        {label}
                      </Command.Item>
                    ))}
                  </Command.Group>

                  <Command.Group heading="Actions" className={styles.group}>
                    {PALETTE_ACTIONS.map(({ id, label }) => {
                      const Icon = ACTION_ICONS[id]
                      return (
                        <Command.Item
                          key={id}
                          className={styles.item}
                          value={`action ${label}`}
                          onSelect={() => {
                            setPage(pickAction(id))
                            setSearch('')
                          }}
                        >
                          <Icon size={14} aria-hidden="true" />
                          {label}
                        </Command.Item>
                      )
                    })}
                  </Command.Group>

                  <Command.Group heading="Services" className={styles.group}>
                    {servicesQuery.isPending && (
                      <Command.Loading className={styles.loading}>
                        Loading services…
                      </Command.Loading>
                    )}
                    {services.map((service) => (
                      <Command.Item
                        key={service}
                        className={`${styles.item} ${styles.itemMono}`}
                        value={service}
                        onSelect={() => {
                          openService(service)
                          close()
                        }}
                      >
                        {service}
                      </Command.Item>
                    ))}
                  </Command.Group>

                  <Command.Group heading="Utilities" className={styles.group}>
                    <Command.Item
                      className={styles.item}
                      value="utility toggle theme"
                      onSelect={() => {
                        onToggleTheme()
                        close()
                      }}
                    >
                      <SunMoon size={14} aria-hidden="true" />
                      Toggle theme
                    </Command.Item>
                    <Command.Item
                      className={styles.item}
                      value="utility copy mcp url"
                      onSelect={copyMcpUrl}
                    >
                      <Copy size={14} aria-hidden="true" />
                      Copy MCP URL
                    </Command.Item>
                  </Command.Group>
                </>
              )}

              {page.id === 'service-pick' && (
                <Command.Group
                  heading={
                    PALETTE_ACTIONS.find((a) => a.id === page.action)?.label
                  }
                  className={styles.group}
                >
                  {servicesQuery.isPending && (
                    <Command.Loading className={styles.loading}>
                      Loading services…
                    </Command.Loading>
                  )}
                  {services.map((service) => (
                    <Command.Item
                      key={service}
                      className={`${styles.item} ${styles.itemMono}`}
                      value={service}
                      onSelect={() => runOnService(page.action, service)}
                    >
                      {service}
                    </Command.Item>
                  ))}
                </Command.Group>
              )}
            </Command.List>
            <div className={styles.footer} aria-hidden="true">
              <span>
                <kbd className={styles.kbd}>↑↓</kbd> select
              </span>
              <span>
                <kbd className={styles.kbd}>↵</kbd> run
              </span>
              <span>
                <kbd className={styles.kbd}>esc</kbd>{' '}
                {page.id === 'service-pick' ? 'back' : 'close'}
              </span>
            </div>
          </Command>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  )
}
