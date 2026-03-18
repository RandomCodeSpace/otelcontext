import { useEffect, useRef } from 'react'
import * as echarts from 'echarts'
import type { EChartsOption } from 'echarts'

interface Props {
  option: EChartsOption
  style?: React.CSSProperties
  className?: string
  /** Event name → handler map. Handlers always reference the latest closure. */
  onEvents?: Record<string, (params: unknown) => void>
  /** Receives the ECharts instance so callers can dispatchAction etc. */
  chartRef?: React.MutableRefObject<echarts.ECharts | null>
}

/**
 * Lightweight ECharts wrapper.
 * - Initialises once on mount; disposes on unmount.
 * - Applies option updates reactively via setOption.
 * - Calls chart.resize() via ResizeObserver so flex/grid parents work.
 * - Registers onEvents with stable wrappers that always delegate to the
 *   latest handler ref, so event closures never go stale.
 */
export default function EChart({ option, style, className, onEvents, chartRef }: Props) {
  const divRef = useRef<HTMLDivElement>(null)
  const instanceRef = useRef<echarts.ECharts | null>(null)

  const optionRef = useRef(option)
  optionRef.current = option

  const onEventsRef = useRef(onEvents)
  onEventsRef.current = onEvents

  // Mount / unmount
  useEffect(() => {
    const el = divRef.current
    if (!el) return
    const chart = echarts.init(el, null, { renderer: 'canvas' })
    instanceRef.current = chart
    if (chartRef) chartRef.current = chart
    chart.setOption(optionRef.current)

    // Stable wrappers delegate to the always-current onEventsRef
    if (onEventsRef.current) {
      Object.keys(onEventsRef.current).forEach((evt) => {
        chart.on(evt, (p: unknown) => onEventsRef.current?.[evt]?.(p))
      })
    }

    const obs = new ResizeObserver(() => instanceRef.current?.resize())
    obs.observe(el)

    return () => {
      obs.disconnect()
      chart.dispose()
      instanceRef.current = null
      if (chartRef) chartRef.current = null
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // Reactive option updates
  useEffect(() => {
    instanceRef.current?.setOption(option, { replaceMerge: ['series'] })
  }, [option])

  return <div ref={divRef} style={style} className={className} />
}
