import styles from './Sparkline.module.css'

interface SparklineProps {
  /** Series values, oldest first. */
  values: readonly number[]
  label: string
}

const W = 96
const H = 24
const PAD = 2

/** Fixed 96×24 SVG sparkline — no axes, no chrome, token colors only. */
export default function Sparkline({ values, label }: Readonly<SparklineProps>) {
  if (values.length < 2) return null
  const max = Math.max(...values, 1)
  const step = (W - PAD * 2) / (values.length - 1)
  const points = values
    .map((v, i) => {
      const x = PAD + i * step
      const y = H - PAD - (Math.max(0, v) / max) * (H - PAD * 2)
      return `${x.toFixed(1)},${y.toFixed(1)}`
    })
    .join(' ')
  return (
    <svg
      className={styles.spark}
      width={W}
      height={H}
      viewBox={`0 0 ${W} ${H}`}
      role="img"
      aria-label={label}
      data-testid="sparkline"
    >
      <polyline className={styles.line} points={points} />
    </svg>
  )
}
