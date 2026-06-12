import { useEffect, useRef } from 'react'
import { useLocation } from 'wouter'
import {
  INITIAL_CHORD_STATE,
  isEditableTarget,
  keyAction,
  type ChordState,
} from '@/lib/chords'

export interface GlobalKeyHandlers {
  /** ⌘K / Ctrl+K — toggle the command palette. */
  onPalette: () => void
  /** '?' — open the shortcut sheet. */
  onShortcuts: () => void
}

/**
 * Window-level keyboard dispatch: palette toggle, shortcut sheet and the
 * 'g m/t/l/h' go-to chords. The rules live in lib/chords (pure, tested);
 * this hook only wires DOM events and wouter navigation.
 */
export function useGlobalKeys({ onPalette, onShortcuts }: GlobalKeyHandlers) {
  const [, navigate] = useLocation()
  const chord = useRef<ChordState>(INITIAL_CHORD_STATE)

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      const { state, action } = keyAction(
        chord.current,
        {
          key: event.key,
          metaKey: event.metaKey,
          ctrlKey: event.ctrlKey,
          altKey: event.altKey,
          editable: isEditableTarget(event.target),
        },
        Date.now(),
      )
      chord.current = state
      if (!action) return
      event.preventDefault()
      if (action.type === 'palette') onPalette()
      else if (action.type === 'shortcuts') onShortcuts()
      else navigate(action.href)
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [navigate, onPalette, onShortcuts])
}
