import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { Metric } from '../Metric'

describe('Metric', () => {
  it('renders value + unit in the indicator (mono) voice', () => {
    render(<Metric label="P99" value="312" unit="ms" />)
    expect(screen.getByText('312')).toHaveClass('num')
    expect(screen.getByText('P99')).toHaveClass('legend')
    expect(screen.getByText('ms')).toBeInTheDocument()
  })

  it('omits the label when not provided', () => {
    render(<Metric value="94" unit="%" />)
    expect(screen.queryByText(/.+/, { selector: '.legend' })).toBeNull()
  })

  it('exposes an accessible label combining legend + value + unit', () => {
    render(<Metric label="ERR" value="4.2" unit="%" />)
    expect(screen.getByRole('group', { name: 'ERR 4.2%' })).toBeInTheDocument()
  })

  it('renders a bare numeral with no unit', () => {
    render(<Metric value="11/12" />)
    const group = screen.getByRole('group', { name: '11/12' })
    expect(group).toBeInTheDocument()
    expect(screen.getByText('11/12')).toHaveClass('num')
  })
})
