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
  run: (io: Io) => Promise<ScenarioResult>
}

export const liveIo: Io = {
  call: async (method, path, opts = {}) => {
    const headers: Record<string, string> = { 'X-Sandbox-Id': sandboxId() }
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
  if (r.status !== 201 || typeof id !== 'number') throw new Error(`could not create ${name}: HTTP ${r.status}`)
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
