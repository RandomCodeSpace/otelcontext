import { useMemo, useRef } from 'react'
import * as echarts from 'echarts'
import EChart from '@/components/shared/EChart'
import type { EChartsOption } from 'echarts'
import type { SystemGraphResponse } from '@/types/api'

interface Props {
  graph: SystemGraphResponse | null
  cache: string
  loading: boolean
  error: string | null
}

function statusColor(status: string): string {
  if (status === 'critical') return '#ef4444'
  if (status === 'degraded') return '#fb923c'
  return '#22c55e'
}

function edgeColor(status: string): string {
  if (status === 'critical') return '#ef4444'
  if (status === 'degraded') return '#fb923c'
  return '#3d5a78'
}

function formatErrorPct(rate: number): string {
  const pct = rate * 100
  if (pct <= 0) return '0%'
  if (pct < 0.01) return '<0.01%'
  if (pct < 1) return `${pct.toFixed(2)}%`
  return `${pct.toFixed(1)}%`
}

function truncate(s: string, max: number): string {
  return s.length <= max ? s : s.slice(0, max - 1) + '…'
}

export default function ServicesPage({ graph, cache, loading, error }: Props) {
  const chartRef = useRef<echarts.ECharts | null>(null)

  const nodes = graph?.nodes ?? []
  const edges = graph?.edges ?? []
  const denseMode = nodes.length > 70

  const maxEdges = denseMode ? 450 : 1200
  const renderedEdges = useMemo(
    () => [...edges].sort((a, b) => b.call_count - a.call_count).slice(0, maxEdges),
    [edges, maxEdges],
  )

  const topFlows = useMemo(
    () => [...edges].sort((a, b) => b.call_count - a.call_count).slice(0, 4),
    [edges],
  )

  const option = useMemo<EChartsOption>(() => {
    const nodeW = denseMode ? 76 : 118
    const nodeH = denseMode ? 34 : 46

    const gNodes = nodes.map((node) => ({
      id: node.id,
      name: node.id,
      symbol: 'roundRect',
      symbolSize: [nodeW, nodeH],
      itemStyle: {
        color: 'rgba(10,17,28,0.88)',
        borderColor: statusColor(node.status),
        borderWidth: denseMode ? 1 : 1.5,
        shadowColor: node.status !== 'healthy' ? statusColor(node.status) : 'transparent',
        shadowBlur: node.status !== 'healthy' ? 8 : 0,
      },
      label: {
        show: true,
        color: '#d9e6f5',
        fontSize: denseMode ? 9 : 11,
        fontFamily: 'Segoe UI, system-ui, sans-serif',
        lineHeight: denseMode ? 13 : 16,
        formatter: denseMode
          ? truncate(node.id, 13)
          : `{nm|${truncate(node.id, 16)}}\n{ms|${node.metrics.avg_latency_ms.toFixed(1)}ms  ${formatErrorPct(node.metrics.error_rate)}}`,
        rich: {
          nm: { fontSize: 11, color: '#d9e6f5', fontFamily: 'Segoe UI, system-ui, sans-serif', lineHeight: 16 },
          ms: { fontSize: 9, color: '#7a94ad', fontFamily: 'Segoe UI, system-ui, sans-serif', lineHeight: 13 },
        },
      },
      tooltip: {
        formatter: [
          `<b style="color:${statusColor(node.status)}">${node.id}</b>`,
          `status: <b>${node.status}</b>`,
          `rps: ${node.metrics.request_rate_rps.toFixed(1)}`,
          `avg latency: ${node.metrics.avg_latency_ms.toFixed(1)} ms`,
          `p99: ${node.metrics.p99_latency_ms.toFixed(1)} ms`,
          `error rate: ${formatErrorPct(node.metrics.error_rate)}`,
        ].join('<br/>'),
      },
    }))

    const gEdges = renderedEdges.map((edge) => {
      const w = Math.min(
        denseMode ? 2 : 3,
        Math.max(
          denseMode ? 0.4 : 0.8,
          Math.log10(edge.call_count + 1) * (denseMode ? 0.35 : 0.65),
        ),
      )
      return {
        source: edge.source,
        target: edge.target,
        symbol: ['none', 'arrow'],
        symbolSize: [6, denseMode ? 6 : 8],
        lineStyle: {
          color: edgeColor(edge.status),
          width: w,
          opacity: denseMode ? 0.5 : 0.72,
          curveness: 0.15,
        },
        label: {
          show: !denseMode,
          formatter: `${edge.call_count}`,
          fontSize: 9,
          color: '#8898aa',
        },
        tooltip: {
          formatter: `${edge.source} → ${edge.target}<br/>calls: ${edge.call_count.toLocaleString()}<br/>avg: ${edge.avg_latency_ms.toFixed(1)} ms<br/>err: ${formatErrorPct(edge.error_rate)}`,
        },
      }
    })

    return {
      backgroundColor: 'transparent',
      animation: true,
      animationDuration: 600,
      tooltip: {
        trigger: 'item',
        backgroundColor: 'rgba(8,14,22,0.96)',
        borderColor: '#2a3a50',
        borderWidth: 1,
        textStyle: { color: '#c7d6ea', fontSize: 12 },
        extraCssText: 'backdrop-filter:blur(4px)',
      },
      series: [
        {
          type: 'graph',
          layout: 'force',
          roam: true,
          draggable: true,
          data: gNodes,
          edges: gEdges,
          force: {
            repulsion: denseMode ? 180 : 280,
            gravity: denseMode ? 0.06 : 0.04,
            edgeLength: denseMode ? 110 : 170,
            layoutAnimation: true,
            friction: 0.65,
          },
          emphasis: {
            focus: 'adjacency',
            scale: false,
            lineStyle: { width: 3 },
          },
          itemStyle: { borderRadius: 6 },
        },
      ],
    }
  }, [nodes, renderedEdges, denseMode])

  const zoomBy = (factor: number) => {
    chartRef.current?.dispatchAction({ type: 'graphRoam', zoom: factor })
  }

  const fitGraph = () => {
    chartRef.current?.dispatchAction({ type: 'restore' })
  }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', flex: 1, minHeight: 0 }}>
      <div className="card" style={{ display: 'flex', flexDirection: 'column', flex: 1, minHeight: 0, overflow: 'hidden', position: 'relative', padding: 0 }}>
        <div style={{ display: 'flex', flexDirection: 'column', flex: 1, minHeight: denseMode ? 480 : 420, borderRadius: 12, border: '1px solid var(--border)', background: 'radial-gradient(circle at 18% 4%, rgba(56,189,248,0.15), transparent 32%), radial-gradient(circle at 100% 100%, rgba(16,185,129,0.1), transparent 38%), linear-gradient(180deg, var(--bg-card), var(--bg-base))', overflow: 'hidden', position: 'relative' }}>

          {/* ── Overlay toolbar ── */}
          <div style={{ position: 'absolute', top: 10, left: 10, right: 10, zIndex: 5, display: 'flex', flexDirection: 'column', gap: '0.5rem', pointerEvents: 'none' }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: '0.4rem', flexWrap: 'wrap' }}>
              <span className="badge" style={{ pointerEvents: 'auto' }}>Cache {cache}</span>
              <span className="badge" style={{ pointerEvents: 'auto' }}>{graph?.system.total_services ?? 0} services</span>
              <span className="badge" style={{ pointerEvents: 'auto' }}>{graph?.system.healthy ?? 0} healthy</span>
              <span className="badge badge-orange" style={{ pointerEvents: 'auto' }}>{graph?.system.degraded ?? 0} degraded</span>
              <span className="badge badge-red" style={{ pointerEvents: 'auto' }}>{graph?.system.critical ?? 0} critical</span>
              {denseMode && <span className="badge" style={{ pointerEvents: 'auto' }}>Dense</span>}
              {renderedEdges.length < edges.length && (
                <span className="badge" style={{ pointerEvents: 'auto' }}>Top {renderedEdges.length}/{edges.length} flows</span>
              )}
              <div style={{ marginLeft: 'auto', display: 'flex', gap: '0.35rem', pointerEvents: 'auto' }}>
                <button className="theme-btn" type="button" onClick={() => zoomBy(1.25)}>+</button>
                <button className="theme-btn" type="button" onClick={() => zoomBy(0.8)}>−</button>
                <button className="theme-btn" type="button" onClick={fitGraph}>Fit</button>
              </div>
            </div>

            {topFlows.length > 0 && (
              <div style={{ display: 'flex', gap: '0.35rem', overflowX: 'auto', pointerEvents: 'auto' }}>
                {topFlows.map((edge) => (
                  <div key={`${edge.source}-${edge.target}`} style={{ border: '1px solid var(--border)', borderRadius: 999, padding: '0.22rem 0.55rem', background: 'rgba(11,18,29,0.82)', whiteSpace: 'nowrap', fontSize: '0.66rem', color: 'var(--text-muted)', flexShrink: 0 }}>
                    <span style={{ color: 'var(--text-primary)', fontWeight: 600 }}>{edge.source} → {edge.target}</span>
                    {' | '}{edge.call_count.toLocaleString()} calls | {edge.avg_latency_ms.toFixed(1)} ms | {formatErrorPct(edge.error_rate)} err
                  </div>
                ))}
              </div>
            )}

            {loading && <div style={{ color: 'var(--text-muted)', fontSize: '0.74rem' }}>Loading graph…</div>}
            {error && <div style={{ color: '#ef4444', fontSize: '0.74rem' }}>{error}</div>}
          </div>

          {/* ── Graph canvas ── */}
          {nodes.length === 0 ? (
            <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center', color: 'var(--text-muted)' }}>No service graph available.</div>
          ) : (
            <EChart option={option} chartRef={chartRef} style={{ flex: 1, minHeight: 0 }} />
          )}
        </div>
      </div>
    </div>
  )
}
