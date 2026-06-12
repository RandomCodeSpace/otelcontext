import type { ComponentType } from 'react'
import {
  DependenciesTab,
  OverviewTab,
  type InspectorTabContext,
} from './inspectorTabs'
import { WhyTab } from './WhyTab'
import { ImpactTab } from './ImpactTab'

// The Inspector tab registry. Adding a tab = one content component plus one
// entry here; ids double as the ?tab= deep-link values the palette uses.

export interface InspectorTabDef {
  id: string
  label: string
  Content: ComponentType<{ ctx: InspectorTabContext }>
}

export const INSPECTOR_TABS: readonly InspectorTabDef[] = [
  { id: 'overview', label: 'Overview', Content: OverviewTab },
  { id: 'why', label: 'Why', Content: WhyTab },
  { id: 'impact', label: 'Impact', Content: ImpactTab },
  { id: 'dependencies', label: 'Dependencies', Content: DependenciesTab },
]

/** Validate a ?tab= param against the registry. */
export function isInspectorTabId(value: string | null): value is string {
  return value !== null && INSPECTOR_TABS.some((tab) => tab.id === value)
}
