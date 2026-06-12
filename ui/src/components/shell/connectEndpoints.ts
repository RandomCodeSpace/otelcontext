// The connect strings for agents and OTLP exporters, derived from the page
// origin so copied values work wherever the operator is browsing from.
// Shared by the pulse-bar Connect popover and the Triage empty state.

export interface Endpoint {
  key: string
  label: string
  value: string
}

export function endpoints(): Endpoint[] {
  const { origin, hostname } = window.location
  return [
    { key: 'mcp', label: 'MCP URL', value: `${origin}/mcp` },
    { key: 'grpc', label: 'OTLP gRPC', value: `${hostname}:4317` },
    { key: 'http', label: 'OTLP HTTP', value: `${origin}/v1/` },
  ]
}
