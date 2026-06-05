import { useMemo } from 'react'
import {
  Alert,
  Badge,
  Card,
  Divider,
  Grid,
  PageHeader,
  Skeleton,
  Space,
  Stat,
  StatusDot,
  Table,
  Timeline,
} from '@ossrandom/design-system'
import type { TableColumn, TimelineItem } from '@ossrandom/design-system'
import { RadialGauge } from '@ossrandom/design-system/charts'
import Truncate from '../common/Truncate'
import type { OtelView } from '../nav/TopNav'
import type { ServiceError } from '../../types/api'
import { fmt } from '../../lib/utils'
import { BP } from '../../lib/breakpoints'
import { useMediaQuery } from '../../hooks/useMediaQuery'
import { useSystemGraph } from '../../hooks/useSystemGraph'
import { useDashboard } from '../../hooks/useDashboard'
import { useTraffic } from '../../hooks/useTraffic'
import { useReady } from '../../hooks/useReady'
import { useAnomalies } from '../../hooks/useAnomalies'
import {
  bucketErrorRatePct,
  coerceDbSizeMb,
  formatUptime,
  healthPct,
  readyCheckToStatus,
  severityToTone,
} from './dashTypes'

interface DashboardViewProps {
  onNavigate: (view: OtelView) => void
}

// Friendly labels for the /ready probe keys.
const READY_LABELS: Record<string, string> = {
  database: 'Database',
  graphrag: 'GraphRAG',
  dlq_disk: 'DLQ disk',
  pipeline: 'Ingest pipeline',
}

// Health hue for the gauge: green when mostly healthy, amber mid, red low.
function healthTone(pct: number): 'good' | 'warning' | 'bad' {
  if (pct >= 90) return 'good'
  if (pct >= 70) return 'warning'
  return 'bad'
}

export default function DashboardView({
  onNavigate,
}: Readonly<DashboardViewProps>) {
  const { graph, loading: graphLoading, error: graphError } = useSystemGraph()
  const {
    dashboard,
    stats,
    loading: dashLoading,
    error: dashError,
  } = useDashboard()
  const { points, error: trafficError } = useTraffic()
  const { ready, error: readyError } = useReady()
  const { anomalies, error: anomalyError } = useAnomalies()

  const summary = graph?.system ?? null

  // Responsive tiers. The DS Grid has no per-breakpoint Col prop, so spans are
  // computed here and passed to <Grid.Col span={...}>.
  const isCoarse = useMediaQuery(BP.coarse)
  const isTablet = useMediaQuery(BP.tablet)
  const isWide = useMediaQuery(BP.wide)

  // 12-col band spans per tier (coarse = stack, tablet = halves, desktop = bento).
  const span = useMemo(() => {
    if (isCoarse) {
      return { hero: 12, traffic: 12, failing: 12, anomalies: 12, platform: 12 }
    }
    if (isTablet) {
      return { hero: 12, traffic: 12, failing: 12, anomalies: 6, platform: 6 }
    }
    return { hero: 12, traffic: 12, failing: 8, anomalies: 4, platform: 12 }
  }, [isCoarse, isTablet])

  // Cold-start gate: GraphRAG rebuilds topology from the DB on a ~60s loop, so
  // total_services stays 0 for up to a minute after a fresh boot. Show skeletons
  // (not a bare Spin) until first data resolves AND the graph is warm.
  const warming =
    (graphLoading && !graph) ||
    (dashLoading && !dashboard) ||
    !summary ||
    summary.total_services === 0

  const fetchError = graphError || dashError || trafficError || readyError

  // ---- Derived display values ------------------------------------------------
  const pct = healthPct(summary?.overall_health_score)

  const trafficCounts = useMemo(
    () => (points ?? []).map((p) => p.count),
    [points],
  )
  const errorRateSeries = useMemo(
    () => (points ?? []).map(bucketErrorRatePct),
    [points],
  )
  const totalRequests = useMemo(
    () => trafficCounts.reduce((a, b) => a + b, 0),
    [trafficCounts],
  )
  // Window-aggregate error rate — steadier than any single bucket for the badge.
  const windowErrorPct = useMemo(() => {
    const totalErr = (points ?? []).reduce((a, p) => a + p.error_count, 0)
    return totalRequests > 0 ? (totalErr / totalRequests) * 100 : 0
  }, [points, totalRequests])

  const dbSizeMb = coerceDbSizeMb(stats?.db_size_mb)

  const containerStyle = isWide
    ? { maxWidth: 1280, margin: '0 auto', width: '100%' }
    : { width: '100%' }

  // ---- Top failing services table -------------------------------------------
  const failingColumns: readonly TableColumn<ServiceError>[] = [
    {
      key: 'service_name',
      title: 'Service',
      dataKey: 'service_name',
      render: (_v, row) => <Truncate text={row.service_name} />,
    },
    {
      key: 'error_count',
      title: 'Errors',
      dataKey: 'error_count',
      width: 90,
      align: 'right',
      render: (_v, row) => fmt(row.error_count),
    },
    {
      key: 'total_count',
      title: 'Total',
      dataKey: 'total_count',
      width: 90,
      align: 'right',
      render: (_v, row) => fmt(row.total_count),
    },
    {
      key: 'error_rate',
      title: 'Error rate',
      dataKey: 'error_rate',
      width: 110,
      align: 'right',
      render: (_v, row) => {
        const ratePct =
          row.error_rate <= 1 ? row.error_rate * 100 : row.error_rate
        return (
          <Badge tone={ratePct >= 5 ? 'danger' : 'neutral'} size="sm">
            {ratePct.toFixed(1)}%
          </Badge>
        )
      },
    },
  ]

  const failing = dashboard?.top_failing_services ?? []

  // ---- Anomaly timeline items -----------------------------------------------
  const anomalyItems: readonly TimelineItem[] = (anomalies ?? []).map((a) => ({
    key: a.id,
    title: a.service || a.type,
    description: a.evidence,
    time: new Date(a.timestamp).toLocaleTimeString(),
    tone: severityToTone(a.severity),
  }))

  // ---- Loading state ---------------------------------------------------------
  if (warming) {
    return (
      <Space
        direction="vertical"
        size="md"
        style={{ display: 'flex', ...containerStyle, padding: '0.75rem' }}
      >
        <PageHeader
          title="Dashboard"
          subtitle="Warming up — building service graph…"
        />
        <Grid columns={12} gap="md">
          <Grid.Col span={12}>
            <Card bordered padding="md" radius="md">
              <Skeleton variant="rect" height={96} />
            </Card>
          </Grid.Col>
          <Grid.Col span={isCoarse ? 12 : 8}>
            <Card bordered padding="md" radius="md">
              <Skeleton variant="text" lines={6} />
            </Card>
          </Grid.Col>
          <Grid.Col span={isCoarse ? 12 : 4}>
            <Card bordered padding="md" radius="md">
              <Skeleton variant="text" lines={6} />
            </Card>
          </Grid.Col>
        </Grid>
      </Space>
    )
  }

  // ---- Main view -------------------------------------------------------------
  return (
    <Space
      direction="vertical"
      size="md"
      style={{ display: 'flex', ...containerStyle, padding: '0.75rem' }}
    >
      <PageHeader title="Dashboard" subtitle="System health at a glance" />

      {fetchError && (
        <Alert severity="warning" title="Some panels failed to load">
          {fetchError}
        </Alert>
      )}

      <Grid columns={12} gap="md">
        {/* Band 1 — Hero health */}
        <Grid.Col span={span.hero}>
          <Card bordered padding="md" radius="md" title="System health">
            <Space size="lg" align="center" wrap style={{ display: 'flex' }}>
              <RadialGauge
                value={pct}
                max={100}
                size={120}
                label={`${Math.round(pct)}%`}
                tone={healthTone(pct)}
              />
              <Divider direction="vertical" />
              <Stat label="Healthy" value={summary?.healthy ?? 0} />
              <Divider direction="vertical" />
              <Stat label="Degraded" value={summary?.degraded ?? 0} />
              <Divider direction="vertical" />
              <Stat label="Critical" value={summary?.critical ?? 0} />
              <Divider direction="vertical" />
              <Stat
                label="Avg latency"
                value={Math.round(dashboard?.avg_latency_ms ?? 0)}
                unit="ms"
              />
              <Divider direction="vertical" />
              <Stat
                label="p99 latency"
                value={Math.round(dashboard?.p99_latency ?? 0)}
                unit="ms"
              />
            </Space>
          </Card>
        </Grid.Col>

        {/* Band 2 — Traffic + Errors */}
        <Grid.Col span={span.traffic}>
          <Card bordered padding="md" radius="md" title="Traffic">
            <Space size="lg" align="center" wrap style={{ display: 'flex' }}>
              <Stat
                label="Requests (30m)"
                value={fmt(totalRequests)}
                sparkline={trafficCounts.length ? trafficCounts : undefined}
              />
              <Divider direction="vertical" />
              <Stat
                label="Error rate (30m)"
                value={`${windowErrorPct.toFixed(1)}%`}
                sparkline={errorRateSeries.length ? errorRateSeries : undefined}
              />
              <Divider direction="vertical" />
              <Stat
                label="Total traces"
                value={fmt(dashboard?.total_traces ?? 0)}
              />
              <Divider direction="vertical" />
              <Stat label="Total logs" value={fmt(dashboard?.total_logs ?? 0)} />
              <Divider direction="vertical" />
              <div style={{ alignSelf: 'center' }}>
                <Badge
                  tone={windowErrorPct >= 5 ? 'danger' : 'neutral'}
                  size="sm"
                >
                  {windowErrorPct >= 5 ? 'elevated errors' : 'nominal'}
                </Badge>
              </div>
            </Space>
          </Card>
        </Grid.Col>

        {/* Band 3 — Top failing services */}
        <Grid.Col span={span.failing}>
          <Card bordered padding="md" radius="md" title="Top failing services">
            <Table<ServiceError>
              columns={failingColumns}
              data={failing}
              rowKey="service_name"
              density="compact"
              onRowClick={() => onNavigate('services')}
              empty={
                <Space size="xs" align="center" style={{ display: 'flex' }}>
                  <StatusDot status="running" />
                  <span>No failing services — all clear.</span>
                </Space>
              }
            />
          </Card>
        </Grid.Col>

        {/* Band 4 — Recent anomalies */}
        <Grid.Col span={span.anomalies}>
          <Card bordered padding="md" radius="md" title="Recent anomalies">
            {anomalyError ? (
              <Alert severity="warning" title="Anomaly feed unavailable">
                {anomalyError}
              </Alert>
            ) : anomalyItems.length ? (
              <Timeline items={anomalyItems} />
            ) : (
              <Space size="xs" align="center" style={{ display: 'flex' }}>
                <StatusDot status="running" />
                <span>No anomalies in the last hour.</span>
              </Space>
            )}
          </Card>
        </Grid.Col>

        {/* Band 5 — Platform health */}
        <Grid.Col span={span.platform}>
          <Card bordered padding="md" radius="md" title="Platform health">
            <Space size="lg" align="center" wrap style={{ display: 'flex' }}>
              <Stat label="Uptime" value={formatUptime(summary?.uptime_seconds)} />
              <Divider direction="vertical" />
              {ready
                ? Object.entries(ready.checks).map(([key, value]) => (
                    <StatusDot
                      key={key}
                      status={readyCheckToStatus(value)}
                      label={`${READY_LABELS[key] ?? key}: ${value}`}
                    />
                  ))
                : readyError && (
                    <span style={{ minWidth: 0 }}>
                      Readiness probe unavailable.
                    </span>
                  )}
              {dbSizeMb !== null && (
                <>
                  <Divider direction="vertical" />
                  <Stat
                    label="Database size"
                    value={dbSizeMb.toFixed(1)}
                    unit="MB"
                  />
                </>
              )}
            </Space>
          </Card>
        </Grid.Col>
      </Grid>
    </Space>
  )
}
