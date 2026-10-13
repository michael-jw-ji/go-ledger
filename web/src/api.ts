import { addLog } from './log'

export type Side = 'debit' | 'credit'

export interface Account {
  id: number
  name: string
  currency: string
  normal_balance: Side
  allow_negative: boolean
  /** Credits skip the row lock; the rollup worker applies them. */
  hot: boolean
  /** Effective balance: includes hot entries not rolled up yet. */
  balance: number
  lock_version: number
  /** Hot entries committed but not yet in the cached balance. */
  pending_entries: number
  created_at: string
}

export interface NewAccount {
  name: string
  currency: string
  normal_balance: Side
  allow_negative: boolean
  hot?: boolean
}

export interface Leg {
  account_id: number
  direction: Side
  amount: number
}

export interface Entry extends Leg {
  id: number
  transfer_id: string
  created_at: string
}

export interface Transfer {
  id: string
  status: 'posted' | 'rejected'
  error_code?: string
  reverses_transfer_id?: string
  entries: Entry[]
  created_at: string
}

export type TransferBody =
  | { entries: Leg[] }
  | { debit_account_id: number; credit_account_id: number; amount: number }

export interface ReconMismatch {
  kind: 'account' | 'transfer' | 'trial_balance'
  account_id?: number
  transfer_id?: string
  currency?: string
  cached: number
  computed: number
}

export interface Reconciliation {
  run_id: number
  started_at: string
  finished_at: string
  accounts_checked: number
  mismatches: ReconMismatch[]
}

export interface ApiResult<T> {
  data: T
  status: number
  replayed: boolean
}

export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly body: unknown
  readonly replayed: boolean

  constructor(status: number, code: string, message: string, body: unknown = null, replayed = false) {
    super(message)
    this.status = status
    this.code = code
    this.body = body
    this.replayed = replayed
  }

  /** The transfer recorded for a business-rule rejection, if any. */
  get transfer(): Transfer | undefined {
    return (this.body as { transfer?: Transfer } | null)?.transfer
  }
}

const BASE = '/api'

async function request<T>(
  method: string,
  path: string,
  opts: { body?: unknown; key?: string; silent?: boolean } = {},
): Promise<ApiResult<T>> {
  const headers: Record<string, string> = {}
  if (opts.body !== undefined) headers['Content-Type'] = 'application/json'
  if (opts.key) headers['Idempotency-Key'] = opts.key

  const start = performance.now()
  let res: Response
  try {
    res = await fetch(BASE + path, {
      method,
      headers,
      body: opts.body === undefined ? undefined : JSON.stringify(opts.body),
    })
  } catch {
    if (!opts.silent) {
      addLog({ at: new Date(), method, path, idempotencyKey: opts.key, status: 0, replayed: false,
        ms: Math.round(performance.now() - start), requestBody: opts.body })
    }
    throw new ApiError(0, 'network_error', 'Could not reach the API. Is `make run` running?')
  }

  const text = await res.text()
  let json: unknown = null
  try {
    json = text ? JSON.parse(text) : null
  } catch {
    json = text
  }
  const replayed = res.headers.get('Idempotent-Replayed') === 'true'
  if (!opts.silent) {
    addLog({ at: new Date(), method, path, idempotencyKey: opts.key, status: res.status, replayed,
      ms: Math.round(performance.now() - start), requestBody: opts.body, responseBody: json })
  }

  if (!res.ok) {
    const err = (json as { error?: { code?: string; message?: string } } | null)?.error
    // Vite's proxy answers 5xx with no JSON when the API is down.
    const code = err?.code ?? (res.status >= 500 ? 'api_unreachable' : `http_${res.status}`)
    const message = err?.message ?? (res.status >= 500 ? 'The API returned an error or is not running.' : res.statusText)
    throw new ApiError(res.status, code, message, json, replayed)
  }
  return { data: json as T, status: res.status, replayed }
}

export const api = {
  health: () => request<null>('GET', '/healthz', { silent: true }),
  listAccounts: () => request<{ accounts: Account[] }>('GET', '/accounts', { silent: true }),
  createAccount: (a: NewAccount) => request<Account>('POST', '/accounts', { body: a }),
  listEntries: (accountId: number, cursor?: string, limit = 20) => {
    const q = new URLSearchParams({ limit: String(limit) })
    if (cursor) q.set('cursor', cursor)
    return request<{ entries: Entry[]; next_cursor: string | null }>(
      'GET', `/accounts/${accountId}/entries?${q}`, { silent: true })
  },
  reconciliation: () => request<Reconciliation>('GET', '/reconciliation', { silent: true }),
  getTransfer: (id: string) => request<Transfer>('GET', `/transfers/${id}`, { silent: true }),
  createTransfer: (body: TransferBody, key: string) =>
    request<Transfer>('POST', '/transfers', { body, key }),
  reverseTransfer: (id: string, key: string) =>
    request<Transfer>('POST', `/transfers/${id}/reverse`, { key }),
}

export function newKey(): string {
  return crypto.randomUUID()
}
