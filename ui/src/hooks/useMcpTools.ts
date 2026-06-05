import { useCallback, useEffect, useState } from 'react';
import {
  listMcpTools,
  McpError,
  type McpTool,
  type McpCallOptions,
} from '../lib/mcpClient';

interface UseMcpToolsState {
  tools: McpTool[];
  loading: boolean;
  /** Connectivity/listing error — doubles as the `/mcp` reachability check. */
  error: string | null;
  reload: () => void;
}

/**
 * Loads the live MCP tool catalog via `tools/list`. The surface was cut from
 * 21 → 7 tools and remains volatile, so the console never hardcodes it. A
 * failure here is also the connectivity signal the view surfaces in an Alert.
 *
 * `opts` carries the optional API key + tenant; the hook re-lists whenever they
 * change (a key/tenant flip can change reachability or scope).
 */
export function useMcpTools(opts?: McpCallOptions): UseMcpToolsState {
  const [tools, setTools] = useState<McpTool[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const apiKey = opts?.apiKey;
  const tenant = opts?.tenant;

  const load = useCallback(() => {
    const ctrl = new AbortController();
    setLoading(true);
    listMcpTools({ apiKey, tenant, signal: ctrl.signal })
      .then((list) => {
        setTools(list);
        setError(null);
      })
      .catch((e: unknown) => {
        if (ctrl.signal.aborted) return;
        if (e instanceof McpError) {
          setError(`MCP ${e.code}: ${e.message}`);
        } else {
          setError(e instanceof Error ? e.message : 'failed to reach /mcp');
        }
        setTools([]);
      })
      .finally(() => {
        if (!ctrl.signal.aborted) setLoading(false);
      });
    return () => ctrl.abort();
  }, [apiKey, tenant]);

  useEffect(() => load(), [load]);

  return { tools, loading, error, reload: load };
}
