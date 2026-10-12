import { useSyncExternalStore } from 'react'

// A session-only log of every API call, so you can see status codes,
// idempotency keys and replays while testing.

export interface LogEntry {
  id: number
  at: Date
  method: string
  path: string
  idempotencyKey?: string
  status: number // 0 = network error
  replayed: boolean
  ms: number
  requestBody?: unknown
  responseBody?: unknown
}

let entries: LogEntry[] = []
let nextId = 1
const listeners = new Set<() => void>()

function emit() {
  for (const l of listeners) l()
}

export function addLog(e: Omit<LogEntry, 'id'>) {
  entries = [{ ...e, id: nextId++ }, ...entries].slice(0, 500)
  emit()
}

export function clearLog() {
  entries = []
  emit()
}

function subscribe(l: () => void) {
  listeners.add(l)
  return () => listeners.delete(l)
}

export function useRequestLog(): LogEntry[] {
  return useSyncExternalStore(subscribe, () => entries)
}
