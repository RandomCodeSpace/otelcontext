import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { Gauge } from '../Gauge'
import styles from '../Gauge.module.css'

// The arc geometry mirrors the component: a 270° sweep of radius 42.
const ARC_LENGTH = 2 * Math.PI * 42 * (270 / 360)

function fillPath(): SVGPathElement {
  // The fill arc is the path that carries a stroke-dashoffset.
  const paths = Array.from(document.querySelectorAll<SVGPathElement>('path'))
  const fill = paths.find((p) => p.getAttribute('stroke-dashoffset') !== null)
  if (!fill) throw new Error('fill arc not found')
  return fill
}

describe('Gauge', () => {
  it('always renders the numeral and legend (the source of truth)', () => {
    render(<Gauge value={0.94} label="HEALTH" valueText="94" unit="%" />)
    expect(screen.getByText('94')).toHaveClass('num')
    expect(screen.getByText('HEALTH')).toHaveClass('legend')
    expect(screen.getByText('%')).toBeInTheDocument()
  })

  it('exposes role=meter with aria-valuenow/min/max reflecting value and max', () => {
    render(
      <Gauge value={312} max={1000} label="P99" valueText="312" unit="ms" />,
    )
    const meter = screen.getByRole('meter')
    expect(meter).toHaveAttribute('aria-valuenow', '312')
    expect(meter).toHaveAttribute('aria-valuemin', '0')
    expect(meter).toHaveAttribute('aria-valuemax', '1000')
  })

  it('names the meter "<label> <valueText><unit>"', () => {
    render(<Gauge value={0.94} label="HEALTH" valueText="94" unit="%" />)
    expect(
      screen.getByRole('meter', { name: 'HEALTH 94%' }),
    ).toBeInTheDocument()
  })

  it('names the meter without a unit when none is given', () => {
    render(<Gauge value={5} max={10} label="SVCS" valueText="5/10" />)
    expect(
      screen.getByRole('meter', { name: 'SVCS 5/10' }),
    ).toBeInTheDocument()
  })

  it('defaults max to 1 for a 0–1 ratio', () => {
    render(<Gauge value={0.5} label="HEALTH" valueText="50" unit="%" />)
    expect(screen.getByRole('meter')).toHaveAttribute('aria-valuemax', '1')
  })

  it('maps tone to the matching status class on the fill arc', () => {
    const { rerender } = render(
      <Gauge value={0.5} label="X" valueText="50" tone="warn" />,
    )
    expect(fillPath()).toHaveClass(styles.warn)

    rerender(<Gauge value={0.5} label="X" valueText="50" tone="crit" />)
    expect(fillPath()).toHaveClass(styles.crit)

    rerender(<Gauge value={0.5} label="X" valueText="50" tone="ok" />)
    expect(fillPath()).toHaveClass(styles.ok)
  })

  it('defaults tone to accent', () => {
    render(<Gauge value={0.5} label="X" valueText="50" />)
    expect(fillPath()).toHaveClass(styles.accent)
  })

  it('reveals a proportional slice of the arc via stroke-dashoffset', () => {
    render(<Gauge value={0.25} label="X" valueText="25" unit="%" />)
    // 25% filled → 75% of the arc length offset out of view.
    const expected = ARC_LENGTH * 0.75
    const offset = Number(fillPath().getAttribute('stroke-dashoffset'))
    expect(offset).toBeCloseTo(expected, 5)
  })

  it('clamps an over-max value to a full arc (offset 0)', () => {
    render(<Gauge value={5} max={1} label="X" valueText="500" unit="%" />)
    const offset = Number(fillPath().getAttribute('stroke-dashoffset'))
    expect(offset).toBeCloseTo(0, 5)
  })

  it('clamps a negative value to an empty arc (offset = full length)', () => {
    render(<Gauge value={-3} max={10} label="X" valueText="0" />)
    const offset = Number(fillPath().getAttribute('stroke-dashoffset'))
    expect(offset).toBeCloseTo(ARC_LENGTH, 5)
  })

  it('treats a non-positive max as 1 without dividing by zero', () => {
    render(<Gauge value={1} max={0} label="X" valueText="1" />)
    const meter = screen.getByRole('meter')
    expect(meter).toHaveAttribute('aria-valuemax', '1')
    // value 1 / safeMax 1 = full arc.
    const offset = Number(fillPath().getAttribute('stroke-dashoffset'))
    expect(offset).toBeCloseTo(0, 5)
  })
})
