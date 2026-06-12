import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { Router } from 'wouter'
import { memoryLocation } from 'wouter/memory-location'
import type { ReactNode } from 'react'
import Shell from '../Shell'

class StubWebSocket {
  static readonly CONNECTING = 0
  static readonly OPEN = 1
  static readonly CLOSING = 2
  static readonly CLOSED = 3
  readyState = StubWebSocket.CONNECTING
  onopen: ((ev: Event) => void) | null = null
  onmessage: ((ev: MessageEvent<string>) => void) | null = null
  onerror: ((ev: Event) => void) | null = null
  onclose: ((ev: CloseEvent) => void) | null = null
  send = vi.fn()
  close = vi.fn()
  constructor(public url: string) {}
}

const fetchMock = vi.fn<typeof fetch>(() =>
  Promise.resolve(new Response('{}', { status: 200 })),
)

function renderShell(path = '/map', onToggleTheme = vi.fn()) {
  const { hook } = memoryLocation({ path })
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={qc}>
      <Router hook={hook}>{children}</Router>
    </QueryClientProvider>
  )
  const utils = render(
    <Shell theme="dark" onToggleTheme={onToggleTheme}>
      <div data-testid="page-content">page</div>
    </Shell>,
    { wrapper },
  )
  return { ...utils, onToggleTheme }
}

beforeEach(() => {
  vi.stubGlobal('WebSocket', StubWebSocket as unknown as typeof WebSocket)
  vi.stubGlobal('fetch', fetchMock)
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('Shell', () => {
  it('renders the pulse banner, navigation and main content', () => {
    renderShell()
    expect(screen.getByRole('banner')).toBeInTheDocument()
    expect(screen.getAllByRole('navigation').length).toBeGreaterThanOrEqual(2)
    expect(screen.getByRole('main')).toContainElement(
      screen.getByTestId('page-content'),
    )
  })

  it('exposes both nav variants (rail + bottom tabs) with exactly 4 destinations', () => {
    renderShell()
    // Each destination appears twice: once in the rail, once in the tab bar.
    for (const name of [/triage/i, /flow map/i, /traces/i, /logs/i]) {
      expect(screen.getAllByRole('link', { name })).toHaveLength(2)
    }
    // The dashboard and MCP-console destinations are retired.
    expect(screen.getAllByRole('link')).toHaveLength(8)
  })

  it('marks the active route with aria-current', () => {
    renderShell('/traces')
    const active = screen
      .getAllByRole('link')
      .filter((a) => a.getAttribute('aria-current') === 'page')
    expect(active).toHaveLength(2) // rail + tab bar
    active.forEach((a) => expect(a).toHaveAttribute('href', '/traces'))
  })

  it('renders the palette buttons when onOpenPalette is wired', async () => {
    const user = userEvent.setup()
    const onOpenPalette = vi.fn()
    const { hook } = memoryLocation({ path: '/map' })
    const qc = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    render(
      <QueryClientProvider client={qc}>
        <Router hook={hook}>
          <Shell
            theme="dark"
            onToggleTheme={() => {}}
            onOpenPalette={onOpenPalette}
          >
            <div />
          </Shell>
        </Router>
      </QueryClientProvider>,
    )
    const buttons = screen.getAllByRole('button', {
      name: /open command palette/i,
    })
    expect(buttons).toHaveLength(2) // pulse bar (md+) + xs tab-bar center
    await user.click(buttons[0])
    expect(onOpenPalette).toHaveBeenCalledTimes(1)
  })

  it('wires the theme toggle through to the callback', async () => {
    const user = userEvent.setup()
    const { onToggleTheme } = renderShell()
    await user.click(
      screen.getByRole('button', { name: /switch to light theme/i }),
    )
    expect(onToggleTheme).toHaveBeenCalledTimes(1)
  })

  it('labels the toggle for the opposite theme when light is active', () => {
    const { hook } = memoryLocation({ path: '/map' })
    const qc = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    render(
      <QueryClientProvider client={qc}>
        <Router hook={hook}>
          <Shell theme="light" onToggleTheme={() => {}}>
            <div />
          </Shell>
        </Router>
      </QueryClientProvider>,
    )
    expect(
      screen.getByRole('button', { name: /switch to dark theme/i }),
    ).toBeInTheDocument()
  })
})
