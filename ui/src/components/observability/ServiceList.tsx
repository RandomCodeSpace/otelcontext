import React, { useMemo } from 'react'
import { Badge, StatusDot, Table } from '@ossrandom/design-system'
import type { TableColumn } from '@ossrandom/design-system'
import type { SystemNode } from '../../types/api'
import Truncate from '../common/Truncate'

type NodeStatus = 'healthy' | 'degraded' | 'failing' | 'unknown'

interface ServiceListProps {
  nodes: SystemNode[]
  toNodeStatus: (status: string | undefined) => NodeStatus
  /** Row click → select the service (parent opens the side panel). */
  onSelect: (id: string) => void
  /** Currently selected service id, mirrored from the graph. */
  selectedId?: string
}

// Map our 4 canonical buckets onto the DS StatusDot vocabulary.
function dotStatus(s: NodeStatus): 'running' | 'degraded' | 'failed' | 'idle' {
  if (s === 'healthy') return 'running'
  if (s === 'degraded') return 'degraded'
  if (s === 'failing') return 'failed'
  return 'idle'
}

function statusTone(s: NodeStatus): 'info' | 'warning' | 'danger' | 'subtle' {
  if (s === 'healthy') return 'info'
  if (s === 'degraded') return 'warning'
  if (s === 'failing') return 'danger'
  return 'subtle'
}

const ServiceList: React.FC<ServiceListProps> = ({
  nodes,
  toNodeStatus,
  onSelect,
  selectedId,
}) => {
  const columns = useMemo<TableColumn<SystemNode>[]>(
    () => [
      {
        key: 'id',
        title: 'Service',
        render: (_value, row) => <Truncate text={row.id} />,
      },
      {
        key: 'status',
        title: 'Status',
        width: 130,
        render: (_value, row) => {
          const s = toNodeStatus(row.status)
          return (
            <StatusDot status={dotStatus(s)} label={
              <Badge tone={statusTone(s)} size="sm">{row.status}</Badge>
            } />
          )
        },
      },
      {
        key: 'rps',
        title: 'Req rate',
        width: 110,
        align: 'right',
        render: (_value, row) => `${Math.round(row.metrics.request_rate_rps)}/s`,
      },
      {
        key: 'err',
        title: 'Error rate',
        width: 110,
        align: 'right',
        render: (_value, row) => `${(row.metrics.error_rate * 100).toFixed(2)}%`,
      },
    ],
    [toNodeStatus],
  )

  return (
    <Table<SystemNode>
      columns={columns}
      data={nodes}
      rowKey="id"
      density="compact"
      stickyHeader
      selection="single"
      selectedKeys={selectedId ? [selectedId] : []}
      onRowClick={(row) => onSelect(row.id)}
      empty="No services match the filter."
    />
  )
}

export default React.memo(ServiceList)
