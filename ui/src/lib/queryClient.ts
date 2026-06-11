import { QueryClient } from '@tanstack/react-query'

// TanStack Query is the only server-state store in the UI. The defaults here
// are deliberate backend-pressure relief for the SQLite memory incident:
//   - staleTime 10s matches the server's /api/system/graph TTL cache, so a
//     remount inside that window is a cache read, not a SQLite query.
//   - refetchIntervalInBackground: false — hidden tabs stop polling entirely.
//   - bounded retries with jittered exponential backoff so N clients
//     recovering from a restart don't thundering-herd the server.

const RETRY_BASE_MS = 1000
const RETRY_MAX_MS = 30_000

/** Exponential backoff with up to +20% jitter, capped at 30s. */
export function retryDelayWithJitter(attemptIndex: number): number {
  const base = Math.min(RETRY_BASE_MS * 2 ** attemptIndex, RETRY_MAX_MS)
  return base + Math.random() * base * 0.2
}

/** Build the app QueryClient. Exported as a factory for test isolation. */
export function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        staleTime: 10_000,
        gcTime: 5 * 60_000,
        refetchIntervalInBackground: false,
        retry: 2,
        retryDelay: retryDelayWithJitter,
      },
    },
  })
}

/** The app-lifetime singleton used by main.tsx. */
export const queryClient = createQueryClient()
