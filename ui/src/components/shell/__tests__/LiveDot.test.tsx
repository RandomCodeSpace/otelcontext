import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, render, screen } from '@testing-library/react'
import { WsManager } from '@/lib/wsManager'
import LiveDot from '../LiveDot'

class MockWebSocket {
  static readonly CONNECTING = 0
  static readonly OPEN = 1
  static readonly CLOSING = 2
  static readonly CLOSED = 3
  static readonly instances: MockWebSocket[] = []

  readyState = MockWebSocket.CONNECTING
  onopen: ((ev: Event) => void) | null = null
  onmessage: ((ev: MessageEvent<string>) => void) | null = null
  onerror: ((ev: Event) => void) | null = null
  onclose: ((ev: CloseEvent) => void) | null = null
  send = vi.fn()
  close = vi.fn()

  constructor(public url: string) {
    MockWebSocket.instances.push(this)
  }

  simulateOpen() {
    this.readyState = MockWebSocket.OPEN
    this.onopen?.(new Event('open'))
  }

  simulateClose() {
    this.readyState = MockWebSocket.CLOSED
    this.onclose?.(new CloseEvent('close'))
  }
}

const latest = () => MockWebSocket.instances[MockWebSocket.instances.length - 1]

let manager: WsManager

beforeEach(() => {
  MockWebSocket.instances.length = 0
  vi.stubGlobal('WebSocket', MockWebSocket as unknown as typeof WebSocket)
  vi.useFakeTimers()
  manager = new WsManager()
})

afterEach(() => {
  manager.stop()
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

describe('LiveDot', () => {
  it('shows the warn/connecting state before the first open', () => {
    manager.start()
    render(<LiveDot manager={manager} />)
    const dot = screen.getByRole('status')
    expect(dot).toHaveAccessibleName('connecting')
  })

  it('shows the ok/live state once connected', () => {
    manager.start()
    render(<LiveDot manager={manager} />)
    act(() => latest().simulateOpen())
    expect(screen.getByRole('status')).toHaveAccessibleName('live')
  })

  it('shows reconnecting with the attempt count after a drop', () => {
    manager.start()
    render(<LiveDot manager={manager} />)
    act(() => {
      latest().simulateOpen()
      latest().simulateClose()
    })
    expect(screen.getByRole('status')).toHaveAccessibleName(
      'reconnecting (attempt 1)',
    )
  })

  it('shows offline when the manager is stopped', () => {
    manager.start()
    render(<LiveDot manager={manager} />)
    act(() => manager.stop())
    expect(screen.getByRole('status')).toHaveAccessibleName('offline')
  })
})
