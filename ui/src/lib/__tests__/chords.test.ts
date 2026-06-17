import { describe, expect, it } from 'vitest'
import {
  CHORD_TIMEOUT_MS,
  INITIAL_CHORD_STATE,
  isEditableTarget,
  keyAction,
  type KeyStroke,
} from '../chords'

const stroke = (key: string, overrides: Partial<KeyStroke> = {}): KeyStroke => ({
  key,
  metaKey: false,
  ctrlKey: false,
  altKey: false,
  editable: false,
  ...overrides,
})

describe('keyAction — palette and shortcuts', () => {
  it('Cmd+K opens the palette', () => {
    const { action } = keyAction(INITIAL_CHORD_STATE, stroke('k', { metaKey: true }), 0)
    expect(action).toEqual({ type: 'palette' })
  })

  it('Ctrl+K opens the palette (also from editable targets)', () => {
    const { action } = keyAction(
      INITIAL_CHORD_STATE,
      stroke('k', { ctrlKey: true, editable: true }),
      0,
    )
    expect(action).toEqual({ type: 'palette' })
  })

  it('? opens the shortcut sheet, but never while typing', () => {
    expect(keyAction(INITIAL_CHORD_STATE, stroke('?'), 0).action).toEqual({
      type: 'shortcuts',
    })
    expect(
      keyAction(INITIAL_CHORD_STATE, stroke('?', { editable: true }), 0).action,
    ).toBeNull()
  })

  it('plain k or modified non-K keys do nothing', () => {
    expect(keyAction(INITIAL_CHORD_STATE, stroke('k'), 0).action).toBeNull()
    expect(
      keyAction(INITIAL_CHORD_STATE, stroke('m', { metaKey: true }), 0).action,
    ).toBeNull()
  })

  it('/ is left alone — page-local filter handlers own it', () => {
    expect(keyAction(INITIAL_CHORD_STATE, stroke('/'), 0).action).toBeNull()
  })
})

describe('keyAction — g go-to chords', () => {
  it('g then h/m navigates within the timeout', () => {
    const cases: Array<[string, string]> = [
      ['m', '/map'],
      ['h', '/'],
    ]
    for (const [key, href] of cases) {
      const { state } = keyAction(INITIAL_CHORD_STATE, stroke('g'), 1000)
      const { action, state: after } = keyAction(state, stroke(key), 1500)
      expect(action).toEqual({ type: 'navigate', href })
      expect(after.pendingG).toBeNull()
    }
  })

  it('the chord expires after the timeout', () => {
    const { state } = keyAction(INITIAL_CHORD_STATE, stroke('g'), 1000)
    const { action } = keyAction(state, stroke('m'), 1000 + CHORD_TIMEOUT_MS + 1)
    expect(action).toBeNull()
  })

  it('an unrelated key cancels the pending chord', () => {
    const { state } = keyAction(INITIAL_CHORD_STATE, stroke('g'), 0)
    const cancelled = keyAction(state, stroke('x'), 10)
    expect(cancelled.action).toBeNull()
    expect(cancelled.state.pendingG).toBeNull()
  })

  it('g pressed while typing does not arm the chord', () => {
    const { state } = keyAction(
      INITIAL_CHORD_STATE,
      stroke('g', { editable: true }),
      0,
    )
    expect(state.pendingG).toBeNull()
  })

  it('g g re-arms (last g wins)', () => {
    const first = keyAction(INITIAL_CHORD_STATE, stroke('g'), 0)
    const second = keyAction(first.state, stroke('g'), 500)
    expect(second.state.pendingG).toBe(500)
  })

  it('modifier combos cancel the pending chord', () => {
    const { state } = keyAction(INITIAL_CHORD_STATE, stroke('g'), 0)
    const { action, state: after } = keyAction(
      state,
      stroke('m', { ctrlKey: true }),
      10,
    )
    expect(action).toBeNull()
    expect(after.pendingG).toBeNull()
  })
})

describe('isEditableTarget', () => {
  it('flags inputs, textareas, selects and contenteditable', () => {
    expect(isEditableTarget(document.createElement('input'))).toBe(true)
    expect(isEditableTarget(document.createElement('textarea'))).toBe(true)
    expect(isEditableTarget(document.createElement('select'))).toBe(true)
    const div = document.createElement('div')
    expect(isEditableTarget(div)).toBe(false)
    expect(isEditableTarget(null)).toBe(false)
  })
})
