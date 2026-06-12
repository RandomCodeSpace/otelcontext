// Command-palette domain logic, kept pure and DOM-free: the two-page state
// (root ↔ service picker for a pending action) and the command each
// selection produces. The cmdk rendering shell lives in
// components/palette/CommandPalette.tsx.

export type PaletteActionId = 'root-cause' | 'impact' | 'search-logs'

export interface PaletteActionDef {
  id: PaletteActionId
  label: string
}

/** The triage verbs, in triage-loop order: why → blast radius → evidence. */
export const PALETTE_ACTIONS: readonly PaletteActionDef[] = [
  { id: 'root-cause', label: 'Root cause analysis…' },
  { id: 'impact', label: 'Blast radius…' },
  { id: 'search-logs', label: 'Search logs…' },
]

export type PalettePage =
  | { id: 'root' }
  | { id: 'service-pick'; action: PaletteActionId }

export const ROOT_PAGE: PalettePage = { id: 'root' }

export function pickAction(action: PaletteActionId): PalettePage {
  return { id: 'service-pick', action }
}

/** Escape inside the picker backs out one page; at the root it closes. */
export function escapeBehavior(page: PalettePage): 'back' | 'close' {
  return page.id === 'service-pick' ? 'back' : 'close'
}

const PICK_PLACEHOLDERS: Record<PaletteActionId, string> = {
  'root-cause': 'Root cause of which service?',
  impact: 'Blast radius of which service?',
  'search-logs': 'Search logs of which service?',
}

export function pagePlaceholder(page: PalettePage): string {
  return page.id === 'root'
    ? 'Type a command or search services…'
    : PICK_PLACEHOLDERS[page.action]
}

/** What executing an action against a chosen service means. */
export type PaletteCommand =
  | {
      kind: 'inspect'
      service: string
      tab: 'why' | 'impact'
      prefetch: 'root_cause_analysis' | 'impact_analysis'
    }
  | { kind: 'navigate'; href: string }

export function serviceCommand(
  action: PaletteActionId,
  service: string,
): PaletteCommand {
  switch (action) {
    case 'root-cause':
      return {
        kind: 'inspect',
        service,
        tab: 'why',
        prefetch: 'root_cause_analysis',
      }
    case 'impact':
      return {
        kind: 'inspect',
        service,
        tab: 'impact',
        prefetch: 'impact_analysis',
      }
    case 'search-logs':
      return {
        kind: 'navigate',
        href: `/logs?service=${encodeURIComponent(service)}`,
      }
  }
}
