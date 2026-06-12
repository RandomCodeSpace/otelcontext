import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import TrailBar from '../TrailBar'
import type { TrailFrame } from '@/lib/trail'

const FRAMES: TrailFrame[] = [
  { kind: 'svc', id: 'checkout' },
  { kind: 'svc', id: 'payments' },
  { kind: 'trace', id: 'a1b2c3d4e5f6a7b8c9d0' },
]

function renderBar(frames: TrailFrame[] = FRAMES) {
  const onPopTo = vi.fn()
  const onPopOne = vi.fn()
  render(<TrailBar frames={frames} onPopTo={onPopTo} onPopOne={onPopOne} />)
  return { onPopTo, onPopOne }
}

describe('TrailBar', () => {
  it('renders nothing at depth 0', () => {
    renderBar([])
    expect(screen.queryByRole('navigation')).not.toBeInTheDocument()
  })

  it('renders one chip per frame, top frame marked current', () => {
    renderBar()
    const chips = screen.getAllByRole('button')
    expect(chips).toHaveLength(3)
    expect(chips[2]).toHaveAttribute('aria-current', 'true')
    expect(chips[0]).not.toHaveAttribute('aria-current')
  })

  it('truncates long trace ids but keeps the full id as title', () => {
    renderBar()
    const chip = screen.getAllByRole('button')[2]
    expect(chip).toHaveAttribute('title', 'a1b2c3d4e5f6a7b8c9d0')
    expect(chip).toHaveTextContent('a1b2c3d4…')
  })

  it('chip tap pops to that frame index', async () => {
    const user = userEvent.setup()
    const { onPopTo } = renderBar()
    await user.click(screen.getByRole('button', { name: /checkout/ }))
    expect(onPopTo).toHaveBeenCalledWith(0)
  })

  it('Backspace pops the top frame', async () => {
    const user = userEvent.setup()
    const { onPopOne } = renderBar()
    await user.keyboard('{Backspace}')
    expect(onPopOne).toHaveBeenCalledTimes(1)
  })

  it('Backspace inside an input is left alone', async () => {
    const user = userEvent.setup()
    const onPopOne = vi.fn()
    render(
      <>
        <input aria-label="filter" defaultValue="ab" />
        <TrailBar frames={FRAMES} onPopTo={vi.fn()} onPopOne={onPopOne} />
      </>,
    )
    const input = screen.getByLabelText('filter')
    await user.click(input)
    await user.keyboard('{Backspace}')
    expect(onPopOne).not.toHaveBeenCalled()
    expect(input).toHaveValue('a')
  })
})
