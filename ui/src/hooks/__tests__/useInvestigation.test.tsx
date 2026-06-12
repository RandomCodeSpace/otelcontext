import { describe, expect, it } from 'vitest'
import { act, renderHook } from '@testing-library/react'
import type { ReactNode } from 'react'
import { Router } from 'wouter'
import { memoryLocation } from 'wouter/memory-location'
import { useInvestigation } from '../useInvestigation'

function setup(path = '/map') {
  const memory = memoryLocation({ path, record: true })
  const wrapper = ({ children }: { children: ReactNode }) => (
    <Router hook={memory.hook} searchHook={memory.searchHook}>
      {children}
    </Router>
  )
  const rendered = renderHook(() => useInvestigation(), { wrapper })
  return { memory, rendered }
}

describe('useInvestigation', () => {
  it('starts closed with an empty trail', () => {
    const { rendered } = setup()
    expect(rendered.result.current.service).toBeNull()
    expect(rendered.result.current.trail).toEqual([])
  })

  it('parses service and trail from the URL (reload survival)', () => {
    const { rendered } = setup('/map?service=payments&trail=svc:checkout,svc:payments')
    expect(rendered.result.current.service).toBe('payments')
    expect(rendered.result.current.trail).toEqual([
      { kind: 'svc', id: 'checkout' },
      { kind: 'svc', id: 'payments' },
    ])
  })

  it('openService pushes the trail and sets ?service in one update', () => {
    const { rendered } = setup()
    act(() => rendered.result.current.openService('checkout'))
    expect(rendered.result.current.service).toBe('checkout')
    expect(rendered.result.current.trail).toEqual([{ kind: 'svc', id: 'checkout' }])

    act(() => rendered.result.current.openService('payments'))
    expect(rendered.result.current.service).toBe('payments')
    expect(rendered.result.current.trail).toHaveLength(2)
  })

  it('re-opening the current service does not grow the trail', () => {
    const { rendered } = setup('/map?service=a&trail=svc:a')
    act(() => rendered.result.current.openService('a'))
    expect(rendered.result.current.trail).toHaveLength(1)
  })

  it('closeInspector clears ?service but keeps the trail', () => {
    const { rendered } = setup('/map?service=a&trail=svc:a,svc:b')
    act(() => rendered.result.current.closeInspector())
    expect(rendered.result.current.service).toBeNull()
    expect(rendered.result.current.trail).toHaveLength(2)
  })

  it('popToFrame truncates and re-activates a svc frame', () => {
    const { rendered } = setup('/map?service=c&trail=svc:a,svc:b,svc:c')
    act(() => rendered.result.current.popToFrame(0))
    expect(rendered.result.current.trail).toEqual([{ kind: 'svc', id: 'a' }])
    expect(rendered.result.current.service).toBe('a')
  })

  it('popOne pops the top frame and activates the frame below', () => {
    const { rendered } = setup('/map?service=b&trail=svc:a,svc:b')
    act(() => rendered.result.current.popOne())
    expect(rendered.result.current.trail).toEqual([{ kind: 'svc', id: 'a' }])
    expect(rendered.result.current.service).toBe('a')
  })

  it('popOne on the last frame empties the trail and closes the inspector', () => {
    const { rendered } = setup('/map?service=a&trail=svc:a')
    act(() => rendered.result.current.popOne())
    expect(rendered.result.current.trail).toEqual([])
    expect(rendered.result.current.service).toBeNull()
  })

  it('popOne with an empty trail is a no-op', () => {
    const { memory, rendered } = setup('/map')
    const before = memory.history.length
    act(() => rendered.result.current.popOne())
    expect(memory.history.length).toBe(before)
  })

  it('popping to a trace frame truncates without opening the inspector', () => {
    const { rendered } = setup('/map?service=b&trail=svc:a,trace:t1,svc:b')
    act(() => rendered.result.current.popToFrame(1))
    expect(rendered.result.current.trail).toEqual([
      { kind: 'svc', id: 'a' },
      { kind: 'trace', id: 't1' },
    ])
    expect(rendered.result.current.service).toBeNull()
  })

  it('preserves the current route path and unrelated params', () => {
    const { memory, rendered } = setup('/map?status=critical')
    act(() => rendered.result.current.openService('checkout'))
    expect(memory.history.at(-1)).toMatch(/^\/map\?/)
    expect(memory.history.at(-1)).toContain('status=critical')
  })

  it('openService clears a stale ?tab so drill-downs land on Overview', () => {
    const { memory, rendered } = setup('/map?service=a&tab=why&trail=svc:a')
    act(() => rendered.result.current.openService('b'))
    expect(memory.history.at(-1)).not.toContain('tab=')
  })

  it('openService with a tab targets that inspector tab', () => {
    const { memory, rendered } = setup('/map')
    act(() => rendered.result.current.openService('checkout', 'why'))
    expect(memory.history.at(-1)).toContain('tab=why')
    expect(rendered.result.current.service).toBe('checkout')
  })

  it('openTrace pushes a trace frame and navigates to /traces', () => {
    const { memory, rendered } = setup('/map?service=a&trail=svc:a')
    act(() => rendered.result.current.openTrace('t1'))
    const dest = memory.history.at(-1)!
    expect(dest).toMatch(/^\/traces\?/)
    expect(dest).toContain('trace=t1')
    expect(dest).toContain('trail=svc%3Aa%2Ctrace%3At1')
    expect(dest).not.toContain('service=')
  })

  it('openTrace pushes history (Back returns to the inspector)', () => {
    const { memory, rendered } = setup('/map?service=a')
    const before = memory.history.length
    act(() => rendered.result.current.openTrace('t1'))
    expect(memory.history.length).toBe(before + 1)
  })

  it('uses replace navigation (no history spam)', () => {
    const { memory, rendered } = setup('/map')
    act(() => rendered.result.current.openService('a'))
    act(() => rendered.result.current.openService('b'))
    // replace rewrites the top entry — drill-downs never stack history.
    expect(memory.history).toHaveLength(1)
    expect(memory.history[0]).toContain('service=b')
  })
})
