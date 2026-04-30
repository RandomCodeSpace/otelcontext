import React, { useMemo } from 'react'
import {
  Alert,
  Badge,
  Card,
  Drawer,
  IconButton,
  PageHeader,
  Progress,
  Space,
  Spin,
  Table,
  type TableColumn,
} from '@ossrandom/design-system'
import { X } from 'lucide-react'
import type { DashboardStats, Trace } from '@/types/api'
import { fmt } from '../../lib/utils'
import StatRow from './StatRow'
import { useMediaQuery } from '../../hooks/useMediaQuery'

interface Props {
  traces: Trace[]
  selected: Trace | null
  loading: boolean
  error: string | null
  onSelect: (traceId: string) => void
  serviceFilter: string | null
  onClearFilter: () => void
  dashboard: DashboardStats | null
}

function statusTone(status: string): 'danger' | 'info' {
  return status.includes('ERROR') ? 'danger' : 'info'
}

const TracesView: React.FC<Props> = ({
  traces,
  selected,
  loading,
  error,
  onSelect,
  serviceFilter,
  onClearFilter,
  dashboard,
}) => {
  const isCompact = useMediaQuery('(max-width: 760px)')

  const filtered = useMemo(
    () => (serviceFilter ? traces.filter((t) => t.service_name === serviceFilter) : traces),
    [traces, serviceFilter],
  )

  const errorCount = useMemo(
    () => filtered.filter((t) => t.status?.includes('ERROR')).length,
    [filtered],
  )

  const avgDuration = useMemo(() => {
    if (filtered.length === 0) return 0
    const total = filtered.reduce((acc, t) => acc + (t.duration_ms ?? 0), 0)
    return total / filtered.length
  }, [filtered])

  const p95Duration = useMemo(() => {
    if (filtered.length === 0) return 0
    const sorted = [...filtered].map((t) => t.duration_ms ?? 0).sort((a, b) => a - b)
    const idx = Math.floor(sorted.length * 0.95)
    return sorted[Math.min(idx, sorted.length - 1)] ?? 0
  }, [filtered])

  const totalUs = Math.max(selected?.duration ?? 1, 1)

  const columns: TableColumn<Trace>[] = [
    {
      key: 'status',
      title: 'Status',
      width: 90,
      render: (_v, row) => <Badge tone={statusTone(row.status)} size="sm">{row.status || 'OK'}</Badge>,
    },
    {
      key: 'service_name',
      title: 'Service',
      dataKey: 'service_name',
      sortable: true,
    },
    {
      key: 'operation',
      title: 'Operation',
      render: (_v, row) => <code>{row.operation || row.trace_id}</code>,
    },
    {
      key: 'span_count',
      title: 'Spans',
      dataKey: 'span_count',
      align: 'right',
      width: 90,
    },
    {
      key: 'duration_ms',
      title: 'Duration',
      align: 'right',
      width: 120,
      render: (_v, row) => `${(row.duration_ms ?? 0).toFixed(1)} ms`,
    },
  ]

  return (
    <Space direction="vertical" size="md">
      <PageHeader
        size="sm"
        title="Distributed Traces"
        subtitle={`${filtered.length} recent · click a row for span waterfall`}
        inlineSubtitle
        actions={
          serviceFilter ? (
            <Space size="xs" align="center">
              <Badge tone="info" size="sm">Filtered: {serviceFilter}</Badge>
              <IconButton
                icon={<X size={11} />}
                aria-label="Clear filter"
                variant="ghost"
                size="xs"
                onClick={onClearFilter}
              />
            </Space>
          ) : undefined
        }
      />

      <StatRow
        items={[
          { label: 'In view', value: fmt(filtered.length) },
          { label: 'Errors', value: errorCount },
          { label: 'Avg', value: avgDuration.toFixed(1), unit: 'ms' },
          (() => {
            const base = dashboard?.avg_latency_ms
            const showDelta = base && base > 0
            const pct = showDelta ? ((p95Duration - base) / base) * 100 : 0
            return {
              label: 'p95',
              value: p95Duration.toFixed(1),
              unit: 'ms',
              delta: showDelta
                ? {
                    value: Number(pct.toFixed(1)),
                    direction: pct >= 0 ? 'up' : 'down',
                    tone: base && p95Duration > base * 2 ? 'bad' : 'neutral',
                  }
                : undefined,
            }
          })(),
        ]}
      />

      <Card bordered padding="sm" radius="md">
        {error && <Alert severity="danger">{error}</Alert>}
        {loading && filtered.length === 0 && <Spin label="Loading traces" />}
        {!loading && filtered.length === 0 && <Alert severity="info">No traces yet.</Alert>}
        {filtered.length > 0 && (
          <Table<Trace>
            columns={columns}
            data={filtered}
            rowKey="trace_id"
            density="compact"
            stickyHeader
            striped
            onRowClick={(row) => onSelect(row.trace_id)}
          />
        )}
      </Card>

      <Drawer
        open={selected !== null}
        onClose={() => onSelect('')}
        placement="right"
        width={isCompact ? '92vw' : 540}
        title={selected ? <code>{selected.trace_id}</code> : undefined}
        description={selected?.service_name}
      >
        {selected && (
          <Space direction="vertical" size="md">
            <Space size="sm" align="center" wrap>
              <Badge tone={statusTone(selected.status)} size="sm">{selected.status}</Badge>
              <Badge tone="subtle" size="sm">{selected.span_count} spans</Badge>
              <Badge tone="subtle" size="sm">{selected.duration_ms?.toFixed(1)} ms</Badge>
            </Space>

            <Card bordered padding="sm" radius="md" title="Span Waterfall">
              <Space direction="vertical" size="sm">
                {(selected.spans ?? []).map((span) => {
                  const widthPct = Math.min(100, Math.max(6, (span.duration / totalUs) * 100))
                  return (
                    <Card key={span.id} bordered padding="sm" radius="sm">
                      <Space direction="vertical" size="xs">
                        <Space justify="between" align="center">
                          <strong>{span.operation_name}</strong>
                          <Badge tone="subtle" size="sm">{(span.duration / 1000).toFixed(1)} ms</Badge>
                        </Space>
                        <Progress value={widthPct} tone="neutral" />
                        <code>{span.service_name}</code>
                      </Space>
                    </Card>
                  )
                })}
                {(selected.spans ?? []).length === 0 && (
                  <Alert severity="info">No spans recorded for this trace.</Alert>
                )}
              </Space>
            </Card>
          </Space>
        )}
      </Drawer>
    </Space>
  )
}

export default React.memo(TracesView)
