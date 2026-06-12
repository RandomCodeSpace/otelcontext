import { describe, expect, it } from 'vitest'
import {
  PALETTE_ACTIONS,
  ROOT_PAGE,
  escapeBehavior,
  pagePlaceholder,
  pickAction,
  serviceCommand,
} from '../palette'

describe('palette pages', () => {
  it('picking an action moves to the service-pick page', () => {
    const page = pickAction('root-cause')
    expect(page).toEqual({ id: 'service-pick', action: 'root-cause' })
  })

  it('Escape backs out of the service-pick page instead of closing', () => {
    expect(escapeBehavior(pickAction('impact'))).toBe('back')
    expect(escapeBehavior(ROOT_PAGE)).toBe('close')
  })

  it('placeholders explain the pending action', () => {
    expect(pagePlaceholder(ROOT_PAGE)).toMatch(/type a command/i)
    expect(pagePlaceholder(pickAction('root-cause'))).toMatch(/root cause/i)
    expect(pagePlaceholder(pickAction('impact'))).toMatch(/blast radius/i)
    expect(pagePlaceholder(pickAction('search-logs'))).toMatch(/logs/i)
  })

  it('exposes the three triage actions in order', () => {
    expect(PALETTE_ACTIONS.map((a) => a.id)).toEqual([
      'root-cause',
      'impact',
      'search-logs',
    ])
  })
})

describe('serviceCommand', () => {
  it('root-cause opens the inspector Why tab and prefetches the RPC', () => {
    expect(serviceCommand('root-cause', 'checkout')).toEqual({
      kind: 'inspect',
      service: 'checkout',
      tab: 'why',
      prefetch: 'root_cause_analysis',
    })
  })

  it('impact opens the inspector Impact tab and prefetches the RPC', () => {
    expect(serviceCommand('impact', 'checkout')).toEqual({
      kind: 'inspect',
      service: 'checkout',
      tab: 'impact',
      prefetch: 'impact_analysis',
    })
  })

  it('search-logs deep-links the /logs service search', () => {
    expect(serviceCommand('search-logs', 'svc with spaces')).toEqual({
      kind: 'navigate',
      href: '/logs?service=svc%20with%20spaces',
    })
  })
})
