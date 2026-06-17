import { Metric } from './Metric'
import styles from './Gauge.module.css'

/** Status palette for the arc fill. Saturation is rationed to status. */
type GaugeTone = 'ok' | 'warn' | 'crit' | 'accent'

interface GaugeProps {
  /** Raw measured value (drives the arc fill and aria-valuenow). */
  value: number
  /** Upper bound of the scale. Defaults to 1 (a 0–1 ratio). */
  max?: number
  /** Accessible name fragment, e.g. "HEALTH". Also the visible legend. */
  label: string
  /** Pre-formatted numeral — the source of truth, always rendered. */
  valueText: string
  /** Optional unit suffix rendered tight against the numeral. */
  unit?: string
  /** Arc fill color token. Defaults to the single accent. */
  tone?: GaugeTone
}

// 270° sweep with a 90° gap centered at the bottom of the dial. The arc starts
// at the lower-left (135° from +x, measuring clockwise in SVG's y-down space)
// and sweeps clockwise through the top to the lower-right.
const SWEEP_DEG = 270
const RADIUS = 42
const CENTER = 50
const CIRCUMFERENCE = 2 * Math.PI * RADIUS
const ARC_LENGTH = CIRCUMFERENCE * (SWEEP_DEG / 360)
// Rotate so the 90° gap sits at the bottom: the dasharray begins at +x (3
// o'clock); rotating the whole ring by 135° puts the visible arc's start at the
// lower-left and its end at the lower-right.
const ROTATION = 135

function polar(angleDeg: number): { x: number; y: number } {
  const rad = (angleDeg * Math.PI) / 180
  return { x: CENTER + RADIUS * Math.cos(rad), y: CENTER + RADIUS * Math.sin(rad) }
}

// Endpoints of the 270° track, anchored to the same geometry as the dasharray.
const start = polar(ROTATION)
const end = polar(ROTATION + SWEEP_DEG)
// large-arc-flag is 1 because the swept angle (270°) exceeds 180°.
const ARC_PATH = `M ${start.x} ${start.y} A ${RADIUS} ${RADIUS} 0 1 1 ${end.x} ${end.y}`

const TONE_CLASS: Record<GaugeTone, string> = {
  ok: styles.ok,
  warn: styles.warn,
  crit: styles.crit,
  accent: styles.accent,
}

/**
 * Reusable SVG arc gauge (Instrument Deck). The numeral (`valueText`) is the
 * source of truth and is rendered centered in the mono voice via {@link Metric};
 * the 270° arc is decoration that fills proportionally to `value / max` and may
 * saturate without ever altering the numeral. Pure SVG + CSS, no chart library.
 */
export function Gauge({
  value,
  max = 1,
  label,
  valueText,
  unit,
  tone = 'accent',
}: Readonly<GaugeProps>) {
  const safeMax = max > 0 ? max : 1
  const fraction = Math.min(Math.max(value / safeMax, 0), 1)
  // Reveal `fraction` of the arc by offsetting the remainder out of view.
  const fillOffset = ARC_LENGTH * (1 - fraction)
  const aria = `${label} ${valueText}${unit ?? ''}`

  return (
    <div
      className={styles.gauge}
      role="meter"
      aria-label={aria}
      aria-valuenow={value}
      aria-valuemin={0}
      aria-valuemax={safeMax}
    >
      <svg
        className={styles.dial}
        viewBox="0 0 100 100"
        aria-hidden="true"
        focusable="false"
      >
        <path className={styles.track} d={ARC_PATH} strokeDasharray={ARC_LENGTH} />
        <path
          className={`${styles.fill} ${TONE_CLASS[tone]}`}
          d={ARC_PATH}
          strokeDasharray={ARC_LENGTH}
          strokeDashoffset={fillOffset}
        />
      </svg>
      <div className={styles.readout}>
        <Metric label={label} value={valueText} unit={unit} />
      </div>
    </div>
  )
}
