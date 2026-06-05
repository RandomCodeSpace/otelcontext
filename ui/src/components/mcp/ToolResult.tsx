import { useState } from 'react';
import { Alert, CodeBlock, Tabs, Spin, Space } from '@ossrandom/design-system';
import type { CallState } from '../../hooks/useMcpCall';
import type {
  AnomalyNode,
  ServiceMapEntry,
  ImpactResult,
  RankedCause,
  SpanNode,
  SearchLogsResult,
  RootCauseInfo,
} from './mcpTypes';
import {
  AnomalyTimelineView,
  ServiceMapView,
  ServiceHealthView,
  ImpactView,
  RootCauseView,
  TraceGraphView,
  SearchLogsView,
} from './ToolResultViews';

interface ToolResultProps {
  readonly tool: string;
  readonly state: CallState;
}

const sectionLabel: React.CSSProperties = {
  fontSize: 12,
  fontWeight: 600,
  textTransform: 'uppercase',
  letterSpacing: 0.4,
  opacity: 0.6,
  marginBottom: 6,
};

/** Pinned root_cause banner — appears above any error-identifying result. */
function RootCauseBanner({ rc }: { rc: RootCauseInfo }) {
  return (
    <Alert severity="danger" title="Root cause">
      <div style={{ display: 'grid', gap: 2, fontSize: 13 }}>
        <div>
          <strong>{rc.service}</strong>
          {rc.operation ? ` · ${rc.operation}` : ''}
        </div>
        {rc.error_message && <div style={{ opacity: 0.85 }}>{rc.error_message}</div>}
        {(rc.trace_id || rc.span_id) && (
          <div style={{ fontSize: 12, opacity: 0.7, fontFamily: 'monospace' }}>
            {rc.trace_id && `trace ${rc.trace_id}`}
            {rc.trace_id && rc.span_id ? ' · ' : ''}
            {rc.span_id && `span ${rc.span_id}`}
          </div>
        )}
      </div>
    </Alert>
  );
}

/** Dispatches to a per-tool friendly view; falls back to a notice otherwise. */
function FriendlyView({ tool, payload }: { tool: string; payload: unknown }) {
  if (payload === undefined || payload === null) {
    return <Alert severity="info" title="No structured payload" />;
  }
  switch (tool) {
    case 'get_anomaly_timeline':
      return <AnomalyTimelineView data={(payload as AnomalyNode[]) ?? []} />;
    case 'get_service_map':
      return <ServiceMapView data={(payload as ServiceMapEntry[]) ?? []} />;
    case 'get_service_health':
      return <ServiceHealthView data={payload as ServiceMapEntry} />;
    case 'impact_analysis':
      return <ImpactView data={payload as ImpactResult} />;
    case 'root_cause_analysis':
      return <RootCauseView data={(payload as RankedCause[]) ?? []} />;
    case 'trace_graph':
      return <TraceGraphView data={payload as SpanNode[]} />;
    case 'search_logs':
      return <SearchLogsView data={payload as SearchLogsResult} />;
    default:
      return <Alert severity="info" title="No friendly renderer — see Raw JSON" />;
  }
}

/** Maps the 3-way error branch to an Alert. RPC −32000/−32001 are warnings. */
function ErrorView({ state }: { state: CallState }) {
  const err = state.error;
  if (!err) return null;
  if (err.kind === 'rpc') {
    if (err.code === -32000) {
      return (
        <Alert severity="warning" title="Server at capacity, retry shortly">
          JSON-RPC −32000 — the MCP concurrency cap was hit.
        </Alert>
      );
    }
    if (err.code === -32001) {
      return (
        <Alert severity="warning" title="Tool exceeded 30s deadline">
          JSON-RPC −32001 — narrow the time range or arguments and retry.
        </Alert>
      );
    }
    return (
      <Alert severity="danger" title={`MCP error ${err.code}`}>
        {err.message}
      </Alert>
    );
  }
  return (
    <Alert severity="danger" title="Request failed">
      {err.message}
    </Alert>
  );
}

export default function ToolResult({ tool, state }: ToolResultProps) {
  const [view, setView] = useState<'friendly' | 'raw'>('friendly');

  if (state.loading) {
    return (
      <div style={{ padding: 24, display: 'flex', justifyContent: 'center' }}>
        <Spin label="Calling tool…" />
      </div>
    );
  }

  // Nothing run yet.
  if (!state.result && !state.error) {
    return (
      <Alert severity="info" title="Run a tool to see results" icon={false}>
        Fill in the form and hit Run tool. The request and decoded response will
        appear here.
      </Alert>
    );
  }

  const parsed = state.parsed;

  return (
    <Space direction="vertical" size="md" style={{ width: '100%' }}>
      {/* Transport / JSON-RPC error branch (1 of 3). */}
      <ErrorView state={state} />

      {/* Tool-level isError branch (2 of 3). */}
      {parsed?.isError && (
        <Alert severity="warning" title="Tool returned an error">
          {parsed.text || 'The tool reported isError without a message.'}
        </Alert>
      )}

      {/* Pinned root_cause, if the payload carries one. */}
      {parsed?.rootCause && <RootCauseBanner rc={parsed.rootCause} />}

      {/* Request body. */}
      {state.requestBody && (
        <div>
          <div style={sectionLabel}>
            Request{typeof state.latencyMs === 'number' ? ` · ${state.latencyMs}ms` : ''}
          </div>
          <CodeBlock code={state.requestBody} language="json" copyable wrap />
        </div>
      )}

      {/* Response (success, 3 of 3): Friendly vs Raw. Raw is always available. */}
      {parsed && !parsed.isError && state.result && (
        <div>
          <div style={sectionLabel}>Response</div>
          <Tabs
            variant="segment"
            value={view}
            items={[
              { key: 'friendly', label: 'Friendly' },
              { key: 'raw', label: 'Raw JSON' },
            ]}
            onChange={(k) => setView(k as 'friendly' | 'raw')}
          />
          <div style={{ marginTop: 12 }}>
            {view === 'friendly' ? (
              <FriendlyView tool={tool} payload={parsed.payload} />
            ) : (
              <CodeBlock
                code={parsed.text || JSON.stringify(state.result, null, 2)}
                language="json"
                copyable
                wrap
              />
            )}
          </div>
        </div>
      )}

      {/* If the tool errored, still surface the raw error text as JSON-ish. */}
      {parsed?.isError && state.result && (
        <div>
          <div style={sectionLabel}>Raw response</div>
          <CodeBlock code={JSON.stringify(state.result, null, 2)} language="json" copyable wrap />
        </div>
      )}
    </Space>
  );
}
