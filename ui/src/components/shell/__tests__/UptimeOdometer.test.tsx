import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, render, screen } from '@testing-library/react'
import { UptimeOdometer } from '../UptimeOdometer'

beforeEach(() => {
  vi.useFakeTimers()
})

afterEach(() => {
  cleanup()
  vi.useRealTimers()
})

describe('UptimeOdometer', () => {
  it('renders d HH:MM:SS from the seconds prop', () => {
    // 6d 04:11:37 = 6*86400 + 4*3600 + 11*60 + 37
    const seconds = 6 * 86400 + 4 * 3600 + 11 * 60 + 37
    render(<UptimeOdometer uptimeSeconds={seconds} />)
    expect(screen.getByText('6d 04:11:37')).toBeInTheDocument()
  })

  it('shows the UP legend', () => {
    render(<UptimeOdometer uptimeSeconds={0} />)
    expect(screen.getByText('UP')).toBeInTheDocument()
  })

  it('zero-pads HH:MM:SS and shows 0d below a day', () => {
    // 5 minutes 9 seconds
    render(<UptimeOdometer uptimeSeconds={5 * 60 + 9} />)
    expect(screen.getByText('0d 00:05:09')).toBeInTheDocument()
  })

  it('advances roughly one second per real second', () => {
    render(<UptimeOdometer uptimeSeconds={10} />)
    expect(screen.getByText('0d 00:00:10')).toBeInTheDocument()
    act(() => {
      vi.advanceTimersByTime(3000)
    })
    expect(screen.getByText('0d 00:00:13')).toBeInTheDocument()
  })

  it('re-syncs to the prop when it changes (authoritative base)', () => {
    const { rerender } = render(<UptimeOdometer uptimeSeconds={10} />)
    act(() => {
      vi.advanceTimersByTime(5000)
    })
    expect(screen.getByText('0d 00:00:15')).toBeInTheDocument()
    // server poll comes back with a fresh value; treat it as the truth
    rerender(<UptimeOdometer uptimeSeconds={100} />)
    expect(screen.getByText('0d 00:01:40')).toBeInTheDocument()
    act(() => {
      vi.advanceTimersByTime(2000)
    })
    expect(screen.getByText('0d 00:01:42')).toBeInTheDocument()
  })

  it('flags a backward jump (process restart) with a flash class', () => {
    const { rerender, container } = render(<UptimeOdometer uptimeSeconds={10000} />)
    // restart: uptime drops sharply
    rerender(<UptimeOdometer uptimeSeconds={3} />)
    expect(screen.getByText('0d 00:00:03')).toBeInTheDocument()
    // the flash marker is present right after the backward jump
    expect(container.querySelector('[data-restarted="true"]')).not.toBeNull()
  })

  it('does not flag a forward (normal) prop update as a restart', () => {
    const { rerender, container } = render(<UptimeOdometer uptimeSeconds={10} />)
    rerender(<UptimeOdometer uptimeSeconds={20} />)
    expect(container.querySelector('[data-restarted="true"]')).toBeNull()
  })

  it('exposes a process-uptime accessible label', () => {
    render(<UptimeOdometer uptimeSeconds={6 * 86400 + 4 * 3600 + 11 * 60 + 37} />)
    const label = screen.getByLabelText(/process uptime/i)
    expect(label).toHaveAccessibleName(/6d 04:11:37/)
  })

  it('clears the interval on unmount', () => {
    const clearSpy = vi.spyOn(globalThis, 'clearInterval')
    const { unmount } = render(<UptimeOdometer uptimeSeconds={0} />)
    unmount()
    expect(clearSpy).toHaveBeenCalled()
    clearSpy.mockRestore()
  })

  it('floors fractional seconds', () => {
    render(<UptimeOdometer uptimeSeconds={61.9} />)
    expect(screen.getByText('0d 00:01:01')).toBeInTheDocument()
  })
})
