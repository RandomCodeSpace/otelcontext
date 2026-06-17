import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { AnomalyNode } from '@/types/api'
import AnomalyStrip from '../AnomalyStrip'

// Hoist-safe mock of the data hook so each state (pending/error/empty/ticks)
// is exercised deterministically without a live MCP call. These branches lived
// in the now-deleted TriageView.test.tsx; migrated here when AnomalyStrip moved
// under ConstellationHome.
const { useAnomalyTimeline } = vi.hoisted(() => ({ useAnomalyTimeline: vi.fn() }))
vi.mock('@/hooks/useAnomalyTimeline', () => ({ useAnomalyTimeline }))

const NOW = 1_700_000_000_000
const anomaly = (over: Partial<AnomalyNode> = {}): AnomalyNode =>
  ({
    service: 'order-service',
    severity: 'critical',
    timestamp: new Date(NOW - 60_000).toISOString(),
    ...over,
  }) as unknown as AnomalyNode

beforeEach(() => useAnomalyTimeline.mockReset())

describe('AnomalyStrip', () => {
  it('renders a skeleton while pending', () => {
    useAnomalyTimeline.mockReturnValue({ isPending: true, data: undefined, error: null, refetch: vi.fn() })
    render(<AnomalyStrip onOpenService={vi.fn()} />)
    expect(screen.getByTestId('strip-skeleton')).toBeInTheDocument()
  })

  it('shows an alert with the message and retries on click', async () => {
    const refetch = vi.fn()
    useAnomalyTimeline.mockReturnValue({ isPending: false, data: undefined, error: new Error('boom'), refetch })
    render(<AnomalyStrip onOpenService={vi.fn()} />)
    expect(screen.getByRole('alert')).toHaveTextContent('boom')
    await userEvent.click(screen.getByRole('button', { name: 'Retry' }))
    expect(refetch).toHaveBeenCalledOnce()
  })

  it('shows the empty state when there are no anomalies in the window', () => {
    useAnomalyTimeline.mockReturnValue({ isPending: false, data: { anomalies: [], fetchedAt: NOW }, error: null, refetch: vi.fn() })
    render(<AnomalyStrip onOpenService={vi.fn()} />)
    expect(screen.getByText(/No anomalies in the last hour/i)).toBeInTheDocument()
  })

  it('renders a recent-service chip and opens the inspector on click', async () => {
    const onOpenService = vi.fn()
    useAnomalyTimeline.mockReturnValue({
      isPending: false,
      data: { anomalies: [anomaly()], fetchedAt: NOW },
      error: null,
      refetch: vi.fn(),
    })
    render(<AnomalyStrip onOpenService={onOpenService} />)
    await userEvent.click(screen.getByRole('button', { name: /order-service/i }))
    expect(onOpenService).toHaveBeenCalledWith('order-service')
  })
})
