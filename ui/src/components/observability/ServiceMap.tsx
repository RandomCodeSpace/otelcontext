import React, { useMemo, useState } from 'react'
import { Alert, Badge, Card, Grid, Input, Space, Spin } from '@ossrandom/design-system'
import { ServiceMap as DSServiceMap } from '@ossrandom/design-system/charts'
import type { ServiceNode as DSNode, ServiceEdge as DSEdge } from '@ossrandom/design-system/charts'
import { Search } from 'lucide-react'
import ServiceSidePanel from './ServiceSidePanel'
import type { SystemGraphResponse, SystemNode } from '../../types/api'

interface ServiceMapProps {
  graph: SystemGraphResponse | null
  cache: string
  loading: boolean
  error: string | null
  onNavigateToTraces: (service: string) => void
  onNavigateToLogs: (service: string) => void
}

function toNodeStatus(status: string | undefined): DSNode['status'] {
  if (status === 'healthy' || status === 'degraded') return status
  if (status === 'critical' || status === 'failing') return 'failing'
  return 'unknown'
}

function toEdgeStatus(status: string | undefined): DSEdge['status'] {
  return status === 'critical' || status === 'failing' ? 'failing' : 'healthy'
}

const ServiceMap: React.FC<ServiceMapProps> = ({
  graph,
  cache: _cache,
  loading,
  error,
  onNavigateToTraces,
  onNavigateToLogs,
}) => {
  const [selectedNode, setSelectedNode] = useState<SystemNode | null>(null)
  const [search, setSearch] = useState('')

  const nodes = graph?.nodes ?? []
  const edges = graph?.edges ?? []

  const dsNodes = useMemo<DSNode[]>(() => {
    const q = search.trim().toLowerCase()
    return nodes
      .filter((n) => !q || n.id.toLowerCase().includes(q))
      .map((n) => ({
        id: n.id,
        label: n.id,
        status: toNodeStatus(n.status),
      }))
  }, [nodes, search])

  const dsEdges = useMemo<DSEdge[]>(() => {
    if (dsNodes.length === 0) return []
    const allowed = new Set(dsNodes.map((n) => n.id))
    return edges
      .filter((e) => allowed.has(e.source) && allowed.has(e.target))
      .slice(0, 500)
      .map((e) => ({
        source: e.source,
        target: e.target,
        status: toEdgeStatus(e.status),
      }))
  }, [edges, dsNodes])

  const handleNodeClick = (node: DSNode) => {
    const match = nodes.find((n) => n.id === node.id)
    setSelectedNode(match ?? null)
  }

  const handleSelectService = (id: string) => {
    const match = nodes.find((n) => n.id === id)
    if (match) setSelectedNode(match)
  }

  if (loading) {
    return (
      <Card bordered padding="lg" radius="md">
        <Spin label="Loading service map" />
      </Card>
    )
  }

  if (error) {
    return (
      <Card bordered padding="md" radius="md">
        <Alert severity="danger" title="Service map failed to load">
          {error}
        </Alert>
      </Card>
    )
  }

  if (!graph || nodes.length === 0) {
    return (
      <Card bordered padding="md" radius="md">
        <Alert severity="info">No services discovered yet.</Alert>
      </Card>
    )
  }

  const toolbar = (
    <Space justify="between" align="center">
      <Input
        value={search}
        onChange={(value) => setSearch(value)}
        placeholder="Filter services"
        size="sm"
        prefix={<Search size={12} />}
      />
      <Space size="xs">
        <Badge tone="subtle" size="sm">{dsNodes.length} of {nodes.length} services</Badge>
        <Badge tone="subtle" size="sm">{dsEdges.length} calls</Badge>
      </Space>
    </Space>
  )

  return (
    <Grid columns={12} gap="md">
      <Grid.Col span={selectedNode ? 8 : 12}>
        <Card bordered padding="md" radius="md" title="Service Map" extra={toolbar}>
          {dsNodes.length === 0 ? (
            <Alert severity="info">No services match the filter.</Alert>
          ) : (
            <DSServiceMap
              nodes={dsNodes}
              edges={dsEdges}
              layout="cose-bilkent"
              height={560}
              onNodeClick={handleNodeClick}
            />
          )}
        </Card>
      </Grid.Col>

      {selectedNode && (
        <Grid.Col span={4}>
          <ServiceSidePanel
            node={selectedNode}
            edges={edges}
            onClose={() => setSelectedNode(null)}
            onSelectService={handleSelectService}
            onViewTraces={onNavigateToTraces}
            onViewLogs={onNavigateToLogs}
          />
        </Grid.Col>
      )}
    </Grid>
  )
}

export default React.memo(ServiceMap)
