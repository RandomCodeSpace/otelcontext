// Bottom-sheet snap resolution for the xs Service Inspector.
// Snap points: 50% and 92% of the viewport. A drag must travel 15% of the
// viewport height to change state; dragging down past the half snap
// dismisses the sheet.

export type SheetSnap = 50 | 92

export function nextSheetState(
  current: SheetSnap,
  /** Pointer travel in px — positive = dragged down. */
  deltaY: number,
  viewportH: number,
): SheetSnap | 'dismiss' {
  const threshold = viewportH * 0.15
  if (Math.abs(deltaY) < threshold) return current
  if (deltaY < 0) return 92 // dragged up
  return current === 92 ? 50 : 'dismiss'
}
