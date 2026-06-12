import { Component, type ErrorInfo, type ReactNode } from 'react'

type Props = { children: ReactNode }
type State = { error: Error | null; info: ErrorInfo | null }

/**
 * Production-grade top-level error boundary.
 *
 * Catches render/lifecycle errors anywhere in the React tree and renders a
 * friendly recovery UI instead of a blank page. Logs the error + component
 * stack to the console with a `[ErrorBoundary]` tag for triage.
 *
 * NOTE ON TELEMETRY FORWARDING: the `/ws` channel (lib/wsManager) is currently
 * receive-only (server pushes log batches to the client). There is no
 * client->server send API exposed, so we do NOT forward errors over WebSocket
 * here to avoid adding coupling. When a bidirectional telemetry channel is
 * introduced, wire the `componentDidCatch` branch below.
 */
export class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null, info: null }

  static getDerivedStateFromError(error: Error): Partial<State> {
    return { error }
  }

  componentDidCatch(error: Error, info: ErrorInfo): void {
    this.setState({ info })
    console.error('[ErrorBoundary] Uncaught error in React tree', {
      message: error.message,
      name: error.name,
      stack: error.stack,
      componentStack: info.componentStack,
      url: typeof window === 'undefined' ? undefined : window.location.href,
      userAgent: typeof navigator === 'undefined' ? undefined : navigator.userAgent,
      timestamp: new Date().toISOString(),
    })
    // TODO(telemetry): forward to server when lib/wsManager exposes a send()
    // API, or via a dedicated POST /api/client-errors endpoint.
  }

  private readonly reset = (): void => {
    this.setState({ error: null, info: null })
  }

  private readonly reload = (): void => {
    if (typeof window !== 'undefined') {
      window.location.reload()
    }
  }

  render(): ReactNode {
    const { error, info } = this.state
    if (!error) return this.props.children

    // Inline styles ONLY — if the app stylesheet failed to load and is the
    // root cause, the fallback must still render. Our tokens (styles/
    // tokens.css) are primary; the hex fallbacks are those tokens' dark
    // values, used only when the stylesheet itself is missing.
    const monoStack =
      'var(--font-mono, ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace)'
    return (
      <div
        role="alert"
        aria-live="assertive"
        style={{
          position: 'fixed',
          inset: 0,
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          padding: '24px',
          background: 'var(--bg-base, #0b0d10)',
          color: 'var(--text-1, #e8ecf2)',
          fontFamily:
            'var(--font-sans, -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif)',
          zIndex: 9999,
          overflow: 'auto',
        }}
      >
        <div
          style={{
            width: '100%',
            maxWidth: '640px',
            background: 'var(--bg-raised, #12151b)',
            border: '1px solid var(--stroke-1, #1e232d)',
            borderRadius: 'var(--radius-3, 10px)',
            padding: '32px',
            boxShadow: 'var(--shadow-sheet, 0 8px 32px rgb(0 0 0 / 0.45))',
          }}
        >
          <div
            style={{
              fontSize: '12px',
              fontWeight: 700,
              letterSpacing: '0.14em',
              textTransform: 'uppercase',
              color: 'var(--crit, #f87171)',
              marginBottom: '12px',
            }}
          >
            Application error
          </div>
          <h1
            style={{
              fontSize: '24px',
              fontWeight: 700,
              margin: '0 0 12px 0',
              color: 'var(--text-1, #e8ecf2)',
            }}
          >
            Something went wrong
          </h1>
          <p
            style={{
              fontSize: '14px',
              lineHeight: 1.6,
              color: 'var(--text-2, #9aa3b2)',
              margin: '0 0 20px 0',
            }}
          >
            The UI encountered an unexpected error and could not continue
            rendering. You can try recovering without a full reload, or refresh
            the page if that fails.
          </p>

          <div
            style={{
              background: 'var(--bg-inset, #07090c)',
              border: '1px solid var(--stroke-1, #1e232d)',
              borderRadius: 'var(--radius-2, 6px)',
              padding: '12px 14px',
              marginBottom: '20px',
              fontFamily: monoStack,
              fontSize: '13px',
              color: 'var(--text-2, #9aa3b2)',
              wordBreak: 'break-word',
            }}
          >
            <span style={{ color: 'var(--crit, #f87171)' }}>
              {error.name || 'Error'}
            </span>
            : {error.message || '(no message)'}
          </div>

          {info?.componentStack && (
            <details
              style={{
                marginBottom: '24px',
                fontSize: '12px',
                color: 'var(--text-3, #646d7c)',
              }}
            >
              <summary
                style={{
                  cursor: 'pointer',
                  userSelect: 'none',
                  padding: '4px 0',
                  color: 'var(--text-2, #9aa3b2)',
                }}
              >
                Component stack
              </summary>
              <pre
                style={{
                  marginTop: '8px',
                  padding: '12px',
                  background: 'var(--bg-inset, #07090c)',
                  border: '1px solid var(--stroke-1, #1e232d)',
                  borderRadius: 'var(--radius-2, 6px)',
                  fontFamily: monoStack,
                  fontSize: '11px',
                  lineHeight: 1.5,
                  color: 'var(--text-2, #9aa3b2)',
                  whiteSpace: 'pre-wrap',
                  wordBreak: 'break-word',
                  maxHeight: '240px',
                  overflow: 'auto',
                }}
              >
                {info.componentStack}
              </pre>
            </details>
          )}

          <div style={{ display: 'flex', gap: '12px', flexWrap: 'wrap' }}>
            <button
              type="button"
              onClick={this.reset}
              style={{
                appearance: 'none',
                border: '1px solid var(--crit, #f87171)',
                background: 'transparent',
                color: 'var(--crit, #f87171)',
                padding: '10px 18px',
                borderRadius: 'var(--radius-2, 6px)',
                fontSize: '14px',
                fontWeight: 600,
                cursor: 'pointer',
                transition: 'background var(--dur-2, 140ms) var(--ease, ease)',
              }}
            >
              Try again
            </button>
            <button
              type="button"
              onClick={this.reload}
              style={{
                appearance: 'none',
                border: '1px solid var(--stroke-2, #2a3140)',
                background: 'transparent',
                color: 'var(--text-1, #e8ecf2)',
                padding: '10px 18px',
                borderRadius: 'var(--radius-2, 6px)',
                fontSize: '14px',
                fontWeight: 500,
                cursor: 'pointer',
                transition: 'border-color var(--dur-2, 140ms) var(--ease, ease)',
              }}
            >
              Reload page
            </button>
          </div>
        </div>
      </div>
    )
  }
}

export default ErrorBoundary
