import { useEffect, useRef, useState } from 'react'
import { FixedSizeList, type ListChildComponentProps } from 'react-window'
import {
  Alert,
  Badge,
  Button,
  Card,
  Grid,
  IconButton,
  Progress,
  ScrollDiv,
  Space,
  Spin,
} from '@ossrandom/design-system'
import { X } from 'lucide-react'
import type { Trace } from '@/types/api'

interface Props {
  traces: Trace[]
  selected: Trace | null
  loading: boolean
  error: string | null
  onSelect: (traceId: string) => void
  serviceFilter: string | null
  onClearFilter: () => void
}

const ITEM_SIZE = 112

interface RowData {
  traces: Trace[]
  selectedId: string | undefined
  onSelect: (traceId: string) => void
}

function statusTone(status: string): 'danger' | 'info' {
  return status.includes('ERROR') ? 'danger' : 'info'
}

function TraceRow({ index, style, data }: ListChildComponentProps<RowData>) {
  const trace = data.traces[index]
  const isSelected = data.selectedId === trace.trace_id
  return (
    <div style={style}>
      <Button
        variant={isSelected ? 'secondary' : 'ghost'}
        block
        onClick={() => data.onSelect(trace.trace_id)}
      >
        <Space direction="vertical" size="xs" align="start">
          <Space justify="between" align="center">
            <strong>{trace.service_name}</strong>
            <Badge tone={statusTone(trace.status)} size="sm">{trace.status || 'OK'}</Badge>
          </Space>
          <code>{trace.operation || trace.trace_id}</code>
          <Space size="xs">
            <Badge tone="neutral" size="sm">{trace.span_count} spans</Badge>
            <Badge tone="subtle" size="sm">{trace.duration_ms?.toFixed(1)} ms</Badge>
          </Space>
        </Space>
      </Button>
    </div>
  )
}

export default function TracesPage({ traces, selected, loading, error, onSelect, serviceFilter, onClearFilter }: Props) {
  const filtered = serviceFilter ? traces.filter((t) => t.service_name === serviceFilter) : traces

  const listContainerRef = useRef<HTMLDivElement | null>(null)
  const [size, setSize] = useState<{ width: number; height: number }>({ width: 0, height: 0 })

  useEffect(() => {
    const el = listContainerRef.current
    if (!el) return
    const observer = new ResizeObserver((entries) => {
      for (const entry of entries) {
        const { width, height } = entry.contentRect
        setSize({ width, height })
      }
    })
    observer.observe(el)
    return () => observer.disconnect()
  }, [])

  const totalUs = Math.max(selected?.duration || 1, 1)

  return (
    <Grid columns={12} gap="md">
      <Grid.Col span={4}>
        <Card
          bordered
          padding="md"
          radius="md"
          title="Traces"
          subtitle="Recent distributed requests"
        >
          <Space direction="vertical" size="sm">
            {serviceFilter && (
              <Space justify="between" align="center">
                <Badge tone="info" size="sm">Filtered: {serviceFilter}</Badge>
                <IconButton
                  icon={<X size={11} />}
                  aria-label="Clear filter"
                  variant="ghost"
                  size="xs"
                  onClick={onClearFilter}
                />
              </Space>
            )}
            {loading && <Spin label="Loading traces" />}
            {error && <Alert severity="danger">{error}</Alert>}
            <div ref={listContainerRef}>
              {size.height > 0 && filtered.length > 0 && (
                <FixedSizeList<RowData>
                  height={size.height}
                  width={size.width}
                  itemCount={filtered.length}
                  itemSize={ITEM_SIZE}
                  itemData={{ traces: filtered, selectedId: selected?.trace_id, onSelect }}
                  overscanCount={6}
                >
                  {TraceRow}
                </FixedSizeList>
              )}
              {!loading && filtered.length === 0 && (
                <Alert severity="info">No traces yet.</Alert>
              )}
            </div>
          </Space>
        </Card>
      </Grid.Col>

      <Grid.Col span={8}>
        <Space direction="vertical" size="md">
          <Card
            bordered
            padding="md"
            radius="md"
            title={selected ? <code>{selected.trace_id}</code> : 'No trace selected'}
            subtitle={selected?.service_name}
            extra={selected ? <Badge tone={statusTone(selected.status)} size="sm">{selected.status}</Badge> : undefined}
          />

          <Card bordered padding="md" radius="md" title="Span Waterfall">
            <ScrollDiv maxHeight={520} thin>
              <Space direction="vertical" size="sm">
                {(selected?.spans ?? []).map((span) => {
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
                {selected && (selected.spans ?? []).length === 0 && (
                  <Alert severity="info">No spans recorded for this trace.</Alert>
                )}
              </Space>
            </ScrollDiv>
          </Card>
        </Space>
      </Grid.Col>
    </Grid>
  )
}
