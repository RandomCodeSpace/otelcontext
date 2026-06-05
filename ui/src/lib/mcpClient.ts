// Minimal JSON-RPC 2.0 transport for the OtelContext MCP endpoint (POST /mcp).
// Shared by the MCP Trial console (hooks/useMcpTools, useMcpCall) and the
// dashboard's anomaly panel (hooks/useAnomalies). Transport only — result
// parsing (content[0].text JSON, root_cause extraction) is the caller's job.

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

export interface McpToolSchema {
  type?: string
  properties?: Record<string, { type?: string; description?: string }>
  required?: string[]
}

export interface McpTool {
  name: string
  description?: string
  inputSchema?: McpToolSchema
}

export async function listMcpTools(opts?: McpCallOptions): Promise<McpTool[]> {
  const result = await mcpRpc<{ tools?: McpTool[] }>('tools/list', {}, opts)
  return result.tools ?? []
}

export async function callMcpTool(
  name: string,
  args: Record<string, unknown>,
  opts?: McpCallOptions,
): Promise<McpToolResult> {
  return mcpRpc<McpToolResult>('tools/call', { name, arguments: args }, opts)
}
