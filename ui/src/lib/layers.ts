/**
 * Z-index contract. The design system's internal ladder is fixed and unexported:
 * dropdown 20 · tooltip/drawer/popover 50 · modal 100/101 · toast 200.
 *
 * Rule: custom floating UI MUST stay BELOW 50 so the library Tooltip/Drawer/Popover
 * always wins. Anything modal-tier must use the library Modal/Drawer/Tooltip —
 * never hand-pick a value in 100–200.
 */
export const LAYER = {
  /** Service-map hover stat chip — above the canvas, below DS tooltips. */
  mapHoverPanel: 10,
  /** In-canvas controls / sticky list headers. */
  canvasControls: 15,
} as const
