import type { Trace } from '@/types/api'
import JsonViewer from '@/components/shared/JsonViewer'

interface Props {
  traces: Trace[]
  selected: Trace | null
  loading: boolean
  error: string | null
  onSelect: (traceId: string) => void
}

export default function TracesPage({ traces, selected, loading, error, onSelect }: Props) {
  return (
    <div className="traces-layout">
      <div className="card" style={{ display: 'flex', flexDirection: 'column', gap: '0.8rem', minHeight: 0, overflow: 'hidden' }}>
        <div style={{ flexShrink: 0 }}>
          <div style={{ fontSize: '0.74rem', textTransform: 'uppercase', letterSpacing: '0.12em', color: 'var(--text-dim)', marginBottom: '0.35rem' }}>Traces</div>
          <div style={{ fontSize: '0.95rem', fontWeight: 700 }}>Recent distributed requests</div>
        </div>
        <div style={{ flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column', gap: '0.65rem', overflowY: 'auto', overflowX: 'hidden' }}>
          {loading && <div style={{ color: 'var(--text-muted)' }}>Loading traces…</div>}
          {error && <div style={{ color: '#ef4444' }}>{error}</div>}
          {traces.map((trace) => (
            <button key={trace.trace_id} onClick={() => onSelect(trace.trace_id)} className="card" style={{ textAlign: 'left', background: selected?.trace_id === trace.trace_id ? 'var(--nav-active-bg)' : 'var(--bg-card)', borderColor: selected?.trace_id === trace.trace_id ? 'var(--color-accent)' : 'var(--border)', padding: '0.9rem', cursor: 'pointer' }}>
              <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: '0.75rem', marginBottom: '0.35rem' }}>
                <div style={{ fontWeight: 700, fontSize: '0.78rem' }}>{trace.service_name}</div>
                <span className={`badge ${trace.status.includes('ERROR') ? 'badge-red' : 'badge-green'}`}>{trace.status || 'OK'}</span>
              </div>
              <div style={{ fontSize: '0.72rem', color: 'var(--text-muted)', marginBottom: '0.3rem' }}>{trace.operation || trace.trace_id}</div>
              <div style={{ display: 'flex', gap: '0.4rem', flexWrap: 'wrap' }}>
                <span className="badge">{trace.span_count} spans</span>
                <span className="badge">{trace.duration_ms?.toFixed(1)} ms</span>
              </div>
            </button>
          ))}
        </div>
      </div>
      <div className="traces-right-col">
        <div className="card">
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: '1rem' }}>
            <div>
              <div style={{ fontSize: '0.85rem', fontWeight: 700 }}>{selected?.trace_id ?? 'No trace selected'}</div>
              <div style={{ fontSize: '0.73rem', color: 'var(--text-muted)', marginTop: '0.2rem' }}>{selected?.service_name}</div>
            </div>
            {selected && <span className={`badge ${selected.status.includes('ERROR') ? 'badge-red' : 'badge-green'}`}>{selected.status}</span>}
          </div>
        </div>
        <div className="card" style={{ overflow: 'auto' }}>
          <div style={{ fontSize: '0.8rem', fontWeight: 700, marginBottom: '0.8rem' }}>Span Waterfall</div>
          <div style={{ display: 'flex', flexDirection: 'column', gap: '0.7rem' }}>
            {(selected?.spans ?? []).map((span) => (
              <div key={span.id} style={{ border: '1px solid var(--border)', borderRadius: 10, padding: '0.8rem', background: 'var(--bg-card)' }}>
                <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: '0.75rem', marginBottom: '0.35rem' }}>
                  <div style={{ fontWeight: 700, fontSize: '0.78rem' }}>{span.operation_name}</div>
                  <span className="badge">{(span.duration / 1000).toFixed(1)} ms</span>
                </div>
                <div style={{ height: 8, borderRadius: 999, background: 'var(--bg-base)', overflow: 'hidden', marginBottom: '0.45rem' }}>
                  <div style={{ width: `${Math.min(100, Math.max(6, (span.duration / Math.max(selected?.duration || 1, 1)) * 100))}%`, height: '100%', background: 'linear-gradient(90deg, var(--color-accent), var(--color-accent-hover))' }} />
                </div>
                <div style={{ fontSize: '0.72rem', color: 'var(--text-muted)' }}>{span.service_name}</div>
              </div>
            ))}
          </div>
        </div>
        <JsonViewer title="Selected trace JSON" value={selected ?? {}} />
      </div>
    </div>
  )
}
