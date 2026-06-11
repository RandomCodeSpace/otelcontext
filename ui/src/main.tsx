import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { QueryClientProvider } from '@tanstack/react-query'
import { ThemeProvider, ToastRegion } from '@ossrandom/design-system'
import '@ossrandom/design-system/styles.css'
import './styles/global.css'
import './styles/tokens.css'
import App from './App'
import { ErrorBoundary } from './components/ErrorBoundary'
import { useTheme } from './hooks/useTheme'
import { queryClient } from './lib/queryClient'

function Root() {
  // Theme is owned here (single source) and fed to ThemeProvider via `mode`,
  // so the provider no longer overwrites a persisted preference on mount.
  // (The design-system provider remains only while legacy views still use
  // DS components — it goes away with the C7 design-system removal.)
  const { theme, toggle } = useTheme()
  return (
    <QueryClientProvider client={queryClient}>
      <ThemeProvider mode={theme}>
        <ErrorBoundary>
          <App theme={theme} onToggleTheme={toggle} />
        </ErrorBoundary>
        <ToastRegion />
      </ThemeProvider>
    </QueryClientProvider>
  )
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <Root />
  </StrictMode>,
)
