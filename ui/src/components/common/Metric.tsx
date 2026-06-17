interface MetricProps {
  /** Uppercase instrument legend, e.g. "P99". Omit for a bare numeral. */
  label?: string
  /** The numeral, pre-formatted by the caller (e.g. via format.ts). */
  value: string
  /** Optional unit suffix rendered tight against the value, e.g. "ms", "%". */
  unit?: string
}

/**
 * The Instrument Deck "indicator" primitive: a measured quantity rendered in
 * the mono/tabular-nums voice with an optional uppercase legend and unit. The
 * accessible name reads "<LEGEND> <value><unit>" so screen readers announce a
 * complete reading while the visual pieces stay aria-hidden.
 */
export function Metric({ label, value, unit }: Readonly<MetricProps>) {
  const aria = [label, `${value}${unit ?? ''}`].filter(Boolean).join(' ')
  return (
    <span role="group" aria-label={aria}>
      {label ? <span className="legend">{label}</span> : null}
      <span className="num" aria-hidden="true">
        {value}
      </span>
      {unit ? (
        <span className="unit" aria-hidden="true">
          {unit}
        </span>
      ) : null}
    </span>
  )
}
