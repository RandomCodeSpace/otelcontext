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
  })

  it('exposes the triage actions in order', () => {
    expect(PALETTE_ACTIONS.map((a) => a.id)).toEqual(['root-cause', 'impact'])
  })
})

describe('serviceCommand', () => {
  it('root-cause opens the inspector Why tab and prefetches the RPC', () => {
    expect(serviceCommand('root-cause', 'checkout')).toEqual({
      service: 'checkout',
      tab: 'why',
      prefetch: 'root_cause_analysis',
    })
  })

  it('impact opens the inspector Impact tab and prefetches the RPC', () => {
    expect(serviceCommand('impact', 'checkout')).toEqual({
      service: 'checkout',
      tab: 'impact',
      prefetch: 'impact_analysis',
    })
  })
})
