import React from 'react'
import { Alert, Badge, Button, Card, Grid, IconButton, Progress, Space, Stat } from '@ossrandom/design-system'
import { ArrowRight, X } from 'lucide-react'
import type { SystemNode, SystemEdge } from '../../types/api'

interface ServiceSidePanelProps {
  node: SystemNode
  edges: SystemEdge[]
  onClose: () => void
  onSelectService: (id: string) => void
  onViewTraces: (service: string) => void
  onViewLogs: (service: string) => void
}

function statusTone(status: string): 'info' | 'warning' | 'danger' | 'neutral' {
  if (status === 'healthy') return 'info'
  if (status === 'degraded') return 'warning'
  if (status === 'critical' || status === 'failing') return 'danger'
  return 'neutral'
}

const ServiceSidePanel: React.FC<ServiceSidePanelProps> = ({
  node,
  edges,
  onClose,
  onSelectService,
  onViewTraces,
  onViewLogs,
}) => {
  const upstream = edges.filter((e) => e.target === node.id)
  const downstream = edges.filter((e) => e.source === node.id)
  const errorRatePercent = node.metrics.error_rate * 100
  const isHighError = node.metrics.error_rate > 0.05

  return (
    <Space direction="vertical" size="md">
      <Card
        bordered
        padding="md"
        radius="md"
        title={
          <Space size="xs" align="center">
            <code>{node.id}</code>
            <Badge tone={statusTone(node.status)} size="sm">{node.status}</Badge>
          </Space>
        }
        extra={
          <IconButton icon={<X size={13} />} aria-label="Close" variant="ghost" size="sm" onClick={onClose} />
        }
      >
        <Grid columns={2} gap="sm">
          <Grid.Col span={1}>
            <Stat label="RPS" value={Math.round(node.metrics.request_rate_rps)} />
          </Grid.Col>
          <Grid.Col span={1}>
            <Stat
              label="Error Rate"
              value={errorRatePercent.toFixed(1)}
              unit="%"
              delta={isHighError ? { value: errorRatePercent, direction: 'up', tone: 'bad' } : undefined}
            />
          </Grid.Col>
          <Grid.Col span={1}>
            <Stat label="Avg Latency" value={node.metrics.avg_latency_ms} unit="ms" />
          </Grid.Col>
          <Grid.Col span={1}>
            <Stat label="P99" value={node.metrics.p99_latency_ms} unit="ms" />
          </Grid.Col>
        </Grid>
      </Card>

      <Card bordered padding="md" radius="md" title="Health Score" extra={<Badge tone="subtle" size="sm">{node.health_score.toFixed(2)}</Badge>}>
        <Progress
          value={node.health_score * 100}
          tone={node.health_score < 0.4 ? 'danger' : node.health_score < 0.7 ? 'warning' : 'neutral'}
        />
      </Card>

      {upstream.length > 0 && (
        <Card bordered padding="md" radius="md" title="Upstream">
          <Space direction="vertical" size="xs">
            {upstream.map((edge) => (
              <Button
                key={edge.source}
                variant="ghost"
                size="sm"
                block
                onClick={() => onSelectService(edge.source)}
              >
                <Space justify="between" align="center">
                  <code>{edge.source}</code>
                  <Badge tone="subtle" size="sm">{edge.call_count} calls</Badge>
                </Space>
              </Button>
            ))}
          </Space>
        </Card>
      )}

      {downstream.length > 0 && (
        <Card bordered padding="md" radius="md" title="Downstream">
          <Space direction="vertical" size="xs">
            {downstream.map((edge) => (
              <Button
                key={edge.target}
                variant="ghost"
                size="sm"
                block
                onClick={() => onSelectService(edge.target)}
              >
                <Space justify="between" align="center">
                  <code>{edge.target}</code>
                  <Badge tone="subtle" size="sm">{edge.call_count} calls</Badge>
                </Space>
              </Button>
            ))}
          </Space>
        </Card>
      )}

      {node.alerts.length > 0 && (
        <Space direction="vertical" size="xs">
          {node.alerts.map((alert, i) => (
            <Alert key={i} severity="danger">
              {alert}
            </Alert>
          ))}
        </Space>
      )}

      <Space size="xs">
        <Button
          variant="secondary"
          size="sm"
          block
          iconRight={<ArrowRight size={11} />}
          onClick={() => onViewTraces(node.id)}
        >
          Traces
        </Button>
        <Button
          variant="secondary"
          size="sm"
          block
          iconRight={<ArrowRight size={11} />}
          onClick={() => onViewLogs(node.id)}
        >
          Logs
        </Button>
      </Space>
    </Space>
  )
}

export default React.memo(ServiceSidePanel)
