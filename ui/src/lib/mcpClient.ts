// Minimal JSON-RPC 2.0 transport for the OtelContext MCP endpoint (POST /mcp).
// Shared by the triage-verb queries (lib/triageVerbs) and the anomaly
// timeline (hooks/useAnomalyTimeline). Transport only — result parsing
// (content[0].text JSON) lives in lib/mcpResults.

export interface McpToolResult {
  content?: { type: string; text?: string }[]
  isError?: boolean
  [key: string]: unknown
}

export interface McpCallOptions {
  apiKey?: string
  tenant?: string
  signal?: AbortSignal
}

export class McpError extends Error {
  code: number
  constructor(code: number, message: string) {
    super(message)
    this.name = 'McpError'
    this.code = code
  }
}

let idCounter = 0

function buildHeaders(opts?: McpCallOptions): Record<string, string> {
  const h: Record<string, string> = { 'Content-Type': 'application/json' }
  if (opts?.apiKey) h.Authorization = `Bearer ${opts.apiKey}`
  if (opts?.tenant) h['X-Tenant-ID'] = opts.tenant
  return h
}

export async function mcpRpc<T = unknown>(
  method: string,
  params: unknown,
  opts?: McpCallOptions,
): Promise<T> {
  const id = ++idCounter
  const res = await fetch('/mcp', {
    method: 'POST',
    headers: buildHeaders(opts),
    body: JSON.stringify({ jsonrpc: '2.0', id, method, params }),
    signal: opts?.signal,
  })
  if (!res.ok) throw new McpError(res.status, `HTTP ${res.status}`)
  const json = (await res.json()) as {
    error?: { code?: number; message?: string }
    result?: T
  }
  if (json.error) {
    throw new McpError(json.error.code ?? -1, json.error.message ?? 'MCP error')
  }
  return json.result as T
}

export async function callMcpTool(
  name: string,
  args: Record<string, unknown>,
  opts?: McpCallOptions,
): Promise<McpToolResult> {
  return mcpRpc<McpToolResult>('tools/call', { name, arguments: args }, opts)
}
