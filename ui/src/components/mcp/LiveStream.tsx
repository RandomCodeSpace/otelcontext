import { useEffect, useRef, useState } from 'react';
import { Terminal, Switch, Tooltip, Space } from '@ossrandom/design-system';
import type { TerminalLine } from '@ossrandom/design-system';

interface LiveStreamProps {
  /** When an API key is set the toggle is disabled — EventSource can't send
   *  an Authorization header, so the SSE GET would 401. */
  readonly apiKey: string;
}

const MAX_LINES = 200;

/**
 * Optional SSE viewer for the `/mcp` GET stream. The backend emits a
 * `: keep-alive` comment every 25s; EventSource swallows comments, so we also
 * surface the open/error lifecycle to make the heartbeat observable.
 */
export default function LiveStream({ apiKey }: LiveStreamProps) {
  const [on, setOn] = useState(false);
  const [lines, setLines] = useState<TerminalLine[]>([]);
  const esRef = useRef<EventSource | null>(null);

  const disabled = apiKey.length > 0;

  useEffect(() => {
    if (!on || disabled) {
      esRef.current?.close();
      esRef.current = null;
      return;
    }
    const push = (line: TerminalLine) =>
      setLines((prev) => [...prev, line].slice(-MAX_LINES));

    const es = new EventSource('/mcp');
    esRef.current = es;
    push({ type: 'info', text: 'opening /mcp SSE stream…', timestamp: new Date() });
    es.onopen = () => push({ type: 'info', text: 'stream open — waiting for events', timestamp: new Date() });
    es.onmessage = (ev) =>
      push({ type: 'stdout', text: ev.data || '(empty event)', timestamp: new Date() });
    es.onerror = () =>
      push({ type: 'warn', text: 'stream error / reconnecting (keep-alive every 25s)', timestamp: new Date() });

    return () => {
      es.close();
      esRef.current = null;
    };
  }, [on, disabled]);

  return (
    <Space direction="vertical" size="sm" style={{ width: '100%' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
        {disabled ? (
          <Tooltip content="EventSource can't send an Authorization header. Clear the API key to use the live stream.">
            <span style={{ display: 'inline-flex' }}>
              <Switch checked={false} disabled label="Live stream" />
            </span>
          </Tooltip>
        ) : (
          <Switch checked={on} label="Live stream" onChange={setOn} />
        )}
      </div>
      {on && !disabled && (
        <Terminal
          title="/mcp SSE"
          lines={lines.length > 0 ? lines : [{ type: 'info', text: 'connecting…' }]}
          streaming
          height={220}
        />
      )}
    </Space>
  );
}
