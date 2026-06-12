// Investigation Trail — a URL-encoded breadcrumb stack of the drill-down
// (`?trail=svc:checkout,svc:payments,trace:a1b2`). Pure data layer: parsing,
// serialization and stack ops. URL wiring lives in hooks/useInvestigation;
// rendering in components/trail/TrailBar.
//
// Frame kinds are extensible (trace frames round-trip today; their activation
// lands with the /traces route in a later phase).

export type TrailKind = 'svc' | 'trace'

export interface TrailFrame {
  kind: TrailKind
  id: string
}

/** Hard cap on stack depth — keeps shared URLs within sane length. */
export const MAX_TRAIL_FRAMES = 20

const KINDS: ReadonlySet<string> = new Set<TrailKind>(['svc', 'trace'])

/** Parse a `?trail=` value. Malformed entries are dropped, never thrown on. */
export function parseTrail(raw: string | null | undefined): TrailFrame[] {
  if (!raw?.trim()) return []
  const frames: TrailFrame[] = []
  for (const part of raw.split(',')) {
    const sep = part.indexOf(':')
    if (sep <= 0) continue
    const kind = part.slice(0, sep)
    if (!KINDS.has(kind)) continue
    let id: string
    try {
      id = decodeURIComponent(part.slice(sep + 1))
    } catch {
      continue // malformed percent-encoding
    }
    if (!id) continue
    frames.push({ kind: kind as TrailKind, id })
  }
  return frames.slice(-MAX_TRAIL_FRAMES)
}

/** Serialize to the `?trail=` value. Empty trail → empty string. */
export function serializeTrail(frames: readonly TrailFrame[]): string {
  return frames
    .map((f) => `${f.kind}:${encodeURIComponent(f.id)}`)
    .join(',')
}

/**
 * Push a drill-down frame. Identical consecutive frames collapse (re-opening
 * the current context is not a navigation); depth is capped by dropping the
 * oldest frame.
 */
export function pushFrame(
  frames: readonly TrailFrame[],
  frame: TrailFrame,
): readonly TrailFrame[] {
  const top = frames.at(-1)
  if (top && top.kind === frame.kind && top.id === frame.id) return frames
  return [...frames, frame].slice(-MAX_TRAIL_FRAMES)
}

/** Pop back to frame `index` (chip tap). Index is clamped into range. */
export function popTo(
  frames: readonly TrailFrame[],
  index: number,
): readonly TrailFrame[] {
  if (frames.length === 0) return frames
  const end = Math.min(Math.max(index, 0), frames.length - 1) + 1
  return end === frames.length ? frames : frames.slice(0, end)
}

/** Id of the top frame when it is a service, else null. */
export function topService(frames: readonly TrailFrame[]): string | null {
  const top = frames.at(-1)
  return top?.kind === 'svc' ? top.id : null
}
