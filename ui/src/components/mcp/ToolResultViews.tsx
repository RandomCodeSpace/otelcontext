// Per-tool "Friendly" renderers. Each takes the already-decoded payload and
// renders a restrained, MCP-Inspector-style view. Every renderer caps the row
// count and reports "showing N of M" — trace_graph / get_service_map can
// approach the 4MB response cap. The caller always keeps a Raw JSON fallback.

import { Table, Timeline, Stat, Badge, Alert, StatusDot } from '@ossrandom/design-system';
import type { TableColumn } from '@ossrandom/design-system';
import Truncate from '../common/Truncate';
import type {
  AnomalyNode,
  ServiceMapEntry,
  ImpactResult,
  RankedCause,
  SpanNode,
  SearchLogsResult,
  ServiceNode,
} from './mcpTypes';

const ROW_CAP = 100;

const statGrid: React.CSSProperties = {
  display: 'grid',
  gap: 12,
  gridTemplateColumns: 'repeat(auto-fit, minmax(160px, 1fr))',
};

function ShowingNote({ shown, total }: { shown: number; total: number }) {
  if (total <= shown) return null;
  return (
    <div style={{ fontSize: 12, opacity: 0.7, marginTop: 8 }}>
      Showing {shown} of {total} — open Raw JSON for the full payload.
    </div>
  );
}

function severityTone(sev: string): 'neutral' | 'danger' | 'warning' | 'info' {
  const s = sev.toUpperCase();
  if (s === 'ERROR' || s === 'FATAL' || s === 'CRITICAL') return 'danger';
  if (s === 'WARN' || s === 'WARNING') return 'warning';
  if (s === 'INFO') return 'info';
  return 'neutral';
}

function healthStatus(score: number): 'running' | 'degraded' | 'failed' {
  if (score >= 0.85) return 'running';
  if (score >= 0.5) return 'degraded';
  return 'failed';
}

function fmtTime(ts: string): string {
  const d = new Date(ts);
  return Number.isNaN(d.getTime()) ? ts : d.toLocaleString();
}

// --- get_anomaly_timeline ---

export function AnomalyTimelineView({ data }: { data: AnomalyNode[] }) {
  if (data.length === 0) return <Alert severity="success" title="No anomalies in window" />;
  const shown = data.slice(0, ROW_CAP);
  return (
    <>
      <Timeline
        items={shown.map((a) => ({
          key: a.id,
          title: `${a.service} — ${a.type}`,
          description: a.evidence,
          time: fmtTime(a.timestamp),
          tone: a.severity?.toLowerCase() === 'critical' ? 'danger' : 'warning',
        }))}
      />
      <ShowingNote shown={shown.length} total={data.length} />
    </>
  );
}

// --- get_service_map ---

interface ServiceRow {
  name: string;
  health_score: number;
  error_rate: number;
  call_count: number;
  avg_latency_ms: number;
  deps: number;
}

export function ServiceMapView({ data }: { data: ServiceMapEntry[] }) {
  const rows: ServiceRow[] = data
    .filter((e) => e.service)
    .slice(0, ROW_CAP)
    .map((e) => {
      const s = e.service as ServiceNode;
      return {
        name: s.name,
        health_score: s.health_score,
        error_rate: s.error_rate,
        call_count: s.call_count,
        avg_latency_ms: s.avg_latency_ms,
        deps: e.calls_to?.length ?? 0,
      };
    });

  const columns: ReadonlyArray<TableColumn<ServiceRow>> = [
    {
      key: 'name',
      title: 'Service',
      render: (_v, r) => (
        <span style={{ display: 'flex', alignItems: 'center', gap: 8, minWidth: 0 }}>
          <StatusDot status={healthStatus(r.health_score)} />
          <Truncate text={r.name} />
        </span>
      ),
    },
    { key: 'health', title: 'Health', align: 'right', render: (_v, r) => r.health_score.toFixed(2) },
    {
      key: 'err',
      title: 'Error rate',
      align: 'right',
      render: (_v, r) => `${(r.error_rate * 100).toFixed(1)}%`,
    },
    { key: 'calls', title: 'Calls', align: 'right', dataKey: 'call_count' },
    {
      key: 'lat',
      title: 'Avg ms',
      align: 'right',
      render: (_v, r) => r.avg_latency_ms.toFixed(1),
    },
    { key: 'deps', title: 'Deps', align: 'right', dataKey: 'deps' },
  ];

  return (
    <>
      <Table columns={columns} data={rows} rowKey="name" density="compact" striped />
      <ShowingNote shown={rows.length} total={data.filter((e) => e.service).length} />
    </>
  );
}

// --- get_service_health ---

export function ServiceHealthView({ data }: { data: ServiceMapEntry }) {
  const s = data.service;
  if (!s) return <Alert severity="warning" title="Service not found in current window" />;
  return (
    <div style={statGrid}>
      <Stat label="Health score" value={s.health_score.toFixed(2)} />
      <Stat label="Error rate" value={`${(s.error_rate * 100).toFixed(1)}`} unit="%" />
      <Stat label="Requests" value={s.call_count} />
      <Stat label="Errors" value={s.error_count} />
      <Stat label="Avg latency" value={s.avg_latency_ms.toFixed(1)} unit="ms" />
      <Stat label="Operations" value={data.operations?.length ?? 0} />
    </div>
  );
}

// --- impact_analysis ---

export function ImpactView({ data }: { data: ImpactResult }) {
  const affected = data.affected_services ?? [];
  if (affected.length === 0) {
    return <Alert severity="success" title={`No downstream impact from ${data.service}`} />;
  }
  const rows = affected.slice(0, ROW_CAP);
  const columns: ReadonlyArray<TableColumn<(typeof affected)[number]>> = [
    { key: 'service', title: 'Service', render: (_v, r) => <Truncate text={r.service} /> },
    { key: 'depth', title: 'Depth', align: 'right', dataKey: 'depth' },
    { key: 'calls', title: 'Calls', align: 'right', dataKey: 'call_count' },
    {
      key: 'impact',
      title: 'Impact',
      align: 'right',
      render: (_v, r) => r.impact_score.toFixed(2),
    },
  ];
  return (
    <>
      <div style={{ marginBottom: 8 }}>
        <Badge tone="warning">{data.total_downstream} downstream affected</Badge>
      </div>
      <Table columns={columns} data={rows} rowKey="service" density="compact" striped />
      <ShowingNote shown={rows.length} total={affected.length} />
    </>
  );
}

// --- root_cause_analysis ---

export function RootCauseView({ data }: { data: RankedCause[] }) {
  if (data.length === 0) return <Alert severity="info" title="No ranked causes found" />;
  const shown = data.slice(0, ROW_CAP);
  return (
    <>
      <Timeline
        items={shown.map((c, i) => ({
          key: `${c.service}|${c.operation}|${i}`,
          title: `${c.service}${c.operation ? ` · ${c.operation}` : ''}`,
          description: (c.evidence ?? []).join(' · ') || `score ${c.score.toFixed(2)}`,
          time: `score ${c.score.toFixed(2)}`,
          tone: i === 0 ? 'danger' : 'warning',
        }))}
      />
      <ShowingNote shown={shown.length} total={data.length} />
    </>
  );
}

// --- trace_graph (indented span tree) ---

interface TreeNode {
  span: SpanNode;
  children: TreeNode[];
}

function buildTree(spans: SpanNode[]): { roots: TreeNode[]; count: number } {
  const byId = new Map<string, TreeNode>();
  for (const s of spans) byId.set(s.id, { span: s, children: [] });
  const roots: TreeNode[] = [];
  for (const node of byId.values()) {
    const parent = node.span.parent_span_id ? byId.get(node.span.parent_span_id) : undefined;
    if (parent) parent.children.push(node);
    else roots.push(node);
  }
  return { roots, count: byId.size };
}

const treeRow: React.CSSProperties = {
  display: 'flex',
  alignItems: 'center',
  gap: 8,
  padding: '3px 0',
  minWidth: 0,
  fontVariantNumeric: 'tabular-nums',
};

export function TraceGraphView({ data }: { data: SpanNode[] }) {
  if (!Array.isArray(data) || data.length === 0) {
    return <Alert severity="info" title="No spans for this trace" />;
  }
  const { roots, count } = buildTree(data);
  let rendered = 0;
  const lines: React.ReactNode[] = [];

  const walk = (node: TreeNode, depth: number) => {
    if (rendered >= ROW_CAP) return;
    rendered += 1;
    const s = node.span;
    lines.push(
      <div key={s.id} style={{ ...treeRow, paddingLeft: depth * 18 }}>
        <StatusDot status={s.is_error ? 'failed' : 'running'} />
        <span style={{ flex: '0 0 auto', fontSize: 13, fontWeight: 500 }}>{s.service}</span>
        <span style={{ minWidth: 0, flex: 1, opacity: 0.8 }}>
          <Truncate text={s.operation || '—'} />
        </span>
        <span style={{ flex: '0 0 auto', fontSize: 12, opacity: 0.7 }}>
          {s.duration_ms.toFixed(1)}ms
        </span>
      </div>,
    );
    for (const c of node.children) walk(c, depth + 1);
  };
  for (const r of roots) walk(r, 0);

  return (
    <>
      <div>{lines}</div>
      <ShowingNote shown={rendered} total={count} />
    </>
  );
}

// --- search_logs ---

export function SearchLogsView({ data }: { data: SearchLogsResult }) {
  const entries = data.entries ?? [];
  if (entries.length === 0) {
    return <Alert severity="info" title="No matching logs" />;
  }
  const rows = entries.slice(0, ROW_CAP);
  const columns: ReadonlyArray<TableColumn<(typeof entries)[number]>> = [
    {
      key: 'ts',
      title: 'Time',
      width: 180,
      render: (_v, r) => <span style={{ fontSize: 12 }}>{fmtTime(r.timestamp)}</span>,
    },
    {
      key: 'sev',
      title: 'Severity',
      width: 96,
      render: (_v, r) => <Badge tone={severityTone(r.severity)}>{r.severity || '—'}</Badge>,
    },
    {
      key: 'svc',
      title: 'Service',
      width: 160,
      render: (_v, r) => <Truncate text={r.service_name} />,
    },
    { key: 'body', title: 'Message', render: (_v, r) => <Truncate text={r.body} /> },
  ];
  return (
    <>
      <Table columns={columns} data={rows} rowKey="id" density="compact" striped />
      <ShowingNote shown={rows.length} total={data.total ?? entries.length} />
    </>
  );
}
