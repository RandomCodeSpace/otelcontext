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
 * NOTE ON TELEMETRY FORWARDING: `useWebSocket` in this codebase is currently
 * receive-only (server pushes log batches to client over `/ws`). There is no
 * client->server send API exposed from the hook, so we do NOT forward errors
 * over WebSocket here to avoid adding coupling. When a bidirectional telemetry
 * channel is introduced, wire the `componentDidCatch` branch below.
 */
export class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null, info: null }

  static getDerivedStateFromError(error: Error): Partial<State> {
    return { error }
  }

  componentDidCatch(error: Error, info: ErrorInfo): void {
    this.setState({ info })
    // eslint-disable-next-line no-console
    console.error('[ErrorBoundary] Uncaught error in React tree', {
      message: error.message,
      name: error.name,
      stack: error.stack,
      componentStack: info.componentStack,
      url: typeof window !== 'undefined' ? window.location.href : undefined,
      userAgent: typeof navigator !== 'undefined' ? navigator.userAgent : undefined,
      timestamp: new Date().toISOString(),
    })
    // TODO(telemetry): forward to server when useWebSocket exposes a send()
    // API, or via a dedicated POST /api/client-errors endpoint.
  }

  private reset = (): void => {
    this.setState({ error: null, info: null })
  }

  private reload = (): void => {
    if (typeof window !== 'undefined') {
      window.location.reload()
    }
  }

  render(): ReactNode {
    const { error, info } = this.state
    if (!error) return this.props.children

    // Inline styles ONLY — if the design system stylesheet failed to load and is
    // the root cause, the fallback must still render correctly. DS CSS vars are
    // used as primary with hex fallbacks.
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
          background: 'var(--bg-0, #0a0a0a)',
          color: 'var(--fg-1, #fff)',
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
            background: 'var(--bg-1, #111)',
            border: '1px solid var(--border-1, #27272a)',
            borderRadius: 'var(--radius-lg, 12px)',
            padding: '32px',
            boxShadow: 'var(--shadow-lg, 0 10px 40px rgba(0, 0, 0, 0.5))',
          }}
        >
          <div
            style={{
              fontSize: '12px',
              fontWeight: 700,
              letterSpacing: '0.14em',
              textTransform: 'uppercase',
              color: 'var(--brand-red-500, #ef4444)',
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
              color: 'var(--fg-1, #fff)',
            }}
          >
            Something went wrong
          </h1>
          <p
            style={{
              fontSize: '14px',
              lineHeight: 1.6,
              color: 'var(--fg-2, #d4d4d8)',
              margin: '0 0 20px 0',
            }}
          >
            The UI encountered an unexpected error and could not continue
            rendering. You can try recovering without a full reload, or refresh
            the page if that fails.
          </p>

          <div
            style={{
              background: 'var(--bg-3, #050505)',
              border: '1px solid var(--border-1, #27272a)',
              borderRadius: 'var(--radius-md, 8px)',
              padding: '12px 14px',
              marginBottom: '20px',
              fontFamily: monoStack,
              fontSize: '13px',
              color: 'var(--fg-2, #d4d4d8)',
              wordBreak: 'break-word',
            }}
          >
            <span style={{ color: 'var(--brand-red-500, #ef4444)' }}>
              {error.name || 'Error'}
            </span>
            : {error.message || '(no message)'}
          </div>

          {info?.componentStack && (
            <details
              style={{
                marginBottom: '24px',
                fontSize: '12px',
                color: 'var(--fg-3, #71717a)',
              }}
            >
              <summary
                style={{
                  cursor: 'pointer',
                  userSelect: 'none',
                  padding: '4px 0',
                  color: 'var(--fg-2, #d4d4d8)',
                }}
              >
                Component stack
              </summary>
              <pre
                style={{
                  marginTop: '8px',
                  padding: '12px',
                  background: 'var(--bg-3, #050505)',
                  border: '1px solid var(--border-1, #27272a)',
                  borderRadius: 'var(--radius-md, 8px)',
                  fontFamily: monoStack,
                  fontSize: '11px',
                  lineHeight: 1.5,
                  color: 'var(--fg-2, #d4d4d8)',
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
                border: '1px solid var(--accent-fg, #ef4444)',
                background: 'var(--accent-fg, #ef4444)',
                color: 'var(--accent-on, #fff)',
                padding: '10px 18px',
                borderRadius: 'var(--radius-md, 8px)',
                fontSize: '14px',
                fontWeight: 600,
                cursor: 'pointer',
                transition: 'background 120ms ease',
              }}
            >
              Try again
            </button>
            <button
              type="button"
              onClick={this.reload}
              style={{
                appearance: 'none',
                border: '1px solid var(--border-2, #3f3f46)',
                background: 'transparent',
                color: 'var(--fg-1, #fff)',
                padding: '10px 18px',
                borderRadius: 'var(--radius-md, 8px)',
                fontSize: '14px',
                fontWeight: 500,
                cursor: 'pointer',
                transition: 'border-color 120ms ease',
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
