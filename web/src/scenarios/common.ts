import { sandboxId } from '../sandbox'

// Break It scenarios are plain functions over an Io, so tests can run them
// against a fake ledger. The live Io talks to the API in the visitor's sandbox.
// Scenarios don't write to the request log: each one reports a summary.

export interface Call {
  status: number
  replayed: boolean
  /** The API's error code, if the response was an error. */
  code?: string
  body: unknown
}

export interface CallOptions {
  key?: string
  body?: unknown
  /** Sent as-is instead of JSON.stringify(body), to test formatting. */
  raw?: string
  headers?: Record<string, string>
}

export interface Io {
  call: (method: string, path: string, opts?: CallOptions) => Promise<Call>
  sleep: (ms: number) => Promise<void>
}

export interface ScenarioResult {
  steps: string[]
  expected: string
  actual: string
  pass: boolean
}

export interface Scenario {
  id: string
  title: string
  summary: string
  /** Needs the server's fault-injection endpoints (a `-tags chaos` build). */
  chaos?: boolean
  run: (io: Io) => Promise<ScenarioResult>
}

/** Whether the server can inject faults. A default build answers 404. */
export async function chaosAvailable(io: Io): Promise<boolean> {
  const r = await io.call('GET', '/chaos')
  return r.status === 200
}

export async function flushRedis(io: Io): Promise<number> {
  const r = await io.call('POST', '/chaos/flush-redis')
  const deleted = (r.body as { deleted?: number } | null)?.deleted
  if (r.status !== 200 || typeof deleted !== 'number') throw failure('flushing Redis failed', r)
  return deleted
}

/** Mismatches the sandbox's own reconciliation reports right now. */
export async function findings(io: Io): Promise<{ kind: string; account_id?: number; cached: number; computed: number }[]> {
  const r = await io.call('GET', '/invariants')
  if (r.status !== 200) throw failure('could not read invariants', r)
  return (r.body as { recon: { mismatches: { kind: string; account_id?: number; cached: number; computed: number }[] } }).recon.mismatches
}

export const liveIo: Io = {
  call: async (method, path, opts = {}) => {
    const headers: Record<string, string> = { ...opts.headers, 'X-Sandbox-Id': sandboxId() }
    if (opts.key) headers['Idempotency-Key'] = opts.key
    let body: string | undefined = opts.raw
    if (body === undefined && opts.body !== undefined) {
      headers['Content-Type'] = 'application/json'
      body = JSON.stringify(opts.body)
    }
    let res: Response
    try {
      res = await fetch('/api' + path, { method, headers, body })
    } catch {
      return { status: 0, replayed: false, code: 'network_error', body: null }
    }
    const text = await res.text()
    let parsed: unknown = null
    try {
      parsed = text ? JSON.parse(text) : null
    } catch {
      parsed = text
    }
    return {
      status: res.status,
      replayed: res.headers.get('Idempotent-Replayed') === 'true',
      code: errorCode(parsed),
      body: parsed,
    }
  },
  sleep: (ms) => new Promise((resolve) => setTimeout(resolve, ms)),
}

/** Why a setup step failed, in words a visitor can act on. */
export function failure(what: string, r: Call): Error {
  if (r.code === 'rate_limited') return new Error(`${what}: too many writes from your address. Wait a few seconds and run it again.`)
  if (r.code === 'sandbox_limit') return new Error(`${what}: your sandbox is full. Reset it and run again.`)
  return new Error(`${what}: ${r.code ?? `HTTP ${r.status}`}`)
}

export function errorCode(body: unknown): string | undefined {
  const code = (body as { error?: { code?: unknown } } | null)?.error?.code
  return typeof code === 'string' ? code : undefined
}

export function transferId(c: Call): string | undefined {
  return (c.body as { id?: string } | null)?.id
}

export async function createAccount(io: Io, name: string, allowNegative: boolean): Promise<number> {
  const r = await io.call('POST', '/accounts', {
    body: { name, currency: 'USD', normal_balance: 'credit', allow_negative: allowNegative },
  })
  const id = (r.body as { id?: number } | null)?.id
  if (r.status !== 201 || typeof id !== 'number') throw failure(`could not create ${name}`, r)
  return id
}

export async function balanceOf(io: Io, id: number): Promise<number> {
  const r = await io.call('GET', `/accounts/${id}`)
  const balance = (r.body as { balance?: number } | null)?.balance
  if (r.status !== 200 || typeof balance !== 'number') throw new Error(`could not read account ${id}`)
  return balance
}

/** The invariants that must hold after any scenario: money conserved, no
 *  key posted twice, and no reconciliation finding in the sandbox. */
export async function invariantsHold(io: Io): Promise<boolean> {
  const r = await io.call('GET', '/invariants')
  if (r.status !== 200) return false
  const inv = r.body as {
    trial_balance: { difference: number }[]
    keys_with_multiple_transfers: number
    recon: { mismatches: unknown[] }
  }
  return (
    inv.trial_balance.every((t) => t.difference === 0) &&
    inv.keys_with_multiple_transfers === 0 &&
    inv.recon.mismatches.length === 0
  )
}

export function dollars(cents: number): string {
  return `$${(cents / 100).toFixed(2)}`
}

export function newKey(): string {
  return crypto.randomUUID()
}
