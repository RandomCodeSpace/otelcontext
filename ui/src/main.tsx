import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { ThemeProvider, ToastRegion } from '@ossrandom/design-system'
import '@ossrandom/design-system/styles.css'
import './styles/global.css'
import App from './App'
import { ErrorBoundary } from './components/ErrorBoundary'
import { useTheme } from './hooks/useTheme'

function Root() {
  // Theme is owned here (single source) and fed to ThemeProvider via `mode`,
  // so the provider no longer overwrites a persisted preference on mount.
  const { theme, toggle } = useTheme()
  return (
    <ThemeProvider mode={theme}>
      <ErrorBoundary>
        <App theme={theme} onToggleTheme={toggle} />
      </ErrorBoundary>
      <ToastRegion />
    </ThemeProvider>
  )
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <Root />
  </StrictMode>,
)
