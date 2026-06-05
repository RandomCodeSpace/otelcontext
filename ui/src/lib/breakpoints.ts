/**
 * Responsive breakpoints. Mirrors @ossrandom/design-system's single media query
 * (max-width: 768px, pointer: coarse) plus its container tokens. Pass these to
 * `useMediaQuery` — the library exposes no JS breakpoint constants.
 */
export const BP = {
  /** The library's own switch point — single source for "is mobile/coarse". */
  coarse: '(max-width: 768px), (pointer: coarse)',
  tablet: '(max-width: 960px)',
  desktop: '(min-width: 961px)',
  wide: '(min-width: 1281px)',
} as const

export type Breakpoint = keyof typeof BP
