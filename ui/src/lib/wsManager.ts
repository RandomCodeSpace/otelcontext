import type { LogEntry } from '@/types/api'

// Module-level WebSocket singleton for the /ws hub stream. Ports the
// hardened lifecycle of the former hooks/useWebSocket.ts (exponential
// backoff, 30s ping heartbeat + 35s dead-connection watchdog,
// visibility/online recovery) out of React entirely, replacing that hook,
// and adds what the rewrite needs:
//   - jittered backoff: delay += Math.random() * delay * 0.2, so a fleet
//     of clients recovering from a restart doesn't reconnect in lockstep;
//   - a bounded ring buffer (cap 5000) for pushed log batches — the UI
//     must never mirror the backend's unbounded-growth incident;
//   - a version counter bumped at most once per 250ms so log consumers
//     re-render per tick, not per frame;
//   - useSyncExternalStore-compatible subscribe/getSnapshot pairs for
//     both connection status and the log buffer.

interface HubBatch {
  type: string
  data: unknown
}

export type WsStatus =
  | 'connecting'
  | 'connected'
  | 'disconnected'
  | 'reconnecting'

/** Immutable status snapshot — replaced (never mutated) on change. */
export interface WsStatusSnapshot {
  status: WsStatus
  /** Reconnect attempts since the last successful open (0 when connected). */
  attempt: number
}

export interface WsManagerOptions {
  /** WebSocket path appended to the page origin. Default "/ws". */
  path?: string
  initialBackoffMs?: number
  maxBackoffMs?: number
  heartbeatIntervalMs?: number
  heartbeatTimeoutMs?: number
  /** Log ring buffer capacity. Default 5000. */
  logCapacity?: number
  /** Minimum spacing between log version bumps. Default 250ms. */
  versionIntervalMs?: number
}

const DEFAULTS: Required<WsManagerOptions> = {
  path: '/ws',
  initialBackoffMs: 100,
  maxBackoffMs: 10_000,
  heartbeatIntervalMs: 30_000,
  heartbeatTimeoutMs: 35_000,
  logCapacity: 5000,
  versionIntervalMs: 250,
}

export class WsManager {
  private readonly opts: Required<WsManagerOptions>

  private socket: WebSocket | null = null
  private started = false
  private snapshot: WsStatusSnapshot = { status: 'disconnected', attempt: 0 }

  private reconnectTimer: ReturnType<typeof setTimeout> | null = null
  private heartbeatInterval: ReturnType<typeof setInterval> | null = null
  private heartbeatTimeout: ReturnType<typeof setTimeout> | null = null

  private readonly statusSubs = new Set<() => void>()
  private readonly logSubs = new Set<() => void>()

  private readonly logBuf: LogEntry[] = []
  private logVersion = 0
  private lastBumpAt = 0
  private versionTimer: ReturnType<typeof setTimeout> | null = null

  constructor(options: WsManagerOptions = {}) {
    this.opts = { ...DEFAULTS, ...options }
  }

  // ---- public lifecycle ----------------------------------------------------

  /** Open the connection and attach recovery listeners. Idempotent. */
  start(): void {
    if (this.started) return
    this.started = true
    document.addEventListener('visibilitychange', this.handleVisibility)
    window.addEventListener('online', this.handleOnline)
    this.connect()
  }

  /** Full teardown: close socket, clear timers, detach listeners. */
  stop(): void {
    if (!this.started) return
    this.started = false
    document.removeEventListener('visibilitychange', this.handleVisibility)
    window.removeEventListener('online', this.handleOnline)
    this.clearReconnectTimer()
    this.clearHeartbeat()
    if (this.versionTimer !== null) {
      clearTimeout(this.versionTimer)
      this.versionTimer = null
    }
    this.detachAndClose()
    this.setState('disconnected', 0)
  }

  // ---- useSyncExternalStore surface (status) --------------------------------

  readonly subscribeStatus = (cb: () => void): (() => void) => {
    this.statusSubs.add(cb)
    return () => this.statusSubs.delete(cb)
  }

  readonly getStatusSnapshot = (): WsStatusSnapshot => this.snapshot

  // ---- useSyncExternalStore surface (logs) ----------------------------------

  readonly subscribeLogs = (cb: () => void): (() => void) => {
    this.logSubs.add(cb)
    return () => this.logSubs.delete(cb)
  }

  /** Monotonic version, bumped at most once per versionIntervalMs. */
  readonly getLogsVersion = (): number => this.logVersion

  /**
   * Current ring buffer contents, oldest first. The array is live between
   * version bumps — copy if you need a stable view across ticks.
   */
  readonly getLogs = (): readonly LogEntry[] => this.logBuf

  // ---- connection internals --------------------------------------------------

  private connect(): void {
    if (!this.started) return
    this.clearReconnectTimer()
    this.clearHeartbeat()
    this.detachAndClose()

    this.setState(
      this.snapshot.attempt === 0 ? 'connecting' : 'reconnecting',
    )

    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
    let ws: WebSocket
    try {
      ws = new WebSocket(
        `${protocol}//${window.location.host}${this.opts.path}`,
      )
    } catch {
      this.scheduleReconnect()
      return
    }
    this.socket = ws

    ws.onopen = () => {
      if (!this.started) return
      this.setState('connected', 0)
      this.startHeartbeat()
    }

    ws.onmessage = (event: MessageEvent<string>) => {
      // Any inbound frame proves liveness — clear the watchdog.
      if (this.heartbeatTimeout !== null) {
        clearTimeout(this.heartbeatTimeout)
        this.heartbeatTimeout = null
      }
      let payload: HubBatch
      try {
        payload = JSON.parse(event.data) as HubBatch
      } catch {
        return // non-JSON frames (incl. server ping echoes) are ignored
      }
      if (payload.type === 'logs' && Array.isArray(payload.data)) {
        this.appendLogs(payload.data as LogEntry[])
      }
    }

    ws.onerror = () => {
      // onclose always follows — it owns reconnect scheduling.
    }

    ws.onclose = () => {
      if (!this.started) return
      if (this.socket === ws) this.socket = null
      this.clearHeartbeat()
      this.setState('disconnected')
      this.scheduleReconnect()
    }
  }

  private scheduleReconnect(): void {
    if (!this.started) return
    this.clearReconnectTimer()
    const attempt = this.snapshot.attempt
    let delay = Math.min(
      this.opts.initialBackoffMs * 2 ** attempt,
      this.opts.maxBackoffMs,
    )
    delay += Math.random() * delay * 0.2 // de-synchronize fleet reconnects
    this.setState('reconnecting', attempt + 1)
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null
      this.connect()
    }, delay)
  }

  private startHeartbeat(): void {
    this.clearHeartbeat()
    this.heartbeatInterval = setInterval(() => {
      const ws = this.socket
      if (!ws || ws.readyState !== WebSocket.OPEN) return
      try {
        ws.send(JSON.stringify({ type: 'ping' }))
      } catch {
        return // close/error handlers will drive recovery
      }
      // Arm the watchdog only if none is pending: the oldest unanswered
      // ping's deadline must hold. (The former useWebSocket hook re-armed
      // it on every ping, which let the deadline slide forever at a 30s
      // ping / 35s timeout — dead connections were never detected.)
      if (this.heartbeatTimeout !== null) return
      this.heartbeatTimeout = setTimeout(() => {
        this.heartbeatTimeout = null
        // Nothing heard since the ping — treat the connection as dead.
        try {
          this.socket?.close()
        } catch {
          // noop
        }
      }, this.opts.heartbeatTimeoutMs)
    }, this.opts.heartbeatIntervalMs)
  }

  private clearHeartbeat(): void {
    if (this.heartbeatInterval !== null) {
      clearInterval(this.heartbeatInterval)
      this.heartbeatInterval = null
    }
    if (this.heartbeatTimeout !== null) {
      clearTimeout(this.heartbeatTimeout)
      this.heartbeatTimeout = null
    }
  }

  private clearReconnectTimer(): void {
    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer)
      this.reconnectTimer = null
    }
  }

  /** Detach handlers before closing so the old socket can't re-schedule. */
  private detachAndClose(): void {
    const existing = this.socket
    if (!existing) return
    existing.onopen = null
    existing.onmessage = null
    existing.onerror = null
    existing.onclose = null
    try {
      existing.close()
    } catch {
      // noop
    }
    this.socket = null
  }

  private setState(status: WsStatus, attempt = this.snapshot.attempt): void {
    if (this.snapshot.status === status && this.snapshot.attempt === attempt) {
      return
    }
    this.snapshot = { status, attempt }
    for (const cb of this.statusSubs) cb()
  }

  // ---- recovery listeners ----------------------------------------------------

  private readonly handleVisibility = (): void => {
    if (document.visibilityState !== 'visible') return
    const ws = this.socket
    const dead =
      !ws ||
      ws.readyState === WebSocket.CLOSED ||
      ws.readyState === WebSocket.CLOSING
    if (dead) {
      this.setState(this.snapshot.status, 0) // fresh backoff ladder
      this.connect()
    }
  }

  private readonly handleOnline = (): void => {
    this.setState(this.snapshot.status, 0)
    this.connect()
  }

  // ---- log buffer internals ----------------------------------------------------

  private appendLogs(batch: LogEntry[]): void {
    for (const entry of batch) this.logBuf.push(entry)
    const overflow = this.logBuf.length - this.opts.logCapacity
    if (overflow > 0) this.logBuf.splice(0, overflow)
    this.requestVersionBump()
  }

  private requestVersionBump(): void {
    if (this.versionTimer !== null) return // trailing bump already queued
    const elapsed = Date.now() - this.lastBumpAt
    if (elapsed >= this.opts.versionIntervalMs) {
      this.bumpVersion()
      return
    }
    this.versionTimer = setTimeout(() => {
      this.versionTimer = null
      this.bumpVersion()
    }, this.opts.versionIntervalMs - elapsed)
  }

  private bumpVersion(): void {
    this.lastBumpAt = Date.now()
    this.logVersion += 1
    for (const cb of this.logSubs) cb()
  }
}

/** App-lifetime singleton. Created lazily so tests can use fresh instances. */
let singleton: WsManager | null = null

export function getWsManager(): WsManager {
  singleton ??= new WsManager()
  return singleton
}
