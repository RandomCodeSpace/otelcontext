import type { CSSProperties } from 'react'

interface TruncateProps {
  readonly text: string
  readonly style?: CSSProperties
}

// Layout-only inline styles (sanctioned escape hatch). `minWidth: 0` is the
// flex/grid fix that lets a shrinkable child ellipsize instead of overflowing.
const baseStyle: CSSProperties = {
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
  minWidth: 0,
  display: 'block',
}

/**
 * Single-line ellipsis with a native title tooltip on hover. Use for service
 * names, log bodies, trace ids, operation names — anything that can overflow.
 */
export default function Truncate({ text, style }: TruncateProps) {
  return (
    <span title={text} style={{ ...baseStyle, ...style }}>
      {text}
    </span>
  )
}
