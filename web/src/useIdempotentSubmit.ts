import { useCallback, useRef, useState } from 'react'
import { ApiError, newKey, type ApiResult } from './api'

// One idempotency key per submission intent:
//  - the key is reused for every retry of the same intent (network errors,
//    5xx, or 409 "still in progress"), so a retry can never post twice;
//  - it is replaced once the server gives a final answer (2xx, or a 4xx
//    other than 409), or when the caller says the form changed.
// A second submit while one is pending is ignored, which is what makes a
// double-click post once.

export type Outcome<T> =
  | { kind: 'ok'; result: ApiResult<T>; key: string }
  | { kind: 'error'; error: ApiError; key: string; final: boolean }

interface Options {
  /** Delay between automatic retries of a 409 in-progress answer. */
  retryDelayMs?: number
  maxConflictRetries?: number
}

export function isFinal(err: ApiError): boolean {
  return err.status >= 400 && err.status < 500 && err.status !== 409
}

export function useIdempotentSubmit<T>(send: (key: string) => Promise<ApiResult<T>>, opts: Options = {}) {
  const { retryDelayMs = 400, maxConflictRetries = 10 } = opts
  const [key, setKey] = useState(newKey)
  const keyRef = useRef(key)
  const pendingRef = useRef(false)
  const [pending, setPending] = useState(false)
  const [outcome, setOutcome] = useState<Outcome<T> | null>(null)

  const rotate = useCallback(() => {
    keyRef.current = newKey()
    setKey(keyRef.current)
  }, [])

  /** Call when the form's values change: it's a new intent. */
  const formChanged = useCallback(() => {
    if (!pendingRef.current) rotate()
  }, [rotate])

  const submit = useCallback(async (): Promise<Outcome<T> | null> => {
    if (pendingRef.current) return null // double-click guard
    pendingRef.current = true
    setPending(true)
    const k = keyRef.current
    let out: Outcome<T>
    try {
      for (let attempt = 0; ; attempt++) {
        try {
          out = { kind: 'ok', result: await send(k), key: k }
          break
        } catch (e) {
          const err = e instanceof ApiError ? e : new ApiError(0, 'unknown', String(e))
          if (err.status === 409 && err.code === 'request_in_progress' && attempt < maxConflictRetries) {
            await new Promise((r) => setTimeout(r, retryDelayMs * Math.min(2 ** attempt, 8)))
            continue
          }
          out = { kind: 'error', error: err, key: k, final: isFinal(err) }
          break
        }
      }
      if (out.kind === 'ok' || out.final) rotate()
      setOutcome(out)
      return out
    } finally {
      pendingRef.current = false
      setPending(false)
    }
  }, [send, rotate, retryDelayMs, maxConflictRetries])

  return { key, pending, outcome, submit, formChanged, setOutcome }
}
