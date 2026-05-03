import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import ServiceSidePanel from '../ServiceSidePanel';
import type { SystemNode, SystemEdge } from '../../../types/api';

const mockNode: SystemNode = {
  id: 'inventory',
  type: 'service',
  health_score: 0.32,
  status: 'critical',
  metrics: { request_rate_rps: 670, error_rate: 0.084, avg_latency_ms: 142, p99_latency_ms: 890, span_count_1h: 5400 },
  alerts: ['error rate above 5%', 'avg latency above 500ms'],
};

const mockEdges: SystemEdge[] = [
  { source: 'user-api', target: 'inventory', call_count: 890, avg_latency_ms: 95, error_rate: 0.03, status: 'degraded' },
  { source: 'inventory', target: 'postgres', call_count: 1200, avg_latency_ms: 12, error_rate: 0, status: 'healthy' },
];

function renderPanel(overrides = {}) {
  const defaults = {
    node: mockNode,
    edges: mockEdges,
    onClose: vi.fn(),
    onSelectService: vi.fn(),
  };
  const props = { ...defaults, ...overrides };
  return { ...render(<ServiceSidePanel {...props} />), ...props };
}

describe('ServiceSidePanel', () => {
  it('renders service name and status badge text', () => {
    renderPanel();
    expect(screen.getByText('inventory')).toBeInTheDocument();
    expect(screen.getByText('critical')).toBeInTheDocument();
  });

  it('renders KPI values', () => {
    renderPanel();
    expect(screen.getByText('670')).toBeInTheDocument();
    expect(screen.getByText('8.4%')).toBeInTheDocument();
    expect(screen.getByText('142ms')).toBeInTheDocument();
    expect(screen.getByText('890ms')).toBeInTheDocument();
  });

  it('renders upstream service name', () => {
    renderPanel();
    expect(screen.getByText('user-api')).toBeInTheDocument();
  });

  it('renders alerts text', () => {
    renderPanel();
    expect(screen.getByText('error rate above 5%')).toBeInTheDocument();
    expect(screen.getByText('avg latency above 500ms')).toBeInTheDocument();
  });

  it('calls onSelectService when upstream service clicked', async () => {
    const { onSelectService } = renderPanel();
    const user = userEvent.setup();
    await user.click(screen.getByText('user-api'));
    expect(onSelectService).toHaveBeenCalledWith('user-api');
  });
});
