import { describe, expect, it } from 'vitest'
import { nextSheetState } from '../sheet'

// viewport 800px → threshold = 120px (15%)
const VH = 800

describe('nextSheetState', () => {
  it('stays put for small drags', () => {
    expect(nextSheetState(50, 40, VH)).toBe(50)
    expect(nextSheetState(92, -40, VH)).toBe(92)
    expect(nextSheetState(92, 100, VH)).toBe(92)
  })

  it('drag up from half snaps to full', () => {
    expect(nextSheetState(50, -200, VH)).toBe(92)
  })

  it('drag down from full snaps to half', () => {
    expect(nextSheetState(92, 200, VH)).toBe(50)
  })

  it('drag down from half dismisses', () => {
    expect(nextSheetState(50, 200, VH)).toBe('dismiss')
  })

  it('drag up from full stays full', () => {
    expect(nextSheetState(92, -300, VH)).toBe(92)
  })
})
