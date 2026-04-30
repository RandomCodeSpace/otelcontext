import React, { useMemo, useState } from 'react'
import {
  Alert,
  Badge,
  Button,
  Card,
  Drawer,
  IconButton,
  Input,
  PageHeader,
  Space,
  Table,
  type TableColumn,
} from '@ossrandom/design-system'
import { Search, Sparkles, X } from 'lucide-react'
import type { DashboardStats, LogEntry } from '@/types/api'
import { fmt } from '../../lib/utils'
import StatRow from './StatRow'
import { useMediaQuery } from '../../hooks/useMediaQuery'

interface Props {
  logs: LogEntry[]
  similar: LogEntry[]
  loading: boolean
  error: string | null
  onSimilar: (query: string) => void
  serviceFilter: string | null
  onClearFilter: () => void
  dashboard: DashboardStats | null
}

function severityTone(severity: string): 'danger' | 'warning' | 'info' {
  if (severity === 'ERROR') return 'danger'
  if (severity === 'WARN') return 'warning'
  return 'info'
}

const SEVERITIES: { value: string; label: string }[] = [
  { value: '', label: 'all' },
  { value: 'INFO', label: 'info' },
  { value: 'WARN', label: 'warn' },
  { value: 'ERROR', label: 'error' },
]

const LogsView: React.FC<Props> = ({
  logs,
  similar,
  loading: _loading,
  error,
  onSimilar,
  serviceFilter,
  onClearFilter,
  dashboard: _dashboard,
}) => {
  const [query, setQuery] = useState('')
  const [severity, setSeverity] = useState('')
  const [drawerOpen, setDrawerOpen] = useState(false)
  const isCompact = useMediaQuery('(max-width: 760px)')

  const filtered = useMemo(() => {
    let result = logs
    if (serviceFilter) result = result.filter((l) => l.service_name === serviceFilter)
    if (severity) result = result.filter((l) => l.severity === severity)
    return result
  }, [logs, severity, serviceFilter])

  const counts = useMemo(() => {
    let info = 0
    let warn = 0
    let err = 0
    for (const log of filtered) {
      if (log.severity === 'ERROR') err++
      else if (log.severity === 'WARN') warn++
      else info++
    }
    return { info, warn, err }
  }, [filtered])

  const runSimilar = () => {
    if (!query.trim()) return
    onSimilar(query)
    setDrawerOpen(true)
  }

  const columns: TableColumn<LogEntry>[] = [
    {
      key: 'severity',
      title: 'Level',
      width: 84,
      render: (_v, row) => <Badge tone={severityTone(row.severity)} size="sm">{row.severity}</Badge>,
    },
    {
      key: 'timestamp',
      title: 'Time',
      width: 100,
      render: (_v, row) => <code>{new Date(row.timestamp).toLocaleTimeString()}</code>,
    },
    {
      key: 'service_name',
      title: 'Service',
      dataKey: 'service_name',
      width: 160,
      sortable: true,
    },
    {
      key: 'body',
      title: 'Message',
      render: (_v, row) => <code>{row.body}</code>,
    },
  ]

  return (
    <Space direction="vertical" size="md">
      <PageHeader
        size="sm"
        title="Logs"
        subtitle="Live tail · filter by severity · find similar incidents"
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
          { label: 'Errors', value: counts.err },
          { label: 'Warnings', value: counts.warn },
          { label: 'Info', value: counts.info },
        ]}
      />

      <Card bordered padding="sm" radius="md">
        <Space direction="vertical" size="sm">
          <Space size="sm" align="center" wrap>
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
            <Input
              value={query}
              onChange={(value) => setQuery(value)}
              placeholder="Search similar"
              size="sm"
              prefix={<Search size={12} />}
              onKeyDown={(e) => {
                if (e.key === 'Enter') runSimilar()
              }}
            />
            <Button
              variant="primary"
              size="sm"
              iconLeft={<Sparkles size={12} />}
              disabled={!query.trim()}
              onClick={runSimilar}
            >
              Find similar
            </Button>
          </Space>

          {error && <Alert severity="danger">{error}</Alert>}
          {filtered.length === 0 && <Alert severity="info">No logs in view.</Alert>}
          {filtered.length > 0 && (
            <Table<LogEntry>
              columns={columns}
              data={filtered}
              rowKey="id"
              density="compact"
              stickyHeader
              striped
            />
          )}
        </Space>
      </Card>

      <Drawer
        open={drawerOpen}
        onClose={() => setDrawerOpen(false)}
        placement="right"
        width={isCompact ? '92vw' : 460}
        title="Similar logs"
        description={query.trim() ? `Matches for "${query.trim()}"` : undefined}
      >
        <Space direction="vertical" size="sm">
          {similar.length === 0 && <Alert severity="info">No similar logs found.</Alert>}
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
        </Space>
      </Drawer>
    </Space>
  )
}

export default React.memo(LogsView)
