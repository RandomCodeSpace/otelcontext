import type { ComponentType } from 'react'
import {
  DependenciesTab,
  OverviewTab,
  type InspectorTabContext,
} from './inspectorTabs'

// The Inspector tab registry — the extension seam for later phases:
// "Why" (MCP root_cause_analysis) and "Impact" (MCP impact_analysis) ship
// by adding their content component to inspectorTabs.tsx and one entry here.

export interface InspectorTabDef {
  id: string
  label: string
  Content: ComponentType<{ ctx: InspectorTabContext }>
}

export const INSPECTOR_TABS: readonly InspectorTabDef[] = [
  { id: 'overview', label: 'Overview', Content: OverviewTab },
  { id: 'dependencies', label: 'Dependencies', Content: DependenciesTab },
]
