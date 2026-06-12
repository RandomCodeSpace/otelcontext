import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { QueryClientProvider } from '@tanstack/react-query'
import './styles/global.css'
import './styles/tokens.css'
import App from './App'
import { ErrorBoundary } from './components/ErrorBoundary'
import { useTheme } from './hooks/useTheme'
import { queryClient } from './lib/queryClient'

function Root() {
  // Theme is owned here (single source): useTheme sets data-theme on
  // <html>, which styles/tokens.css turns into the whole theme system.
  const { theme, toggle } = useTheme()
  return (
    <QueryClientProvider client={queryClient}>
      <ErrorBoundary>
        <App theme={theme} onToggleTheme={toggle} />
      </ErrorBoundary>
    </QueryClientProvider>
  )
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <Root />
  </StrictMode>,
)
