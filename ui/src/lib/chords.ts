// Global keyboard dispatch — a pure state machine so every rule is unit-
// testable without a DOM: ⌘/Ctrl+K palette (works while typing — the only
// global that does), '?' shortcut sheet, and the 'g m/t/l/h' go-to chords.
// '/' is deliberately absent: page-local filter handlers own it.

export type GlobalKeyAction =
  | { type: 'palette' }
  | { type: 'shortcuts' }
  | { type: 'navigate'; href: string }
  | null

export interface ChordState {
  /** Epoch ms when 'g' armed the chord; null = idle. */
  pendingG: number | null
}

export const INITIAL_CHORD_STATE: ChordState = { pendingG: null }

/** A second leniency beyond typical chord typing speed. */
export const CHORD_TIMEOUT_MS = 1200

/** The g-chord destinations: g h home, g m map, g t traces, g l logs. */
export const GO_TARGETS: Readonly<Record<string, string>> = {
  h: '/',
  m: '/map',
  t: '/traces',
  l: '/logs',
}

export interface KeyStroke {
  key: string
  metaKey: boolean
  ctrlKey: boolean
  altKey: boolean
  /** True when the event target is an editable element. */
  editable: boolean
}

const IDLE: ChordState = { pendingG: null }

export function keyAction(
  state: ChordState,
  stroke: KeyStroke,
  now: number,
): { state: ChordState; action: GlobalKeyAction } {
  const modified = stroke.metaKey || stroke.ctrlKey || stroke.altKey

  // ⌘K / Ctrl+K — the palette toggle, valid everywhere including inputs.
  if ((stroke.metaKey || stroke.ctrlKey) && stroke.key.toLowerCase() === 'k') {
    return { state: IDLE, action: { type: 'palette' } }
  }
  if (modified || stroke.editable) {
    return { state: IDLE, action: null }
  }

  if (stroke.key === '?') {
    return { state: IDLE, action: { type: 'shortcuts' } }
  }

  if (stroke.key === 'g') {
    return { state: { pendingG: now }, action: null }
  }

  if (
    state.pendingG !== null &&
    now - state.pendingG <= CHORD_TIMEOUT_MS &&
    stroke.key in GO_TARGETS
  ) {
    return { state: IDLE, action: { type: 'navigate', href: GO_TARGETS[stroke.key] } }
  }

  return { state: IDLE, action: null }
}

/** True when the key event originates from an editable element. */
export function isEditableTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false
  return (
    target.isContentEditable ||
    target.tagName === 'INPUT' ||
    target.tagName === 'TEXTAREA' ||
    target.tagName === 'SELECT'
  )
}
