import { useCallback, useEffect, useRef, useState } from 'react';
import {
  callMcpTool,
  McpError,
  type McpCallOptions,
  type McpToolResult,
} from '../lib/mcpClient';
import {
  parseToolResult,
  type ParsedResult,
} from '../components/mcp/mcpTypes';

const HISTORY_KEY = 'otelcontext.mcp.history';
const HISTORY_CAP = 50;

/** A single invocation as persisted to the History tab (sessionStorage). */
export interface HistoryEntry {
  id: string;
  tool: string;
  args: Record<string, unknown>;
  /** The exact JSON-RPC request body sent (for Copy JSON-RPC / re-run). */
  requestBody: string;
  status: 'success' | 'tool_error' | 'rpc_error';
  latencyMs: number;
  timestamp: number;
}

/** Discriminated error surfaced to the Result pane (3-way per the spec). */
export type CallError =
  | { kind: 'rpc'; code: number; message: string }
  | { kind: 'transport'; message: string };

export interface CallState {
  loading: boolean;
  /** Raw JSON-RPC result (undefined until a call resolves). */
  result?: McpToolResult;
  /** Decoded payload + root_cause + tool-level isError. */
  parsed?: ParsedResult;
  /** Transport / JSON-RPC error (NOT a tool-level isError). */
  error?: CallError;
  /** The request body of the in-flight or last call. */
  requestBody?: string;
  latencyMs?: number;
}

function readHistory(): HistoryEntry[] {
  try {
    const raw = sessionStorage.getItem(HISTORY_KEY);
    if (!raw) return [];
    const parsed = JSON.parse(raw);
    return Array.isArray(parsed) ? (parsed as HistoryEntry[]) : [];
  } catch {
    return [];
  }
}

function writeHistory(entries: HistoryEntry[]): void {
  try {
    sessionStorage.setItem(HISTORY_KEY, JSON.stringify(entries.slice(0, HISTORY_CAP)));
  } catch {
    /* sessionStorage unavailable / quota — history is best-effort. */
  }
}

/** Builds the JSON-RPC request body the transport will POST (for display). */
export function buildRequestBody(tool: string, args: Record<string, unknown>): string {
  return JSON.stringify(
    { jsonrpc: '2.0', id: 1, method: 'tools/call', params: { name: tool, arguments: args } },
    null,
    2,
  );
}

let entrySeq = 0;
function nextId(): string {
  entrySeq += 1;
  return `${Date.now().toString(36)}-${entrySeq}`;
}

/**
 * Invokes MCP tools, tracks call state with the 3-way error branch, and
 * records a capped, sessionStorage-backed history. The latest in-flight call
 * wins — a new `call()` aborts the previous one.
 */
export function useMcpCall(opts?: McpCallOptions) {
  const [state, setState] = useState<CallState>({ loading: false });
  const [history, setHistory] = useState<HistoryEntry[]>(() => readHistory());
  const abortRef = useRef<AbortController | null>(null);

  const apiKey = opts?.apiKey;
  const tenant = opts?.tenant;

  useEffect(() => {
    return () => abortRef.current?.abort();
  }, []);

  const pushHistory = useCallback((entry: HistoryEntry) => {
    setHistory((prev) => {
      const next = [entry, ...prev].slice(0, HISTORY_CAP);
      writeHistory(next);
      return next;
    });
  }, []);

  const call = useCallback(
    async (tool: string, args: Record<string, unknown>) => {
      abortRef.current?.abort();
      const ctrl = new AbortController();
      abortRef.current = ctrl;

      const requestBody = buildRequestBody(tool, args);
      const started = performance.now();
      setState({ loading: true, requestBody });

      try {
        const result = await callMcpTool(tool, args, {
          apiKey,
          tenant,
          signal: ctrl.signal,
        });
        if (ctrl.signal.aborted) return;
        const latencyMs = Math.round(performance.now() - started);
        const parsed = parseToolResult(result);
        setState({ loading: false, result, parsed, requestBody, latencyMs });
        pushHistory({
          id: nextId(),
          tool,
          args,
          requestBody,
          status: parsed.isError ? 'tool_error' : 'success',
          latencyMs,
          timestamp: Date.now(),
        });
      } catch (e: unknown) {
        if (ctrl.signal.aborted) return;
        const latencyMs = Math.round(performance.now() - started);
        const error: CallError =
          e instanceof McpError
            ? { kind: 'rpc', code: e.code, message: e.message }
            : { kind: 'transport', message: e instanceof Error ? e.message : 'request failed' };
        setState({ loading: false, error, requestBody, latencyMs });
        pushHistory({
          id: nextId(),
          tool,
          args,
          requestBody,
          status: 'rpc_error',
          latencyMs,
          timestamp: Date.now(),
        });
      }
    },
    [apiKey, tenant, pushHistory],
  );

  const reset = useCallback(() => setState({ loading: false }), []);

  const clearHistory = useCallback(() => {
    setHistory([]);
    writeHistory([]);
  }, []);

  return { state, call, reset, history, clearHistory };
}
