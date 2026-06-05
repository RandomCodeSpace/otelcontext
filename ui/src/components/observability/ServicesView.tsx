import React, { useCallback, useMemo, useState } from 'react'
import {
  Alert,
  Card,
  Drawer,
  Input,
  PageHeader,
  Space,
  Spin,
  Tabs,
} from '@ossrandom/design-system'
import { Search } from 'lucide-react'
import ServiceGraph from './ServiceGraph'
import ServiceList from './ServiceList'
import ServiceSidePanel from './ServiceSidePanel'
import StatRow from './StatRow'
import type { SystemNode } from '../../types/api'
import { fmt } from '../../lib/utils'
import { BP } from '../../lib/breakpoints'
import { useMediaQuery } from '../../hooks/useMediaQuery'
import { useWindowHeight } from '../../hooks/useWindowHeight'
import { useSystemGraph } from '../../hooks/useSystemGraph'
import { useDashboard } from '../../hooks/useDashboard'

type NodeStatus = 'healthy' | 'degraded' | 'failing' | 'unknown'

function toNodeStatus(status: string | undefined): NodeStatus {
  if (status === 'healthy' || status === 'degraded') return status
  if (status === 'critical' || status === 'failing') return 'failing'
  return 'unknown'
}

function isEdgeFailing(status: string | undefined): boolean {
  return status === 'critical' || status === 'failing'
}

const ServicesView: React.FC = () => {
  const { graph, loading, error } = useSystemGraph()
  const { dashboard, stats } = useDashboard()
  const [selectedNode, setSelectedNode] = useState<SystemNode | null>(null)
  const [search, setSearch] = useState('')
  const isCompact = useMediaQuery(BP.coarse)
  // The interactive cytoscape graph needs hover + a wide canvas, so small/touch
  // screens default to the accessible list. `viewMode` (null = auto) lets the
  // user force either rendering on any device via the header toggle.
  const [viewMode, setViewMode] = useState<'graph' | 'list' | null>(null)
  const showList = viewMode ? viewMode === 'list' : isCompact
  const windowH = useWindowHeight()
  // Subtract chrome above the canvas (TopNav + PageHeader + StatRow + Card
  // padding + Space gaps + breathing margin). 460px floor so a very short
  // window still shows a usable canvas.
  const mapHeight = isCompact ? 460 : Math.max(460, windowH - 320)

  const nodes = graph?.nodes ?? []
  const edges = graph?.edges ?? []

  // Search filters the node set; edges to/from dropped nodes are filtered
  // inside ServiceGraph (it only keeps edges whose endpoints are present).
  const filteredNodes = useMemo<SystemNode[]>(() => {
    const q = search.trim().toLowerCase()
    if (!q) return nodes
    return nodes.filter((n) => n.id.toLowerCase().includes(q))
  }, [nodes, search])

  const totalServices = dashboard?.active_services ?? nodes.length
  const errorRate = dashboard?.error_rate ?? 0
  const s = stats as Record<string, unknown> | null
  const num = (v: unknown): number | undefined => {
    if (typeof v === 'number') return v
    if (typeof v === 'string' && v.trim() !== '' && Number.isFinite(Number(v))) return Number(v)
    return undefined
  }
  const totalTraces = num(s?.TraceCount) ?? num(s?.traceCount) ?? dashboard?.total_traces ?? 0
  const totalLogs = num(s?.LogCount) ?? num(s?.logCount) ?? dashboard?.total_logs ?? 0
  const dbMb = num(s?.DBSizeMB) ?? num(s?.db_size_mb)

  const handleSelectService = useCallback(
    (id: string) => {
      const match = nodes.find((n) => n.id === id)
      if (match) setSelectedNode(match)
    },
    [nodes],
  )

  const handleClose = useCallback(() => setSelectedNode(null), [])

  return (
    <Space direction="vertical" size="md" style={{ display: 'flex', width: '100%' }}>
      <PageHeader
        size="sm"
        title="Service Topology"
        subtitle="Live dependency map · click a node for details"
        inlineSubtitle
      />

      <StatRow
        items={[
          { label: 'Services', value: totalServices },
          { label: 'Error rate', value: errorRate.toFixed(2), unit: '%' },
          { label: 'Traces', value: fmt(totalTraces) },
          { label: 'Logs', value: fmt(totalLogs) },
          ...(dbMb != null && Number.isFinite(dbMb) ? [{ label: 'DB', value: dbMb.toFixed(0), unit: 'MB' }] : []),
        ]}
      />

      <Card
        bordered
        padding="sm"
        radius="md"
        extra={
          <Space size="sm" align="center">
            <Tabs
              items={[
                { key: 'graph', label: 'Graph' },
                { key: 'list', label: 'List' },
              ]}
              value={showList ? 'list' : 'graph'}
              onChange={(k) => setViewMode(k as 'graph' | 'list')}
              variant="segment"
              size="sm"
            />
            <Input
              value={search}
              onChange={(value) => setSearch(value)}
              placeholder="Filter services"
              size="sm"
              prefix={<Search size={12} />}
            />
          </Space>
        }
      >
        {loading && <Spin label="Loading service map" />}
        {error && (
          <Alert severity="danger" title="Service map failed to load">
            {error}
          </Alert>
        )}
        {!loading && !error && nodes.length === 0 && (
          <Alert severity="info">No services discovered yet.</Alert>
        )}
        {!loading && !error && filteredNodes.length === 0 && nodes.length > 0 && (
          <Alert severity="info">No services match the filter.</Alert>
        )}
        {!loading && !error && filteredNodes.length > 0 && (
          showList ? (
            <ServiceList
              nodes={filteredNodes}
              toNodeStatus={toNodeStatus}
              onSelect={handleSelectService}
              selectedId={selectedNode?.id}
            />
          ) : (
            <ServiceGraph
              nodes={filteredNodes}
              edges={edges}
              toNodeStatus={toNodeStatus}
              isEdgeFailing={isEdgeFailing}
              height={mapHeight}
              onSelect={handleSelectService}
            />
          )
        )}
      </Card>

      <Drawer
        open={selectedNode !== null}
        onClose={handleClose}
        placement="right"
        width={isCompact ? '92vw' : 420}
        title={selectedNode ? <code>{selectedNode.id}</code> : undefined}
        description="Service detail · upstream, downstream, alerts"
      >
        {selectedNode && (
          <ServiceSidePanel
            node={selectedNode}
            edges={edges}
            onClose={handleClose}
            onSelectService={handleSelectService}
          />
        )}
      </Drawer>
    </Space>
  )
}

export default React.memo(ServicesView)
