import { Fragment } from 'react'
import { formatMs } from '@/lib/format'
import type { WaterfallLayout } from '@/lib/waterfall'
import type { Span } from '@/types/api'
import styles from './TracesView.module.css'

// Fixed logical pixel geometry — the wrapper scrolls horizontally on
// narrow screens (the xs "pan" affordance). One SVG, flat primitives:
// no nested DOM per span.
const W = 960
const AXIS_H = 18
const ROW_H = 22
const BAR_H = 10
const LABEL_W = 220
const RIGHT_PAD = 64
const INDENT = 12
const MAX_INDENT_LEVELS = 8
const TRACK_W = W - LABEL_W - RIGHT_PAD
/** Mono 10px ≈ 6.2px/char — used to clip labels without DOM measurement. */
const CHAR_W = 6.2

function clipText(text: string, maxPx: number): string {
  const maxChars = Math.max(3, Math.floor(maxPx / CHAR_W))
  if (text.length <= maxChars) return text
  return `${text.slice(0, maxChars - 1)}…`
}

interface WaterfallProps {
  layout: WaterfallLayout<Span>
  selectedSpanId: string | null
  onSelect: (span: Span) => void
}

/**
 * Time-positioned trace waterfall: span bars offset by (start − trace
 * start), width = duration, depth-indented labels, service-hash hue at
 * low saturation, error spans in --crit. Rows are keyboard-walkable
 * (Tab + Enter/Space) and the whole chart is a single SVG.
 */
export default function Waterfall({
  layout,
  selectedSpanId,
  onSelect,
}: Readonly<WaterfallProps>) {
  const { rows, totalMs } = layout
  const height = AXIS_H + rows.length * ROW_H + 2
  const ticks = [0, 0.25, 0.5, 0.75, 1]

  return (
    <div className={styles.waterfallWrap}>
      <svg
        className={styles.waterfallSvg}
        width={W}
        height={height}
        role="group"
        aria-label={`Trace waterfall, ${rows.length} spans over ${formatMs(totalMs)}`}
      >
        {/* time axis */}
        {ticks.map((t) => {
          const x = LABEL_W + t * TRACK_W
          return (
            <Fragment key={t}>
              <line
                className={styles.axisLine}
                x1={x}
                y1={AXIS_H}
                x2={x}
                y2={height}
              />
              <text
                className={styles.axisLabel}
                x={x}
                y={AXIS_H - 6}
                textAnchor={t === 1 ? 'end' : 'middle'}
              >
                {formatMs(totalMs * t)}
              </text>
            </Fragment>
          )
        })}

        {rows.map((row, i) => {
          const y = AXIS_H + i * ROW_H
          const indent = Math.min(row.depth, MAX_INDENT_LEVELS) * INDENT
          const barX = LABEL_W + row.offsetFrac * TRACK_W
          const barW = Math.max(2, row.widthFrac * TRACK_W)
          const selected = row.span.span_id === selectedSpanId
          const durationMs = row.span.duration / 1000
          const label = clipText(
            row.span.operation_name || row.span.span_id,
            LABEL_W - indent - 8,
          )
          return (
            <g key={row.span.span_id}>
              {selected && (
                <rect
                  className={styles.rowBgSelected}
                  x={0}
                  y={y}
                  width={W}
                  height={ROW_H}
                />
              )}
              <text
                className={styles.spanLabel}
                x={indent + 4}
                y={y + ROW_H / 2 + 3.5}
              >
                {label}
              </text>
              <rect
                x={barX}
                y={y + (ROW_H - BAR_H) / 2}
                width={barW}
                height={BAR_H}
                rx={2}
                fill={
                  row.isError
                    ? 'var(--crit)'
                    : `hsl(${row.hue} 35% 52% / 0.85)`
                }
              />
              <text
                className={styles.spanDuration}
                x={W - RIGHT_PAD + 6}
                y={y + ROW_H / 2 + 3.5}
              >
                {formatMs(durationMs)}
              </text>
              {/* full-row hit target on top — keyboard and pointer */}
              <rect
                className={styles.spanBar}
                x={0}
                y={y}
                width={W}
                height={ROW_H}
                fill="transparent"
                role="button"
                tabIndex={0}
                aria-pressed={selected}
                aria-label={`${row.span.operation_name} · ${row.span.service_name} · ${formatMs(durationMs)}${row.isError ? ' · error' : ''}`}
                onClick={() => onSelect(row.span)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' || e.key === ' ') {
                    e.preventDefault()
                    onSelect(row.span)
                  }
                }}
              />
            </g>
          )
        })}
      </svg>
    </div>
  )
}
