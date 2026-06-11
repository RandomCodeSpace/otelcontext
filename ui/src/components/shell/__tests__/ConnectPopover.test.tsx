import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import ConnectPopover from '../ConnectPopover'

// userEvent.setup() installs its own navigator.clipboard stub, so the spy
// must wrap that stub (defining our own mock first would be overwritten).
function setup() {
  const user = userEvent.setup()
  const writeText = vi
    .spyOn(navigator.clipboard, 'writeText')
    .mockResolvedValue(undefined)
  return { user, writeText }
}

describe('ConnectPopover', () => {
  it('opens a menu listing the three connect endpoints', async () => {
    const { user } = setup()
    render(<ConnectPopover />)

    await user.click(screen.getByRole('button', { name: /connect/i }))

    expect(await screen.findByText(/MCP URL/i)).toBeInTheDocument()
    expect(screen.getByText(/OTLP gRPC/i)).toBeInTheDocument()
    expect(screen.getByText(/OTLP HTTP/i)).toBeInTheDocument()
  })

  it('copies the MCP URL derived from the page origin', async () => {
    const { user, writeText } = setup()
    render(<ConnectPopover />)

    await user.click(screen.getByRole('button', { name: /connect/i }))
    await user.click(await screen.findByRole('menuitem', { name: /MCP URL/i }))

    expect(writeText).toHaveBeenCalledWith(`${window.location.origin}/mcp`)
  })

  it('copies the OTLP gRPC host:port', async () => {
    const { user, writeText } = setup()
    render(<ConnectPopover />)

    await user.click(screen.getByRole('button', { name: /connect/i }))
    await user.click(
      await screen.findByRole('menuitem', { name: /OTLP gRPC/i }),
    )

    expect(writeText).toHaveBeenCalledWith(`${window.location.hostname}:4317`)
  })

  it('copies the OTLP HTTP base URL', async () => {
    const { user, writeText } = setup()
    render(<ConnectPopover />)

    await user.click(screen.getByRole('button', { name: /connect/i }))
    await user.click(
      await screen.findByRole('menuitem', { name: /OTLP HTTP/i }),
    )

    expect(writeText).toHaveBeenCalledWith(`${window.location.origin}/v1/`)
  })

  it('keeps the menu open after a copy so several values can be taken', async () => {
    const { user } = setup()
    render(<ConnectPopover />)

    await user.click(screen.getByRole('button', { name: /connect/i }))
    await user.click(await screen.findByRole('menuitem', { name: /MCP URL/i }))

    expect(screen.getByRole('menuitem', { name: /OTLP gRPC/i })).toBeInTheDocument()
  })
})
