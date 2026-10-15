import { describe, expect, it } from 'vitest'
import type { Call, CallOptions, Io, Scenario } from './common'
import { retryStorm } from './retryStorm'
import { doubleSpend } from './doubleSpend'
import { keyReuse } from './keyReuse'

// A small in-memory ledger that answers the API routes the scenarios use.
// The flags break one guarantee each, so the fail paths can be exercised.
interface Flags {
  /** false: every POST posts, as if idempotency were gone. */
  dedupe?: boolean
  /** true: an insufficient balance is allowed to go negative. */
  overdraft?: boolean
  /** false: a reused key replays instead of answering 422. */
  keyCheck?: boolean
}

class FakeLedger {
  private accounts = new Map<number, { balance: number; allowNegative: boolean }>()
  private transfers = new Map<string, { hash: string; id: string; body: unknown }>()
  private nextAccount = 1
  private nextTransfer = 1
  private flags: Required<Flags>

  constructor(flags: Flags = {}) {
    this.flags = { dedupe: true, overdraft: false, keyCheck: true, ...flags }
  }

  readonly io: Io = {
    call: async (method, path, opts = {}) => this.handle(method, path, opts),
    sleep: async () => {},
  }

  private handle(method: string, path: string, opts: CallOptions): Call {
    if (method === 'POST' && path === '/accounts') {
      const b = opts.body as { allow_negative: boolean }
      const id = this.nextAccount++
      this.accounts.set(id, { balance: 0, allowNegative: b.allow_negative })
      return ok(201, { id })
    }
    if (method === 'GET' && path.startsWith('/accounts/')) {
      const acct = this.accounts.get(Number(path.split('/')[2]))
      return acct ? ok(200, { balance: acct.balance }) : ok(404, { error: { code: 'not_found' } })
    }
    if (method === 'POST' && path === '/transfers') return this.post(opts)
    if (method === 'GET' && path === '/invariants') {
      const total = [...this.accounts.values()].reduce((s, a) => s + a.balance, 0)
      return ok(200, {
        trial_balance: [{ currency: 'USD', difference: 0 }],
        keys_with_multiple_transfers: 0,
        recon: { mismatches: [], accounts_checked: this.accounts.size, finished_at: '' },
        held: [{ currency: 'USD', amount: total }],
        accounts: this.accounts.size,
      })
    }
    throw new Error(`unexpected ${method} ${path}`)
  }

  private post(opts: CallOptions): Call {
    const raw = opts.raw ?? JSON.stringify(opts.body)
    const hash = JSON.stringify(JSON.parse(raw))
    const key = opts.key ?? ''
    const seen = this.transfers.get(key)
    if (seen && this.flags.dedupe) {
      if (seen.hash !== hash && this.flags.keyCheck) {
        return ok(422, { error: { code: 'idempotency_key_reused' } })
      }
      return { ...ok(201, seen.body), replayed: true }
    }

    const b = JSON.parse(raw) as { debit_account_id: number; credit_account_id: number; amount: number }
    const debit = this.accounts.get(b.debit_account_id)!
    const credit = this.accounts.get(b.credit_account_id)!
    if (!debit.allowNegative && !this.flags.overdraft && debit.balance < b.amount) {
      return ok(422, { error: { code: 'insufficient_funds' } })
    }
    debit.balance -= b.amount
    credit.balance += b.amount
    const body = { id: `t-${this.nextTransfer++}`, status: 'posted' }
    this.transfers.set(key, { hash, id: body.id, body })
    return ok(201, body)
  }
}

function ok(status: number, body: unknown): Call {
  return { status, replayed: false, code: (body as { error?: { code?: string } }).error?.code, body }
}

async function run(s: Scenario, flags?: Flags) {
  return s.run(new FakeLedger(flags).io)
}

describe('Retry storm', () => {
  it('passes when one request posts and the rest replay', async () => {
    const r = await run(retryStorm)
    expect(r.pass).toBe(true)
    expect(r.actual).toContain('1 posted, 19 replayed')
  })

  it('fails when the server posts every retry', async () => {
    const r = await run(retryStorm, { dedupe: false })
    expect(r.pass).toBe(false)
    expect(r.actual).toContain('20 posted')
  })
})

describe('Double spend', () => {
  it('passes when one payment posts and the other is declined', async () => {
    const r = await run(doubleSpend)
    expect(r.pass).toBe(true)
    expect(r.actual).toContain('1 posted, 1 insufficient_funds')
  })

  it('fails when the wallet is allowed to go negative', async () => {
    const r = await run(doubleSpend, { overdraft: true })
    expect(r.pass).toBe(false)
    expect(r.actual).toContain('2 posted')
  })
})

describe('Key reuse', () => {
  it('passes with 422 for the reused key and a replay for the reformatted original', async () => {
    const r = await run(keyReuse)
    expect(r.pass).toBe(true)
    expect(r.actual).toContain('422 idempotency_key_reused')
    expect(r.actual).toContain('same transfer id')
  })

  it('fails when a reused key silently replays a different request', async () => {
    const r = await run(keyReuse, { keyCheck: false })
    expect(r.pass).toBe(false)
  })
})
