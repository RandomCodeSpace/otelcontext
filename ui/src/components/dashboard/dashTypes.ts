import type { AnomalySeverity, TrafficPoint } from '../../types/api'

// Map a GraphRAG anomaly severity to a design-system Timeline tone. The
// Timeline component has no 'info' tone, so info collapses to 'neutral'.
export function severityToTone(
  sev: AnomalySeverity,
): 'neutral' | 'success' | 'warning' | 'danger' {
  switch (sev) {
    case 'critical':
      return 'danger'
    case 'warning':
      return 'warning'
    default:
      return 'neutral' // 'info' -> neutral (no Timeline 'info' tone)
  }
}

// Map a /ready check string ("ok" | "skipped" | "not running" | "saturated
// NN%" | error text) to a design-system StatusDot status. StatusDot has no
// generic "ok"; "running" is the healthy green, "failed" the red, "idle" the
// muted grey for skipped/unknown probes.
export function readyCheckToStatus(
  value: string | undefined,
): 'running' | 'degraded' | 'failed' | 'idle' {
  if (!value) return 'idle'
  const v = value.toLowerCase()
  if (v === 'ok') return 'running'
  if (v === 'skipped') return 'idle'
  if (v.startsWith('saturated')) return 'degraded'
  return 'failed' // "not running", ping/connection errors, etc.
}

// Error-rate percentage for a single traffic bucket (0–100). Guards the
// divide-by-zero on idle minutes.
export function bucketErrorRatePct(p: TrafficPoint): number {
  if (!p.count) return 0
  return (p.error_count / p.count) * 100
}

// Health score (0–1 from the backend, occasionally 0–100) clamped to a 0–100
// percentage for display and gauge input.
export function healthPct(score: number | undefined): number {
  if (typeof score !== 'number' || Number.isNaN(score)) return 0
  const pct = score <= 1 ? score * 100 : score
  return Math.max(0, Math.min(100, pct))
}

// Format server uptime (system.uptime_seconds from /api/system/graph) to a
// compact human string: "45s", "12m", "3h 04m", "5d 02h". Returns "—" when
// absent/invalid.
export function formatUptime(seconds: number | undefined): string {
  if (typeof seconds !== 'number' || !Number.isFinite(seconds) || seconds < 0) {
    return '—'
  }
  const s = Math.floor(seconds)
  if (s < 60) return `${s}s`
  if (s < 3600) return `${Math.floor(s / 60)}m`
  if (s < 86400) {
    const h = Math.floor(s / 3600)
    const m = Math.floor((s % 3600) / 60)
    return `${h}h ${String(m).padStart(2, '0')}m`
  }
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  return `${d}d ${String(h).padStart(2, '0')}h`
}

// Coerce RepoStats.db_size_mb (loosely typed via the index signature — may be a
// number, a numeric string, or absent on non-SQLite drivers) to a number or
// null.
export function coerceDbSizeMb(value: unknown): number | null {
  if (typeof value === 'number' && Number.isFinite(value)) return value
  if (typeof value === 'string') {
    const n = Number.parseFloat(value)
    return Number.isFinite(n) ? n : null
  }
  return null
}
