import { useState } from 'react';
import { Button, Badge, CodeBlock, Alert, Space, Modal } from '@ossrandom/design-system';
import Truncate from '../common/Truncate';
import type { HistoryEntry } from '../../hooks/useMcpCall';
import { toCurl } from './toolFields';

interface ToolHistoryProps {
  readonly history: HistoryEntry[];
  readonly tenant?: string;
  readonly onClear: () => void;
  /** Re-run loads the entry's tool + args back into the workbench. */
  readonly onRerun: (entry: HistoryEntry) => void;
}

const rowStyle: React.CSSProperties = {
  display: 'flex',
  alignItems: 'center',
  gap: 12,
  padding: '8px 0',
  borderBottom: '1px solid var(--rcs-color-border, rgba(127,127,127,0.18))',
};

function statusBadge(status: HistoryEntry['status']) {
  if (status === 'success') return <Badge tone="info">ok</Badge>;
  if (status === 'tool_error') return <Badge tone="warning">tool error</Badge>;
  return <Badge tone="danger">rpc error</Badge>;
}

export default function ToolHistory({ history, tenant, onClear, onRerun }: ToolHistoryProps) {
  const [snippet, setSnippet] = useState<{ title: string; code: string; lang: 'json' | 'bash' } | null>(
    null,
  );

  if (history.length === 0) {
    return <Alert severity="info" title="No calls yet" icon={false}>History is session-scoped and clears when the tab closes.</Alert>;
  }

  return (
    <Space direction="vertical" size="sm" style={{ width: '100%' }}>
      <div style={{ display: 'flex', justifyContent: 'flex-end' }}>
        <Button variant="ghost" size="sm" onClick={onClear}>
          Clear history
        </Button>
      </div>

      {history.map((h) => (
        <div key={h.id} style={rowStyle}>
          <span style={{ flex: '0 0 auto' }}>{statusBadge(h.status)}</span>
          <span style={{ flex: '0 0 auto', fontWeight: 500, fontSize: 13 }}>{h.tool}</span>
          <span style={{ minWidth: 0, flex: 1, opacity: 0.7, fontSize: 12 }}>
            <Truncate text={JSON.stringify(h.args)} />
          </span>
          <span style={{ flex: '0 0 auto', fontSize: 12, opacity: 0.6 }}>{h.latencyMs}ms</span>
          <span style={{ flex: '0 0 auto', display: 'flex', gap: 6 }}>
            <Button variant="secondary" size="sm" onClick={() => onRerun(h)}>
              Re-run
            </Button>
            <Button
              variant="ghost"
              size="sm"
              onClick={() => setSnippet({ title: 'JSON-RPC request', code: h.requestBody, lang: 'json' })}
            >
              JSON-RPC
            </Button>
            <Button
              variant="ghost"
              size="sm"
              onClick={() => setSnippet({ title: 'curl', code: toCurl(h.requestBody, tenant), lang: 'bash' })}
            >
              curl
            </Button>
          </span>
        </div>
      ))}

      <Modal
        open={snippet !== null}
        title={snippet?.title}
        size="lg"
        onClose={() => setSnippet(null)}
      >
        {snippet && (
          <CodeBlock code={snippet.code} language={snippet.lang} copyable wrap />
        )}
      </Modal>
    </Space>
  );
}
