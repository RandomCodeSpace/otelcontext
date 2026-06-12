import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { LogEntry } from '@/types/api'
import { WsManager } from '../wsManager'

// wsManager draws jitter from lib/random (CSPRNG-backed); stub the module so
// fake-timer advancement stays deterministic.
const jitter = vi.hoisted(() => ({ value: 0 }))
vi.mock('../random', () => ({ cryptoRandom: () => jitter.value }))

// Local WebSocket mock, kept file-local so no suite depends on shared
// mutable test state.
class MockWebSocket {
  static readonly CONNECTING = 0
  static readonly OPEN = 1
  static readonly CLOSING = 2
  static readonly CLOSED = 3

  static readonly instances: MockWebSocket[] = []

  readyState = MockWebSocket.CONNECTING
  url: string
  onopen: ((ev: Event) => void) | null = null
  onmessage: ((ev: MessageEvent<string>) => void) | null = null
  onerror: ((ev: Event) => void) | null = null
  onclose: ((ev: CloseEvent) => void) | null = null
  send = vi.fn<(data: string) => void>()
  close = vi.fn<() => void>(() => {
    this.readyState = MockWebSocket.CLOSED
    queueMicrotask(() => this.onclose?.(new CloseEvent('close')))
  })

  constructor(url: string) {
    this.url = url
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

  simulateMessage(data: unknown) {
    this.onmessage?.(
      new MessageEvent<string>('message', { data: JSON.stringify(data) }),
    )
  }
}

const sockets = () => MockWebSocket.instances
const latest = () => MockWebSocket.instances[MockWebSocket.instances.length - 1]

function logEntry(id: number): LogEntry {
  return {
    id,
    trace_id: '',
    span_id: '',
    severity: 'INFO',
    body: `log ${id}`,
    service_name: 'svc',
    attributes_json: '',
    timestamp: '2026-06-11T00:00:00Z',
  }
}

function setVisibility(state: DocumentVisibilityState) {
  Object.defineProperty(document, 'visibilityState', {
    configurable: true,
    get: () => state,
  })
  document.dispatchEvent(new Event('visibilitychange'))
}

let manager: WsManager | null = null

beforeEach(() => {
  MockWebSocket.instances.length = 0
  vi.stubGlobal('WebSocket', MockWebSocket as unknown as typeof WebSocket)
  vi.useFakeTimers()
})

afterEach(() => {
  manager?.stop()
  manager = null
  vi.useRealTimers()
  vi.unstubAllGlobals()
  setVisibility('visible')
})

describe('WsManager connection lifecycle', () => {
  it('connects to /ws and reports connected on open', () => {
    manager = new WsManager()
    manager.start()

    expect(sockets()).toHaveLength(1)
    expect(latest().url).toMatch(/\/ws$/)
    expect(manager.getStatusSnapshot().status).toBe('connecting')

    latest().simulateOpen()
    expect(manager.getStatusSnapshot()).toEqual({
      status: 'connected',
      attempt: 0,
    })
  })

  it('start() is idempotent — one socket per manager', () => {
    manager = new WsManager()
    manager.start()
    manager.start()
    expect(sockets()).toHaveLength(1)
  })

  it('schedules an exponential reconnect after close (jitter floor)', () => {
    jitter.value = 0
    manager = new WsManager()
    manager.start()
    latest().simulateOpen()

    latest().simulateClose()
    expect(manager.getStatusSnapshot().status).toBe('reconnecting')
    expect(manager.getStatusSnapshot().attempt).toBe(1)

    vi.advanceTimersByTime(99)
    expect(sockets()).toHaveLength(1)
    vi.advanceTimersByTime(1)
    expect(sockets()).toHaveLength(2)

    // Second failure → 200ms.
    latest().simulateClose()
    expect(manager.getStatusSnapshot().attempt).toBe(2)
    vi.advanceTimersByTime(199)
    expect(sockets()).toHaveLength(2)
    vi.advanceTimersByTime(1)
    expect(sockets()).toHaveLength(3)
  })

  it('adds up to 20% jitter to the backoff delay', () => {
    jitter.value = 1
    manager = new WsManager()
    manager.start()
    latest().simulateClose()

    // delay = 100 + 100 * 0.2 = 120ms at the jitter ceiling.
    vi.advanceTimersByTime(119)
    expect(sockets()).toHaveLength(1)
    vi.advanceTimersByTime(1)
    expect(sockets()).toHaveLength(2)
  })

  it('caps the backoff base at maxBackoffMs', () => {
    jitter.value = 0
    manager = new WsManager({ initialBackoffMs: 100, maxBackoffMs: 300 })
    manager.start()

    // Attempts: 100, 200, then capped at 300.
    latest().simulateClose()
    vi.advanceTimersByTime(100)
    latest().simulateClose()
    vi.advanceTimersByTime(200)
    latest().simulateClose()
    vi.advanceTimersByTime(299)
    expect(sockets()).toHaveLength(3)
    vi.advanceTimersByTime(1)
    expect(sockets()).toHaveLength(4)
  })

  it('reconnects immediately with reset attempts when the tab becomes visible', () => {
    jitter.value = 0
    manager = new WsManager()
    manager.start()

    // Drive into a long backoff.
    latest().simulateClose()
    vi.advanceTimersByTime(100)
    latest().simulateClose()
    expect(manager.getStatusSnapshot().attempt).toBe(2)

    setVisibility('hidden')
    setVisibility('visible')
    // Visibility recovery connects synchronously (no backoff wait).
    expect(sockets()).toHaveLength(3)
    latest().simulateOpen()
    expect(manager.getStatusSnapshot()).toEqual({
      status: 'connected',
      attempt: 0,
    })
  })

  it('stop() closes the socket and halts reconnects', () => {
    manager = new WsManager()
    manager.start()
    const socket = latest()
    manager.stop()

    expect(socket.close).toHaveBeenCalled()
    expect(manager.getStatusSnapshot().status).toBe('disconnected')

    vi.advanceTimersByTime(60_000)
    expect(sockets()).toHaveLength(1)
  })
})

describe('WsManager heartbeat', () => {
  it('pings every 30s and recycles a connection that goes silent', () => {
    manager = new WsManager()
    manager.start()
    const socket = latest()
    socket.simulateOpen()

    vi.advanceTimersByTime(30_000)
    expect(socket.send).toHaveBeenCalledTimes(1)
    expect(JSON.parse(socket.send.mock.calls[0][0])).toEqual({ type: 'ping' })

    // No inbound traffic for 35s after the ping → watchdog closes the socket.
    vi.advanceTimersByTime(35_000)
    expect(socket.close).toHaveBeenCalled()
  })

  it('any inbound message clears the dead-connection watchdog', () => {
    manager = new WsManager()
    manager.start()
    const socket = latest()
    socket.simulateOpen()

    vi.advanceTimersByTime(30_000) // ping at t30, watchdog armed for t65
    socket.simulateMessage({ type: 'pong' })
    vi.advanceTimersByTime(34_000) // t64 — past the t65-armed watchdog? no: cleared
    expect(socket.close).not.toHaveBeenCalled()
  })
})

describe('WsManager log ring buffer', () => {
  it('appends pushed log batches and bumps the version', () => {
    manager = new WsManager()
    manager.start()
    latest().simulateOpen()

    const v0 = manager.getLogsVersion()
    latest().simulateMessage({ type: 'logs', data: [logEntry(1), logEntry(2)] })
    expect(manager.getLogs()).toHaveLength(2)
    expect(manager.getLogsVersion()).toBeGreaterThan(v0)
  })

  it('caps the buffer at logCapacity, dropping oldest first', () => {
    manager = new WsManager({ logCapacity: 5 })
    manager.start()
    latest().simulateOpen()

    latest().simulateMessage({
      type: 'logs',
      data: [1, 2, 3, 4].map(logEntry),
    })
    latest().simulateMessage({ type: 'logs', data: [5, 6, 7].map(logEntry) })

    const ids = manager.getLogs().map((l) => l.id)
    expect(ids).toEqual([3, 4, 5, 6, 7])
  })

  it('defaults the cap to 5000 entries', () => {
    manager = new WsManager()
    manager.start()
    latest().simulateOpen()

    latest().simulateMessage({
      type: 'logs',
      data: Array.from({ length: 5001 }, (_, i) => logEntry(i)),
    })
    expect(manager.getLogs()).toHaveLength(5000)
    expect(manager.getLogs()[0].id).toBe(1) // entry 0 evicted
  })

  it('bumps the version at most once per 250ms window', () => {
    manager = new WsManager()
    manager.start()
    latest().simulateOpen()

    const notify = vi.fn()
    manager.subscribeLogs(notify)

    latest().simulateMessage({ type: 'logs', data: [logEntry(1)] })
    expect(notify).toHaveBeenCalledTimes(1) // first bump is immediate

    latest().simulateMessage({ type: 'logs', data: [logEntry(2)] })
    latest().simulateMessage({ type: 'logs', data: [logEntry(3)] })
    expect(notify).toHaveBeenCalledTimes(1) // coalesced

    vi.advanceTimersByTime(250)
    expect(notify).toHaveBeenCalledTimes(2) // one trailing bump for both
    expect(manager.getLogs()).toHaveLength(3)
  })

  it('tracks a monotonic appended total that survives ring eviction', () => {
    manager = new WsManager({ logCapacity: 3 })
    manager.start()
    latest().simulateOpen()

    expect(manager.getLogsTotal()).toBe(0)
    latest().simulateMessage({ type: 'logs', data: [1, 2].map(logEntry) })
    expect(manager.getLogsTotal()).toBe(2)
    latest().simulateMessage({ type: 'logs', data: [3, 4, 5].map(logEntry) })
    // Buffer holds only 3 entries, but the total keeps counting.
    expect(manager.getLogs()).toHaveLength(3)
    expect(manager.getLogsTotal()).toBe(5)
  })

  it('ignores malformed and non-log frames', () => {
    manager = new WsManager()
    manager.start()
    latest().simulateOpen()

    latest().onmessage?.(
      new MessageEvent<string>('message', { data: 'not json' }),
    )
    latest().simulateMessage({ type: 'logs', data: 'not an array' })
    latest().simulateMessage({ type: 'metrics', data: [] })
    expect(manager.getLogs()).toHaveLength(0)
  })
})

describe('WsManager external-store contract', () => {
  it('returns a stable snapshot reference between changes', () => {
    manager = new WsManager()
    manager.start()
    const a = manager.getStatusSnapshot()
    const b = manager.getStatusSnapshot()
    expect(a).toBe(b)

    latest().simulateOpen()
    const c = manager.getStatusSnapshot()
    expect(c).not.toBe(a)
    expect(manager.getStatusSnapshot()).toBe(c)
  })

  it('notifies status subscribers and honors unsubscribe', () => {
    manager = new WsManager()
    const seen: string[] = []
    const unsub = manager.subscribeStatus(() => {
      seen.push(manager!.getStatusSnapshot().status)
    })
    manager.start()
    latest().simulateOpen()
    expect(seen).toContain('connected')

    const count = seen.length
    unsub()
    latest().simulateClose()
    expect(seen).toHaveLength(count)
  })
})
