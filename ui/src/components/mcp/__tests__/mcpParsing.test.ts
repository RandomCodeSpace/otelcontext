import { describe, it, expect } from 'vitest';
import {
  extractToolText,
  findRootCause,
  formatStreamEvent,
  parseToolResult,
} from '../mcpTypes';
import {
  fieldKindFor,
  fieldsForTool,
  toArguments,
  toCurl,
} from '../toolFields';
import type { McpToolResult } from '../../../lib/mcpClient';
import type { McpTool } from '../../../lib/mcpClient';

describe('extractToolText', () => {
  it('reads content[0].text (GraphRAG tools)', () => {
    const r: McpToolResult = { content: [{ type: 'text', text: '[1,2,3]' }] };
    expect(extractToolText(r)).toBe('[1,2,3]');
  });

  it('reads content[0].resource.text (search_logs / trace fallback)', () => {
    const r: McpToolResult = {
      content: [{ type: 'resource', resource: { uri: 'x', mimeType: 'application/json', text: '{"total":0}' } } as never],
    };
    expect(extractToolText(r)).toBe('{"total":0}');
  });

  it('returns empty string for empty content', () => {
    expect(extractToolText({ content: [] })).toBe('');
    expect(extractToolText({})).toBe('');
  });
});

describe('findRootCause', () => {
  it('finds a nested root_cause block', () => {
    const payload = { foo: [{ bar: { root_cause: { service: 'svc', operation: 'op', error_message: 'boom', span_id: 's', trace_id: 't' } } }] };
    const rc = findRootCause(payload);
    expect(rc?.service).toBe('svc');
    expect(rc?.error_message).toBe('boom');
  });

  it('returns null when absent', () => {
    expect(findRootCause({ a: 1, b: [2, 3] })).toBeNull();
  });
});

describe('parseToolResult', () => {
  it('decodes a success payload and flags isError false', () => {
    const r: McpToolResult = { content: [{ type: 'text', text: '{"total":2}' }] };
    const p = parseToolResult(r);
    expect(p.isError).toBe(false);
    expect(p.payload).toEqual({ total: 2 });
  });

  it('surfaces tool-level errors with the text intact', () => {
    const r: McpToolResult = { isError: true, content: [{ type: 'text', text: 'Error: unknown tool' }] };
    const p = parseToolResult(r);
    expect(p.isError).toBe(true);
    expect(p.text).toContain('unknown tool');
    expect(p.payload).toBeUndefined();
  });
});

describe('fieldKindFor', () => {
  it('maps names to controls', () => {
    expect(fieldKindFor('service')).toBe('service');
    expect(fieldKindFor('service_name')).toBe('service');
    expect(fieldKindFor('severity')).toBe('severity');
    expect(fieldKindFor('time_range')).toBe('duration');
    expect(fieldKindFor('since')).toBe('datetime');
    expect(fieldKindFor('limit')).toBe('number');
    expect(fieldKindFor('query')).toBe('text');
    expect(fieldKindFor('unknown', 'number')).toBe('number');
  });
});

describe('fieldsForTool', () => {
  const searchLogs: McpTool = {
    name: 'search_logs',
    inputSchema: {
      type: 'object',
      properties: {
        query: { type: 'string', description: 'full text' },
        limit: { type: 'number', description: 'max rows' },
        service: { type: 'string' },
      },
    },
  };

  it('derives fields, clamps limit max to 200, marks required', () => {
    const fields = fieldsForTool({
      ...searchLogs,
      inputSchema: { ...searchLogs.inputSchema!, required: ['query'] },
    });
    const limit = fields.find((f) => f.name === 'limit');
    expect(limit?.max).toBe(200);
    expect(fields.find((f) => f.name === 'query')?.required).toBe(true);
    expect(fields.find((f) => f.name === 'service')?.kind).toBe('service');
  });
});

describe('toArguments', () => {
  it('coerces numbers, drops empties', () => {
    const fields = fieldsForTool({
      name: 't',
      inputSchema: { type: 'object', properties: { limit: { type: 'number' }, query: { type: 'string' }, service: { type: 'string' } } },
    });
    const args = toArguments(fields, { limit: '50', query: '', service: 'cart' });
    expect(args).toEqual({ limit: 50, service: 'cart' });
  });
});

describe('formatStreamEvent', () => {
  it('summarises a graph snapshot notification (stringified params.data)', () => {
    const snap = JSON.stringify({
      Nodes: {
        a: { Status: 'healthy' },
        b: { Status: 'degraded' },
        c: { Status: 'critical' },
        d: { Status: 'healthy' },
      },
      Edges: [{}, {}],
    });
    const raw = JSON.stringify({
      jsonrpc: '2.0',
      method: 'notifications/resources/updated',
      params: { uri: 'OtelContext://system/graph', data: snap },
    });
    const line = formatStreamEvent(raw);
    expect(line.text).toBe('graph · 4 svc · 2 edges · healthy 2 / degraded 1 / critical 1');
    expect(line.type).toBe('warn'); // has a critical node
  });

  it('uses stdout tone when no critical nodes', () => {
    const raw = JSON.stringify({
      method: 'notifications/resources/updated',
      params: { data: JSON.stringify({ Nodes: { a: { Status: 'healthy' } }, Edges: [] }) },
    });
    const line = formatStreamEvent(raw);
    expect(line.type).toBe('stdout');
    expect(line.text).toContain('1 svc');
  });

  it('renders the handshake event', () => {
    const raw = JSON.stringify({ jsonrpc: '2.0', method: 'notifications/initialized', params: {} });
    expect(formatStreamEvent(raw)).toEqual({ type: 'info', text: 'handshake · stream initialized' });
  });

  it('falls back to the method name, then raw text, then empty', () => {
    expect(formatStreamEvent(JSON.stringify({ method: 'notifications/ping' })).text).toBe('notifications/ping');
    expect(formatStreamEvent('not json').text).toBe('not json');
    expect(formatStreamEvent('').text).toBe('(empty event)');
  });
});

describe('toCurl', () => {
  it('uses a $API_KEY placeholder, never a real key', () => {
    const body = JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'tools/call', params: { name: 'search_logs', arguments: { query: 'x' } } });
    const curl = toCurl(body, 'acme');
    expect(curl).toContain('Bearer $API_KEY');
    expect(curl).toContain("X-Tenant-ID: acme");
    expect(curl).not.toContain('secret');
  });
});
