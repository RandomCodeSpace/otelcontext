import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import ConnectInline from '../ConnectInline'

afterEach(() => {
  vi.unstubAllGlobals()
})

function stubClipboard(writeText: (text: string) => Promise<void>) {
  vi.stubGlobal('navigator', {
    ...navigator,
    clipboard: { writeText },
  } as Navigator)
}

describe('ConnectInline', () => {
  it('lists the MCP and OTLP endpoints derived from the page origin', () => {
    render(<ConnectInline />)
    expect(screen.getByText('MCP URL')).toBeInTheDocument()
    expect(screen.getByText('OTLP gRPC')).toBeInTheDocument()
    expect(screen.getByText('OTLP HTTP')).toBeInTheDocument()
    expect(screen.getByText(/:4317$/)).toBeInTheDocument()
  })

  it('copies a value on tap', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    stubClipboard(writeText)
    render(<ConnectInline />)
    fireEvent.click(screen.getByRole('button', { name: /copy otlp grpc/i }))
    await waitFor(() =>
      expect(writeText).toHaveBeenCalledWith(expect.stringMatching(/:4317$/)),
    )
  })

  it('survives a clipboard rejection (value stays selectable)', async () => {
    const writeText = vi.fn().mockRejectedValue(new Error('denied'))
    stubClipboard(writeText)
    render(<ConnectInline />)
    fireEvent.click(screen.getByRole('button', { name: /copy mcp url/i }))
    await waitFor(() => expect(writeText).toHaveBeenCalled())
    expect(screen.getByText(/\/mcp$/)).toBeInTheDocument()
  })
})
