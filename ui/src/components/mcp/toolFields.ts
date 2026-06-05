// Schema → control mapping for the MCP ToolForm. The JSON schema the backend
// ships is string/number-only (no enums), so we dispatch on the FIELD NAME to
// pick the right control, per the design spec §5.

import type { McpTool } from '../../lib/mcpClient';

export type FieldKind =
  | 'service' // searchable Select from /api/metadata/services
  | 'severity' // fixed Select ERROR/WARN/INFO/DEBUG
  | 'datetime' // DatePicker → RFC3339 + presets
  | 'duration' // Go duration Input (e.g. "15m")
  | 'number' // NumberInput
  | 'text'; // plain Input

export interface ToolField {
  name: string;
  kind: FieldKind;
  required: boolean;
  description?: string;
  /** Hard ceiling for numeric fields (limit ≤ 200). */
  max?: number;
  min?: number;
}

export const SEVERITY_OPTIONS = ['ERROR', 'WARN', 'INFO', 'DEBUG'] as const;

/** Classify a schema property by name (and declared type as a fallback). */
export function fieldKindFor(name: string, schemaType?: string): FieldKind {
  switch (name) {
    case 'service':
    case 'service_name':
      return 'service';
    case 'severity':
      return 'severity';
    case 'since':
    case 'start':
    case 'end':
      return 'datetime';
    case 'time_range':
      return 'duration';
    case 'depth':
    case 'limit':
    case 'page':
      return 'number';
    default:
      return schemaType === 'number' ? 'number' : 'text';
  }
}

/** Derive the ordered field list for a tool from its live inputSchema. */
export function fieldsForTool(tool: McpTool): ToolField[] {
  const props = tool.inputSchema?.properties ?? {};
  const required = new Set(tool.inputSchema?.required ?? []);
  return Object.keys(props).map((name) => {
    const kind = fieldKindFor(name, props[name]?.type);
    const field: ToolField = {
      name,
      kind,
      required: required.has(name),
      description: props[name]?.description,
    };
    if (name === 'limit') field.max = 200;
    if (kind === 'number') field.min = 0;
    return field;
  });
}

/**
 * Coerce the form's string-keyed value bag into the typed `arguments` object
 * the MCP server expects: numbers as numbers, empty strings dropped. The form
 * stores datetimes already as RFC3339 strings.
 */
export function toArguments(
  fields: ToolField[],
  values: Record<string, string>,
): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const f of fields) {
    const raw = values[f.name];
    if (raw === undefined || raw === '') continue;
    if (f.kind === 'number') {
      const n = Number(raw);
      if (!Number.isNaN(n)) out[f.name] = n;
    } else {
      out[f.name] = raw;
    }
  }
  return out;
}

/** Datetime presets — value is a relative window applied to `now` on click. */
export const DATE_PRESETS: ReadonlyArray<{ label: string; ms: number }> = [
  { label: '15m', ms: 15 * 60_000 },
  { label: '1h', ms: 60 * 60_000 },
  { label: '24h', ms: 24 * 60 * 60_000 },
];

/** Build a curl invocation for the History "Copy as curl" action. The API key
 *  is intentionally a `$API_KEY` placeholder — never the real secret. */
export function toCurl(requestBody: string, tenant?: string): string {
  const compact = (() => {
    try {
      return JSON.stringify(JSON.parse(requestBody));
    } catch {
      return requestBody;
    }
  })();
  const lines = [
    "curl -sS http://localhost:8080/mcp \\",
    "  -H 'Content-Type: application/json' \\",
    '  -H "Authorization: Bearer $API_KEY" \\',
  ];
  if (tenant) lines.push(`  -H 'X-Tenant-ID: ${tenant}' \\`);
  lines.push(`  -d '${compact.replace(/'/g, "'\\''")}'`);
  return lines.join('\n');
}
