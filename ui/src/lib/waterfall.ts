// Waterfall layout math for the /traces detail view. Pure functions —
// the SVG renderer consumes the fractions produced here, so all of the
// tricky cases (orphan parents, cycles, clock skew, zero durations) are
// unit-testable without DOM.
//
// Span contract mirrors GET /api/traces/{id} (internal/api/views/views.go):
// RFC3339 start_time, duration in microseconds, parent_span_id linking.
// Structural subset of types/api.ts Span so callers get their full span
// back in each row (buildWaterfall is generic over it).

export interface WaterfallSpan {
  span_id: string
  parent_span_id: string
  /** RFC3339 */
  start_time: string
  /** microseconds */
  duration: number
  service_name: string
  /** OTLP status code, e.g. STATUS_CODE_ERROR */
  status?: string
}

export interface WaterfallRow<S extends WaterfallSpan = WaterfallSpan> {
  span: S
  /** Tree depth — 0 for roots/orphans. */
  depth: number
  /** Bar start as a fraction of the trace duration, 0..1. */
  offsetFrac: number
  /** Bar width as a fraction of the trace duration; offset+width ≤ 1. */
  widthFrac: number
  /** Deterministic service hue, 0..359 (low-saturation fill). */
  hue: number
  isError: boolean
}

export interface WaterfallLayout<S extends WaterfallSpan = WaterfallSpan> {
  rows: WaterfallRow<S>[]
  /** Epoch ms of the earliest span start. */
  traceStartMs: number
  /** Wall-clock extent of the trace in ms (≥ 0). */
  totalMs: number
}

/** Deterministic string hash → hue bucket (FNV-1a, 32-bit). */
export function serviceHue(name: string): number {
  let hash = 0x811c9dc5
  for (let i = 0; i < name.length; i++) {
    hash ^= name.charCodeAt(i)
    hash = Math.imul(hash, 0x01000193)
  }
  return (hash >>> 0) % 360
}

interface Node<S extends WaterfallSpan> {
  span: S
  startMs: number
  durationMs: number
}

function toNode<S extends WaterfallSpan>(span: S): Node<S> {
  const parsed = Date.parse(span.start_time)
  const startMs = Number.isFinite(parsed) ? parsed : 0
  const durationMs =
    Number.isFinite(span.duration) && span.duration > 0
      ? span.duration / 1000
      : 0
  return { span, startMs, durationMs }
}

function byStartThenId<S extends WaterfallSpan>(
  a: Node<S>,
  b: Node<S>,
): number {
  if (a.startMs !== b.startMs) return a.startMs - b.startMs
  return a.span.span_id < b.span.span_id ? -1 : 1
}

const clamp01 = (v: number) => Math.min(1, Math.max(0, v))

/**
 * Compute the time-positioned waterfall layout for a trace's spans:
 * depth-first row order (children sorted by start time, span_id tiebreak),
 * bar offset = start − trace start, bar width = duration — both as
 * fractions of the trace extent so the renderer scales freely.
 *
 * Robustness: spans whose parent is missing become roots; parent cycles
 * are broken by an emitted-set guard; malformed timestamps collapse to
 * offset 0; a zero-extent trace renders every bar at full width.
 */
export function buildWaterfall<S extends WaterfallSpan>(
  spans: S[],
): WaterfallLayout<S> {
  if (spans.length === 0) return { rows: [], traceStartMs: 0, totalMs: 0 }

  const nodes = spans.map(toNode)
  const byId = new Map<string, Node<S>>()
  for (const n of nodes) byId.set(n.span.span_id, n)

  const traceStartMs = Math.min(...nodes.map((n) => n.startMs))
  const traceEndMs = Math.max(...nodes.map((n) => n.startMs + n.durationMs))
  const totalMs = Math.max(0, traceEndMs - traceStartMs)

  // children keyed by parent span id; roots = no parent, unknown parent,
  // or self-parent. Cycle members surface later via the emitted-set sweep.
  const children = new Map<string, Node<S>[]>()
  const roots: Node<S>[] = []
  for (const n of nodes) {
    const pid = n.span.parent_span_id
    if (!pid || pid === n.span.span_id || !byId.has(pid)) {
      roots.push(n)
    } else {
      const siblings = children.get(pid)
      if (siblings) siblings.push(n)
      else children.set(pid, [n])
    }
  }
  roots.sort(byStartThenId)
  for (const siblings of children.values()) siblings.sort(byStartThenId)

  const rows: WaterfallRow<S>[] = []
  const emitted = new Set<string>()
  // span of a 0ms trace still needs a visible bar — treat extent as 1.
  const denomMs = totalMs > 0 ? totalMs : 1

  const emit = (node: Node<S>, depth: number): void => {
    if (emitted.has(node.span.span_id)) return // cycle guard
    emitted.add(node.span.span_id)
    const offsetFrac = clamp01((node.startMs - traceStartMs) / denomMs)
    const widthFrac = Math.min(
      totalMs > 0 ? clamp01(node.durationMs / denomMs) : 1,
      1 - offsetFrac,
    )
    rows.push({
      span: node.span,
      depth,
      offsetFrac,
      widthFrac,
      hue: serviceHue(node.span.service_name),
      isError: (node.span.status ?? '').toUpperCase().includes('ERROR'),
    })
    for (const child of children.get(node.span.span_id) ?? []) {
      emit(child, depth + 1)
    }
  }

  for (const root of roots) emit(root, 0)
  // Cycle members unreachable from any root: emit them flat so every
  // span the API returned is on screen.
  for (const n of nodes) {
    if (!emitted.has(n.span.span_id)) emit(n, 0)
  }

  return { rows, traceStartMs, totalMs }
}
