import ToneBadge, { type BadgeTone } from '@/components/common/ToneBadge'
import type { CoverageType } from '@/types/api'

const TONE: Record<CoverageType, BadgeTone> = {
  full: 'ok',
  sampled: 'warn',
  exemplar: 'unknown',
}

/**
 * Labels a surface with the data-coverage vocabulary from #164:
 * full | sampled | exemplar. Absent coverage renders nothing — legacy
 * responses without the field must look exactly as they did before.
 * The coverage_note (e.g. "missing exemplars do not imply zero events")
 * rides as the tooltip.
 */
export default function CoverageBadge({
  coverage,
  note,
}: Readonly<{ coverage?: CoverageType; note?: string }>) {
  if (!coverage) return null
  return (
    <span title={note}>
      <ToneBadge tone={TONE[coverage]}>{coverage}</ToneBadge>
    </span>
  )
}
