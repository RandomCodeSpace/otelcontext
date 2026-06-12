import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { Router } from 'wouter'
import { memoryLocation } from 'wouter/memory-location'
import PaletteHost from '../PaletteHost'

const fetchMock = vi.fn<typeof fetch>((input) => {
  const url = String(input)
  if (url.includes('/api/metadata/services')) {
    return Promise.resolve(
      new Response(JSON.stringify(['checkout', 'payments']), { status: 200 }),
    )
  }
  return Promise.resolve(new Response('{}', { status: 200 }))
})

beforeEach(() => {
  vi.stubGlobal('fetch', fetchMock)
})

afterEach(() => {
  vi.unstubAllGlobals()
  fetchMock.mockClear()
})

function setup({
  paletteOpen = true,
  shortcutsOpen = false,
  path = '/',
}: { paletteOpen?: boolean; shortcutsOpen?: boolean; path?: string } = {}) {
  const memory = memoryLocation({ path, record: true })
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const onPaletteOpenChange = vi.fn()
  const onShortcutsOpenChange = vi.fn()
  const onToggleTheme = vi.fn()
  render(
    <QueryClientProvider client={qc}>
      <Router hook={memory.hook} searchHook={memory.searchHook}>
        <PaletteHost
          paletteOpen={paletteOpen}
          onPaletteOpenChange={onPaletteOpenChange}
          shortcutsOpen={shortcutsOpen}
          onShortcutsOpenChange={onShortcutsOpenChange}
          onToggleTheme={onToggleTheme}
        />
      </Router>
    </QueryClientProvider>,
  )
  return { memory, qc, onPaletteOpenChange, onShortcutsOpenChange, onToggleTheme }
}

describe('CommandPalette', () => {
  it('shows Navigate, Actions, Services and Utilities sections', async () => {
    setup()
    expect(await screen.findByText('Navigate')).toBeInTheDocument()
    expect(screen.getByText('Actions')).toBeInTheDocument()
    expect(screen.getByText('Utilities')).toBeInTheDocument()
    // services arrive from /api/metadata/services
    expect(await screen.findByText('checkout')).toBeInTheDocument()
  })

  it('navigate commands route and close the palette', async () => {
    const user = userEvent.setup()
    const { memory, onPaletteOpenChange } = setup()
    await user.click(await screen.findByText('Traces'))
    expect(memory.history.at(-1)).toBe('/traces')
    expect(onPaletteOpenChange).toHaveBeenCalledWith(false)
  })

  it('selecting a service opens the Inspector (trail push)', async () => {
    const user = userEvent.setup()
    const { memory, onPaletteOpenChange } = setup({ path: '/map' })
    await user.click(await screen.findByText('payments'))
    const dest = memory.history.at(-1)!
    expect(dest).toContain('service=payments')
    expect(dest).toContain('trail=svc%3Apayments')
    expect(onPaletteOpenChange).toHaveBeenCalledWith(false)
  })

  it('root-cause action: two-step picker, then inspector Why tab + prefetch', async () => {
    const user = userEvent.setup()
    const { memory, qc } = setup({ path: '/map' })
    await user.click(await screen.findByText('Root cause analysis…'))
    // second step: service picker page
    expect(
      screen.getByPlaceholderText(/root cause of which service/i),
    ).toBeInTheDocument()
    expect(screen.queryByText('Navigate')).toBeNull()

    await user.click(await screen.findByText('checkout'))
    const dest = memory.history.at(-1)!
    expect(dest).toContain('service=checkout')
    expect(dest).toContain('tab=why')
    // the RPC was prefetched into the shared per-service cache entry
    await waitFor(() =>
      expect(
        qc.getQueryState(['mcp', 'root_cause_analysis', 'checkout']),
      ).toBeDefined(),
    )
  })

  it('blast-radius action lands on the Impact tab', async () => {
    const user = userEvent.setup()
    const { memory, qc } = setup({ path: '/' })
    await user.click(await screen.findByText('Blast radius…'))
    await user.click(await screen.findByText('payments'))
    expect(memory.history.at(-1)).toContain('tab=impact')
    await waitFor(() =>
      expect(
        qc.getQueryState(['mcp', 'impact_analysis', 'payments']),
      ).toBeDefined(),
    )
  })

  it('search-logs action deep-links the /logs service search', async () => {
    const user = userEvent.setup()
    const { memory } = setup()
    await user.click(await screen.findByText('Search logs…'))
    await user.click(await screen.findByText('checkout'))
    expect(memory.history.at(-1)).toBe('/logs?service=checkout')
  })

  it('Escape on the picker page backs out instead of closing', async () => {
    const user = userEvent.setup()
    const { onPaletteOpenChange } = setup()
    await user.click(await screen.findByText('Blast radius…'))
    await user.keyboard('{Escape}')
    expect(await screen.findByText('Navigate')).toBeInTheDocument()
    expect(onPaletteOpenChange).not.toHaveBeenCalledWith(false)
    // a second Escape at the root closes
    await user.keyboard('{Escape}')
    expect(onPaletteOpenChange).toHaveBeenCalledWith(false)
  })

  it('utilities: toggle theme', async () => {
    const user = userEvent.setup()
    const { onToggleTheme, onPaletteOpenChange } = setup()
    await user.click(await screen.findByText('Toggle theme'))
    expect(onToggleTheme).toHaveBeenCalledTimes(1)
    expect(onPaletteOpenChange).toHaveBeenCalledWith(false)
  })

  it('utilities: copy MCP URL writes the origin-derived value', async () => {
    const user = userEvent.setup()
    const writeText = vi.fn(() => Promise.resolve())
    vi.stubGlobal('navigator', {
      ...window.navigator,
      clipboard: { writeText },
    })
    setup()
    await user.click(await screen.findByText('Copy MCP URL'))
    expect(writeText).toHaveBeenCalledWith(`${window.location.origin}/mcp`)
  })

  it('filters services by typed query', async () => {
    const user = userEvent.setup()
    setup()
    await screen.findByText('checkout')
    await user.type(screen.getByPlaceholderText(/type a command/i), 'paym')
    expect(screen.queryByText('checkout')).toBeNull()
    expect(screen.getByText('payments')).toBeInTheDocument()
  })
})

describe('ShortcutSheet', () => {
  it('lists the global and map shortcuts', async () => {
    setup({ paletteOpen: false, shortcutsOpen: true })
    expect(
      await screen.findByRole('heading', { name: /keyboard shortcuts/i }),
    ).toBeInTheDocument()
    expect(screen.getByText(/command palette/i)).toBeInTheDocument()
    expect(screen.getByText(/fit graph to view/i)).toBeInTheDocument()
  })

  it('closes via the close button', async () => {
    const user = userEvent.setup()
    const { onShortcutsOpenChange } = setup({
      paletteOpen: false,
      shortcutsOpen: true,
    })
    await user.click(
      await screen.findByRole('button', { name: /close shortcuts/i }),
    )
    expect(onShortcutsOpenChange).toHaveBeenCalledWith(false)
  })
})
