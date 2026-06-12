import { useCallback, useMemo } from 'react'
import { useLocation, useSearch } from 'wouter'
import {
  parseTrail,
  popTo,
  pushFrame,
  serializeTrail,
  topService,
  type TrailFrame,
} from '@/lib/trail'
import { buildHref, readParam } from '@/lib/urlState'

// The single source of truth for the investigation state, all of it in the
// URL so it survives reload and is shareable into an incident channel:
//   ?service=X       — which service the Inspector shows (null = closed)
//   ?trail=svc:a,... — the drill-down breadcrumb stack
// Every mutation is ONE navigate(replace) so the two stay consistent and the
// browser history doesn't fill up with param churn.

export interface Investigation {
  /** Service shown in the Inspector, or null when closed. */
  service: string | null
  /** The breadcrumb stack, oldest first. */
  trail: readonly TrailFrame[]
  /** Drill into a service: pushes the trail and opens the Inspector. */
  openService: (id: string) => void
  /** Close the Inspector. The trail is history — it stays. */
  closeInspector: () => void
  /** Chip tap: pop back to frame `index` and activate it. */
  popToFrame: (index: number) => void
  /** Backspace: pop the top frame and activate the one below. */
  popOne: () => void
}

export function useInvestigation(): Investigation {
  const [path, navigate] = useLocation()
  const search = useSearch()

  const service = readParam(search, 'service')
  const trail = useMemo(() => parseTrail(readParam(search, 'trail')), [search])

  const apply = useCallback(
    (updates: Record<string, string | null>) => {
      navigate(buildHref(path, search, updates), { replace: true })
    },
    [navigate, path, search],
  )

  const openService = useCallback(
    (id: string) => {
      const next = pushFrame(trail, { kind: 'svc', id })
      apply({ service: id, trail: serializeTrail(next) || null })
    },
    [apply, trail],
  )

  const closeInspector = useCallback(() => {
    apply({ service: null })
  }, [apply])

  const activate = useCallback(
    (frames: readonly TrailFrame[]) => {
      apply({
        // svc frames re-open the Inspector; other kinds (trace) only truncate
        // for now — their activation lands with the /traces route.
        service: topService(frames),
        trail: serializeTrail(frames) || null,
      })
    },
    [apply],
  )

  const popToFrame = useCallback(
    (index: number) => activate(popTo(trail, index)),
    [activate, trail],
  )

  const popOne = useCallback(() => {
    if (trail.length === 0) return
    activate(trail.slice(0, -1))
  }, [activate, trail])

  return { service, trail, openService, closeInspector, popToFrame, popOne }
}
