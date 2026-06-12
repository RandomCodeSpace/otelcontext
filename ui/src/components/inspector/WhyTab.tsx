import { useMemo } from 'react'
import { ChevronRight } from 'lucide-react'
import { useTriageTool } from '@/hooks/useTriageTool'
import { rootCauseQueryOptions } from '@/lib/triageVerbs'
import type { RankedCause, SpanNode } from '@/types/api'
import type { InspectorTabContext } from './inspectorTabs'
import VerbTabFrame from './VerbTabFrame'
import styles from './ServiceInspector.module.css'

// "Why" tab — MCP root_cause_analysis as a human verb. Ranked probable
// causes with a relative score bar, evidence bullets, and the error-chain
// spans deep-linking into /traces (pushing the investigation trail).

function shortId(id: string): string {
  return id.length > 12 ? `${id.slice(0, 12)}…` : id
}

function ChainSpanRow({
  span,
  openTrace,
}: Readonly<{ span: SpanNode; openTrace: (id: string) => void }>) {
  return (
    <li>
      <button
        type="button"
        className={styles.depRow}
        onClick={() => openTrace(span.trace_id)}
        aria-label={`Open trace ${span.trace_id}: ${span.service} · ${span.operation}`}
      >
        <span className={styles.depName}>
          {span.service} · {span.operation}
        </span>
        <span className={styles.depMeta}>trace {shortId(span.trace_id)}</span>
        <ChevronRight size={13} className={styles.depChevron} aria-hidden="true" />
      </button>
    </li>
  )
}

function CauseCard({
  cause,
  rank,
  maxScore,
  openTrace,
}: Readonly<{
  cause: RankedCause
  rank: number
  maxScore: number
  openTrace: (id: string) => void
}>) {
  const frac = maxScore > 0 ? Math.max(0, cause.score) / maxScore : 0
  return (
    <li className={styles.causeCard}>
      <div className={styles.causeHead}>
        <span className={styles.causeRank}>#{rank}</span>
        <span className={styles.causeService}>
          {cause.service}
          {cause.operation && (
            <span className={styles.causeOp}> · {cause.operation}</span>
          )}
        </span>
        <span className={styles.causeScore}>{cause.score.toFixed(1)}</span>
      </div>
      <div className={styles.scoreTrack} aria-hidden="true">
        <div
          className={styles.scoreFill}
          style={{ width: `${Math.min(100, frac * 100)}%` }}
        />
      </div>
      {cause.evidence.length > 0 && (
        <ul className={styles.evidenceList}>
          {cause.evidence.map((line) => (
            <li key={line} className={styles.evidenceItem}>
              {line}
            </li>
          ))}
        </ul>
      )}
      {cause.error_chain && cause.error_chain.length > 0 && (
        <>
          <h4 className={styles.sectionTitle}>Error chain</h4>
          <ul className={styles.depList}>
            {cause.error_chain.map((span) => (
              <ChainSpanRow key={span.id} span={span} openTrace={openTrace} />
            ))}
          </ul>
        </>
      )}
    </li>
  )
}

export function WhyTab({ ctx }: Readonly<{ ctx: InspectorTabContext }>) {
  const service = ctx.node.id
  const options = useMemo(() => rootCauseQueryOptions(service), [service])
  const tool = useTriageTool(options)
  const causes = tool.data ?? []
  const maxScore = causes.length > 0 ? Math.max(...causes.map((c) => c.score)) : 0

  return (
    <VerbTabFrame
      status={tool.status}
      error={tool.error}
      runLabel="Run root-cause analysis"
      hint={`Trace error chains upstream from ${service} and rank the probable causes with evidence.`}
      onRun={tool.run}
      onCancel={() => void tool.cancel()}
    >
      {causes.length === 0 ? (
        <>
          <p className={styles.quiet}>
            No probable causes in the window — no error chains or anomalies
            implicate an upstream service right now.
          </p>
          <button type="button" className={styles.stateAction} onClick={tool.run}>
            Re-run
          </button>
        </>
      ) : (
        <section aria-label="Ranked probable causes">
          <ol className={styles.causeList}>
            {causes.map((cause, i) => (
              <CauseCard
                key={`${cause.service}|${cause.operation}`}
                cause={cause}
                rank={i + 1}
                maxScore={maxScore}
                openTrace={ctx.openTrace}
              />
            ))}
          </ol>
        </section>
      )}
    </VerbTabFrame>
  )
}
