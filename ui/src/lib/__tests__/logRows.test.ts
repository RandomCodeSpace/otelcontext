import { describe, expect, it } from 'vitest'
import {
  countNewSince,
  filterBySeverity,
  formatLogTime,
  parseAttributes,
  severityCounts,
  severityTone,
} from '../logRows'
import type { LogEntry } from '@/types/api'

let autoId = 0
function log(over: Partial<LogEntry> = {}): LogEntry {
  autoId += 1
  return {
    id: autoId,
    trace_id: '',
    span_id: '',
    severity: 'INFO',
    body: `line ${autoId}`,
    service_name: 'svc-a',
    attributes_json: '',
    timestamp: '2026-06-11T10:00:00.123Z',
    ...over,
  }
}

describe('severityTone', () => {
  it('maps severities to badge tones', () => {
    expect(severityTone('ERROR')).toBe('crit')
    expect(severityTone('FATAL')).toBe('crit')
    expect(severityTone('WARN')).toBe('warn')
    expect(severityTone('WARNING')).toBe('warn')
    expect(severityTone('INFO')).toBe('ok')
    expect(severityTone('DEBUG')).toBe('unknown')
    expect(severityTone('TRACE')).toBe('unknown')
  })

  it('is case-insensitive and tolerant of OTLP SEVERITY_NUMBER_* names', () => {
    expect(severityTone('error')).toBe('crit')
    expect(severityTone('SEVERITY_NUMBER_ERROR')).toBe('crit')
    expect(severityTone('SEVERITY_NUMBER_WARN')).toBe('warn')
    expect(severityTone('')).toBe('unknown')
    expect(severityTone('whatever')).toBe('unknown')
  })
})

describe('formatLogTime', () => {
  it('renders HH:mm:ss.SSS in local time', () => {
    const d = new Date('2026-06-11T10:02:03.045Z')
    const expected = [
      String(d.getHours()).padStart(2, '0'),
      String(d.getMinutes()).padStart(2, '0'),
      String(d.getSeconds()).padStart(2, '0'),
    ].join(':')
    expect(formatLogTime('2026-06-11T10:02:03.045Z')).toBe(`${expected}.045`)
  })

  it('returns a dash for invalid timestamps', () => {
    expect(formatLogTime('garbage')).toBe('—')
  })
})

describe('filterBySeverity', () => {
  const logs = [
    log({ severity: 'ERROR' }),
    log({ severity: 'WARN' }),
    log({ severity: 'INFO' }),
  ]

  it('passes everything through when no severity is selected', () => {
    expect(filterBySeverity(logs, null)).toHaveLength(3)
  })

  it('keeps only the selected normalized severity', () => {
    const errors = filterBySeverity(logs, 'ERROR')
    expect(errors).toHaveLength(1)
    expect(errors[0].severity).toBe('ERROR')
  })

  it('matches case-insensitively', () => {
    expect(filterBySeverity([log({ severity: 'error' })], 'ERROR')).toHaveLength(1)
  })
})

describe('severityCounts', () => {
  it('counts normalized severities', () => {
    const counts = severityCounts([
      log({ severity: 'ERROR' }),
      log({ severity: 'error' }),
      log({ severity: 'WARN' }),
      log({ severity: 'INFO' }),
      log({ severity: 'DEBUG' }),
    ])
    expect(counts.ERROR).toBe(2)
    expect(counts.WARN).toBe(1)
    expect(counts.INFO).toBe(1)
    expect(counts.DEBUG).toBe(1)
  })

  it('returns zeroed counts for an empty buffer', () => {
    const counts = severityCounts([])
    expect(counts.ERROR).toBe(0)
    expect(counts.WARN).toBe(0)
  })
})

describe('countNewSince', () => {
  it('reports the delta between two monotonic totals', () => {
    expect(countNewSince(120, 100)).toBe(20)
  })

  it('never goes negative', () => {
    expect(countNewSince(80, 100)).toBe(0)
  })
})

describe('parseAttributes', () => {
  it('parses a JSON object into entries', () => {
    expect(parseAttributes('{"http.method":"GET","retries":2}')).toEqual([
      ['http.method', 'GET'],
      ['retries', '2'],
    ])
  })

  it('stringifies nested values compactly', () => {
    expect(parseAttributes('{"a":{"b":1}}')).toEqual([['a', '{"b":1}']])
  })

  it('returns null for empty or invalid JSON', () => {
    expect(parseAttributes('')).toBeNull()
    expect(parseAttributes('not json')).toBeNull()
    expect(parseAttributes('[1,2]')).toBeNull() // not an object
  })

  it('parses the OTLP key/value-list shape the ingester writes', () => {
    // internal/ingest/otlp.go marshals []*commonpb.KeyValue directly.
    const raw = JSON.stringify([
      { key: 'http.method', value: { Value: { StringValue: 'GET' } } },
    ])
    const entries = parseAttributes(raw)
    expect(entries).not.toBeNull()
    expect(entries![0][0]).toBe('http.method')
    expect(entries![0][1]).toContain('GET')
  })
})
