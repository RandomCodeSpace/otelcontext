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

function renderShell(path = '/', onToggleTheme = vi.fn()) {
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
    expect(screen.getAllByRole('navigation').length).toBeGreaterThanOrEqual(1)
    expect(screen.getByRole('main')).toContainElement(
      screen.getByTestId('page-content'),
    )
  })

  it('renders the single Service Map destination in the rail', () => {
    renderShell()
    // The Service Map is the only human destination — logs/traces are MCP-tool
    // surfaces, the flow map folded into home, and the bottom tab bar was
    // removed (Search lives in the top pulse bar). One nav link, in the rail.
    expect(screen.getAllByRole('link', { name: /service map/i })).toHaveLength(1)
    expect(screen.getAllByRole('link')).toHaveLength(1)
  })

  it('marks the active route with aria-current', () => {
    renderShell('/')
    const active = screen
      .getAllByRole('link')
      .filter((a) => a.getAttribute('aria-current') === 'page')
    expect(active).toHaveLength(1) // the single rail nav link
    active.forEach((a) => expect(a).toHaveAttribute('href', '/'))
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
    expect(buttons).toHaveLength(1) // the pulse-bar ⌘K (bottom tab bar removed)
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
