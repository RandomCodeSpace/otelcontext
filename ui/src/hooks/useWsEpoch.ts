import { useSyncExternalStore } from 'react'
import { getWsManager, type WsManager } from '@/lib/wsManager'

interface EpochSnapshot {
  epoch: number
  revision: number
}

/**
 * Hook to monitor WebSocket epoch/revision changes.
 * Components use this to know when to discard accumulated state on epoch change.
 * Epoch changes indicate a new process generation; revision tracks coalesced updates.
 */
export function useWsEpoch(manager?: WsManager): EpochSnapshot {
  const ws = manager ?? getWsManager()

  const snapshot = useSyncExternalStore(
    (cb) => {
      // Subscribe to epoch changes via the status subscription
      // (epoch changes trigger log buffer reset and version bump)
      return ws.subscribeLogs(cb)
    },
    () => {
      // Return current epoch/revision from the manager
      // This requires exposing getEpochSnapshot() on WsManager
      return (ws.getEpochSnapshot as () => EpochSnapshot)() ?? { epoch: 0, revision: 0 }
    },
  )

  return snapshot
}
