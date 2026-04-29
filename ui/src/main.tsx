import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { ThemeProvider, ToastRegion } from '@ossrandom/design-system'
import '@ossrandom/design-system/styles.css'
import './styles/global.css'
import App from './App'

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <ThemeProvider mode="dark">
      <App />
      <ToastRegion />
    </ThemeProvider>
  </StrictMode>,
)
