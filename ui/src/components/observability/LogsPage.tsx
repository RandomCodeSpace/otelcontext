import { useEffect, useMemo, useRef, useState } from 'react'
import { VariableSizeList, type ListChildComponentProps } from 'react-window'
import {
  Alert,
  Badge,
  Button,
  Card,
  Grid,
  IconButton,
  Input,
  ScrollDiv,
  Space,
} from '@ossrandom/design-system'
import { Search, X } from 'lucide-react'
import type { LogEntry } from '@/types/api'

interface Props {
  logs: LogEntry[]
  similar: LogEntry[]
  loading: boolean
  error: string | null
  onSimilar: (query: string) => void
  serviceFilter: string | null
  onClearFilter: () => void
}

const LOG_BASE_HEIGHT = 62
const LOG_LINE_HEIGHT = 19
const LOG_CHARS_PER_LINE = 80
const LOG_GAP = 9

function estimateLogHeight(body: string): number {
  const len = body ? body.length : 0
  const lines = Math.max(1, Math.ceil(len / LOG_CHARS_PER_LINE))
  return LOG_BASE_HEIGHT + lines * LOG_LINE_HEIGHT + LOG_GAP
}

interface RowData {
  logs: LogEntry[]
}

function severityTone(severity: string): 'danger' | 'warning' | 'info' {
  if (severity === 'ERROR') return 'danger'
  if (severity === 'WARN') return 'warning'
  return 'info'
}

function LogRow({ index, style, data }: ListChildComponentProps<RowData>) {
  const log = data.logs[index]
  return (
    <div style={style}>
      <Card bordered padding="sm" radius="md">
        <Space direction="vertical" size="xs">
          <Space justify="between" align="center">
            <Space size="xs" align="center">
              <Badge tone={severityTone(log.severity)} size="sm">{log.severity}</Badge>
              <span>{log.service_name}</span>
            </Space>
            <code>{new Date(log.timestamp).toLocaleTimeString()}</code>
          </Space>
          <code>{log.body}</code>
        </Space>
      </Card>
    </div>
  )
}

const SEVERITIES: { value: string; label: string }[] = [
  { value: '', label: 'all' },
  { value: 'INFO', label: 'info' },
  { value: 'WARN', label: 'warn' },
  { value: 'ERROR', label: 'error' },
]

export default function LogsPage({ logs, similar, loading, error, onSimilar, serviceFilter, onClearFilter }: Props) {
  const [query, setQuery] = useState('')
  const [severity, setSeverity] = useState('')

  const filtered = useMemo(() => {
    let result = logs
    if (serviceFilter) {
      result = result.filter((log) => log.service_name === serviceFilter)
    }
    if (severity) {
      result = result.filter((log) => log.severity === severity)
    }
    return result
  }, [logs, severity, serviceFilter])

  const streamContainerRef = useRef<HTMLDivElement | null>(null)
  const [streamSize, setStreamSize] = useState<{ width: number; height: number }>({ width: 0, height: 0 })
  const listRef = useRef<VariableSizeList<RowData> | null>(null)

  useEffect(() => {
    const el = streamContainerRef.current
    if (!el) return
    const observer = new ResizeObserver((entries) => {
      for (const entry of entries) {
        const { width, height } = entry.contentRect
        setStreamSize({ width, height })
      }
    })
    observer.observe(el)
    return () => observer.disconnect()
  }, [])

  useEffect(() => {
    listRef.current?.resetAfterIndex(0)
  }, [filtered, streamSize.width])

  const getItemSize = (index: number): number => estimateLogHeight(filtered[index]?.body ?? '')

  return (
    <Grid columns={12} gap="md">
      <Grid.Col span={4}>
        <Card
          bordered
          padding="md"
          radius="md"
          title="Live Log Search"
          subtitle="Tail, filter, and query similar incidents"
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
            <Input
              value={query}
              onChange={(value) => setQuery(value)}
              placeholder="Find similar logs"
              size="sm"
              prefix={<Search size={12} />}
            />
            <Space size="xs" wrap>
              {SEVERITIES.map((item) => (
                <Button
                  key={item.value || 'all'}
                  variant={severity === item.value ? 'secondary' : 'ghost'}
                  size="sm"
                  onClick={() => setSeverity(item.value)}
                >
                  {item.label}
                </Button>
              ))}
            </Space>
            <Button variant="primary" block disabled={!query.trim()} onClick={() => onSimilar(query)}>
              Run Similarity Search
            </Button>
            <ScrollDiv maxHeight={420} thin>
              <Space direction="vertical" size="xs">
                {similar.map((log) => (
                  <Card key={`similar-${log.id}`} bordered padding="sm" radius="md">
                    <Space direction="vertical" size="xs">
                      <Space justify="between" align="center">
                        <strong>{log.service_name}</strong>
                        <Badge tone={severityTone(log.severity)} size="sm">{log.severity}</Badge>
                      </Space>
                      <code>{log.body}</code>
                    </Space>
                  </Card>
                ))}
                {similar.length === 0 && query.trim() && (
                  <Alert severity="info">No similar logs yet — run search.</Alert>
                )}
              </Space>
            </ScrollDiv>
          </Space>
        </Card>
      </Grid.Col>

      <Grid.Col span={8}>
        <Card
          bordered
          padding="md"
          radius="md"
          title="Stream"
          extra={loading ? <Badge tone="subtle" size="sm">Loading</Badge> : undefined}
        >
          <Space direction="vertical" size="sm">
            {error && <Alert severity="danger">{error}</Alert>}
            <div ref={streamContainerRef}>
              {streamSize.height > 0 && filtered.length > 0 && (
                <VariableSizeList<RowData>
                  ref={listRef}
                  height={streamSize.height}
                  width={streamSize.width}
                  itemCount={filtered.length}
                  itemSize={getItemSize}
                  estimatedItemSize={90}
                  itemData={{ logs: filtered }}
                  overscanCount={6}
                >
                  {LogRow}
                </VariableSizeList>
              )}
              {!loading && filtered.length === 0 && (
                <Alert severity="info">No logs yet.</Alert>
              )}
            </div>
          </Space>
        </Card>
      </Grid.Col>
    </Grid>
  )
}
