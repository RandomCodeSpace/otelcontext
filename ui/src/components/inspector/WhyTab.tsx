import { useMemo } from 'react'
import { useTriageTool } from '@/hooks/useTriageTool'
import { rootCauseQueryOptions } from '@/lib/triageVerbs'
import type { RankedCause, SpanNode } from '@/types/api'
import type { InspectorTabContext } from './inspectorTabs'
import VerbTabFrame from './VerbTabFrame'
import styles from './ServiceInspector.module.css'

// "Why" tab — MCP root_cause_analysis as a human verb. Ranked probable
// causes with a relative score bar, evidence bullets, and the error-chain
// spans listed as read-only context. (Full trace drill-down is an AI-agent
// surface via the trace_graph MCP tool — the human UI no longer ships a
// dedicated traces screen, so the spans are non-interactive here.)

function shortId(id: string): string {
  return id.length > 12 ? `${id.slice(0, 12)}…` : id
}

function ChainSpanRow({ span }: Readonly<{ span: SpanNode }>) {
  return (
    <li className={styles.chainSpan}>
      <span className={styles.depName}>
        {span.service} · {span.operation}
      </span>
      <span className={styles.depMeta}>trace {shortId(span.trace_id)}</span>
    </li>
  )
}

function CauseCard({
  cause,
  rank,
  maxScore,
}: Readonly<{
  cause: RankedCause
  rank: number
  maxScore: number
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
              <ChainSpanRow key={span.id} span={span} />
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
              />
            ))}
          </ol>
        </section>
      )}
    </VerbTabFrame>
  )
}
