import { useEffect, useRef, useState } from 'react'
import styles from './UptimeOdometer.module.css'

interface UptimeOdometerProps {
  /**
   * Authoritative PROCESS uptime in seconds, refreshed each /api/system/graph
   * poll. The component ticks 1Hz between polls and re-syncs whenever this
   * value changes — never an SLA, just how long the process has been up.
   */
  uptimeSeconds: number
}

function format(totalSeconds: number): string {
  const t = Math.max(0, Math.floor(totalSeconds))
  const days = Math.floor(t / 86400)
  const hours = Math.floor((t % 86400) / 3600)
  const minutes = Math.floor((t % 3600) / 60)
  const seconds = t % 60
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${days}d ${pad(hours)}:${pad(minutes)}:${pad(seconds)}`
}

/**
 * Uptime odometer — legend "UP" + mono "<d>d HH:MM:SS", ticking 1Hz off a
 * local clock anchored to the last server-provided base. A backward jump in
 * the prop (process restart) re-bases and briefly flashes via a --dur-*
 * transition. The displayed numeral is the source of truth.
 */
export function UptimeOdometer({ uptimeSeconds }: Readonly<UptimeOdometerProps>) {
  // Base seconds + the wall-clock instant it was captured. Ticks derive from
  // (now - anchor) so a tab that was backgrounded snaps forward on return.
  const baseRef = useRef(uptimeSeconds)
  const anchorRef = useRef(Date.now())
  const [display, setDisplay] = useState(uptimeSeconds)
  const [restarted, setRestarted] = useState(false)

  // Re-sync to the prop whenever the server hands us a fresh reading.
  useEffect(() => {
    const backwards = uptimeSeconds < baseRef.current
    baseRef.current = uptimeSeconds
    anchorRef.current = Date.now()
    setDisplay(uptimeSeconds)
    if (backwards) {
      setRestarted(true)
      const id = setTimeout(() => setRestarted(false), 600)
      return () => clearTimeout(id)
    }
    return undefined
  }, [uptimeSeconds])

  useEffect(() => {
    const tick = () => {
      const elapsed = (Date.now() - anchorRef.current) / 1000
      setDisplay(baseRef.current + elapsed)
    }
    const id = setInterval(tick, 1000)
    // Snap immediately when the tab regains focus rather than waiting up to 1s.
    const onVisible = () => {
      if (document.visibilityState === 'visible') tick()
    }
    document.addEventListener('visibilitychange', onVisible)
    return () => {
      clearInterval(id)
      document.removeEventListener('visibilitychange', onVisible)
    }
  }, [])

  const text = format(display)

  return (
    <span
      className={styles.odometer}
      role="group"
      aria-label={`Process uptime ${text}`}
    >
      <span className="legend" aria-hidden="true">
        UP
      </span>
      <span
        className={`num ${styles.value} ${restarted ? styles.flash : ''}`}
        data-restarted={restarted || undefined}
        aria-hidden="true"
      >
        {text}
      </span>
    </span>
  )
}
