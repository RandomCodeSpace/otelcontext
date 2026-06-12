import { describe, expect, it } from 'vitest'
import {
  extractToolText,
  parseAnomalies,
  parseImpactResult,
  parseRankedCauses,
} from '../mcpResults'
import type { McpToolResult } from '../mcpClient'

const text = (t: string): McpToolResult => ({
  content: [{ type: 'text', text: t }],
})

describe('extractToolText', () => {
  it('reads the first text content block', () => {
    expect(extractToolText(text('[1,2]'))).toBe('[1,2]')
  })

  it('reads resource content (search_logs style)', () => {
    const r = {
      content: [
        {
          type: 'resource',
          resource: { uri: 'x', mimeType: 'application/json', text: '{"a":1}' },
        },
      ],
    } as unknown as McpToolResult
    expect(extractToolText(r)).toBe('{"a":1}')
  })

  it('returns empty string for null / empty content', () => {
    expect(extractToolText(null)).toBe('')
    expect(extractToolText({})).toBe('')
    expect(extractToolText({ content: [] })).toBe('')
  })
})

describe('parseRankedCauses', () => {
  it('parses a RankedCause array and sorts by score descending', () => {
    const causes = [
      { service: 'a', operation: 'op-a', score: 1, evidence: ['e1'] },
      { service: 'b', operation: 'op-b', score: 3, evidence: ['e2'] },
    ]
    const out = parseRankedCauses(text(JSON.stringify(causes)))
    expect(out.map((c) => c.service)).toEqual(['b', 'a'])
  })

  it('returns [] for the Go "null" marshal of an empty slice', () => {
    expect(parseRankedCauses(text('null'))).toEqual([])
  })

  it('returns [] for non-JSON text and non-array JSON', () => {
    expect(parseRankedCauses(text('Error: nope'))).toEqual([])
    expect(parseRankedCauses(text('{"service":"x"}'))).toEqual([])
  })

  it('returns [] when result is null', () => {
    expect(parseRankedCauses(null)).toEqual([])
  })
})

describe('parseImpactResult', () => {
  it('parses an ImpactResult object', () => {
    const impact = {
      service: 'checkout',
      affected_services: [
        { service: 'payments', depth: 1, call_count: 10, impact_score: 0.4 },
      ],
      total_downstream: 1,
    }
    const out = parseImpactResult(text(JSON.stringify(impact)))
    expect(out?.service).toBe('checkout')
    expect(out?.affected_services).toHaveLength(1)
  })

  it('normalizes a missing affected_services (Go omits empty slices as null)', () => {
    const out = parseImpactResult(
      text('{"service":"x","affected_services":null,"total_downstream":0}'),
    )
    expect(out?.affected_services).toEqual([])
  })

  it('returns null for null / non-object / malformed payloads', () => {
    expect(parseImpactResult(null)).toBeNull()
    expect(parseImpactResult(text('null'))).toBeNull()
    expect(parseImpactResult(text('[1]'))).toBeNull()
    expect(parseImpactResult(text('not json'))).toBeNull()
  })
})

describe('parseAnomalies', () => {
  it('parses the anomaly array from content text', () => {
    const anomalies = [
      {
        id: 'a1',
        type: 'error_spike',
        severity: 'critical',
        service: 'svc',
        evidence: 'errors x10',
        timestamp: '2026-06-12T00:00:00Z',
      },
    ]
    expect(parseAnomalies(text(JSON.stringify(anomalies)))).toHaveLength(1)
  })

  it('tolerates a structured anomalies field', () => {
    const r = { anomalies: [{ id: 'a' }] } as unknown as McpToolResult
    expect(parseAnomalies(r)).toHaveLength(1)
  })

  it('returns [] for null, "null" text and junk', () => {
    expect(parseAnomalies(null)).toEqual([])
    expect(parseAnomalies(text('null'))).toEqual([])
    expect(parseAnomalies(text('Error: boom'))).toEqual([])
  })
})
