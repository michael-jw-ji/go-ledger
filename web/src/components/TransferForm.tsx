import { useCallback, useMemo, useRef, useState, type FormEvent } from 'react'
import { api, ApiError, newKey, type Account, type Leg, type Side, type Transfer, type TransferBody } from '../api'
import { formatMoney, formatSigned, parseAmount } from '../money'
import { effect, useInvalidateLedger } from '../queries'
import { useIdempotentSubmit } from '../useIdempotentSubmit'
import { TransferResult } from './TransferResult'

interface LegDraft {
  accountId: string
  direction: Side
  amount: string
}

type Mode = 'simple' | 'multi'

interface Built {
  body?: TransferBody
  legs: Leg[]
  error?: string
}

function build(mode: Mode, debitId: string, creditId: string, amount: string, drafts: LegDraft[]): Built {
  if (mode === 'simple') {
    const cents = parseAmount(amount)
    if (!debitId || !creditId) return { legs: [], error: 'Pick both accounts.' }
    if (debitId === creditId) return { legs: [], error: 'Pick two different accounts.' }
    if (cents === null) return { legs: [], error: 'Enter an amount like 12.50.' }
    const d = Number(debitId)
    const c = Number(creditId)
    return {
      body: { debit_account_id: d, credit_account_id: c, amount: cents },
      legs: [
        { account_id: d, direction: 'debit', amount: cents },
        { account_id: c, direction: 'credit', amount: cents },
      ],
    }
  }
  const legs: Leg[] = []
  for (const [i, l] of drafts.entries()) {
    const cents = parseAmount(l.amount)
    if (!l.accountId) return { legs, error: `Entry ${i + 1}: pick an account.` }
    if (cents === null) return { legs, error: `Entry ${i + 1}: enter an amount.` }
    legs.push({ account_id: Number(l.accountId), direction: l.direction, amount: cents })
  }
  const debits = legs.filter((l) => l.direction === 'debit').reduce((s, l) => s + l.amount, 0)
  const credits = legs.filter((l) => l.direction === 'credit').reduce((s, l) => s + l.amount, 0)
  if (debits !== credits) return { legs, error: 'Debits must equal credits.' }
  return { body: { entries: legs }, legs }
}

interface LabRow {
  id: number
  label: string
  status: number
  code?: string
  replayed: boolean
  transferId?: string
}

export function TransferForm({
  accounts,
  onOpenTransfer,
  retryDelayMs,
}: {
  accounts: Account[]
  onOpenTransfer?: (id: string, accountId?: number) => void
  retryDelayMs?: number
}) {
  const invalidate = useInvalidateLedger()
  const [mode, setMode] = useState<Mode>('simple')
  const [debitId, setDebitId] = useState('')
  const [creditId, setCreditId] = useState('')
  const [amount, setAmount] = useState('')
  const [drafts, setDrafts] = useState<LegDraft[]>([
    { accountId: '', direction: 'debit', amount: '' },
    { accountId: '', direction: 'credit', amount: '' },
  ])

  const built = useMemo(() => build(mode, debitId, creditId, amount, drafts), [mode, debitId, creditId, amount, drafts])
  const bodyRef = useRef(built.body)
  bodyRef.current = built.body

  const lastSent = useRef<{ body: TransferBody; key: string } | null>(null)
  const send = useCallback(async (key: string) => {
    const body = bodyRef.current
    if (!body) throw new ApiError(0, 'validation_error', 'The form is incomplete.')
    lastSent.current = { body, key }
    try {
      return await api.createTransfer(body, key)
    } finally {
      invalidate()
    }
  }, [invalidate])

  const { key, pending, outcome, submit, formChanged } = useIdempotentSubmit<Transfer>(send, { retryDelayMs })

  const change = <T,>(set: (v: T) => void) => (v: T) => {
    set(v)
    formChanged()
  }

  const onSubmit = (e: FormEvent) => {
    e.preventDefault()
    void submit()
  }

  // ── Idempotency lab ──
  const [lab, setLab] = useState<LabRow[]>([])
  const [labBusy, setLabBusy] = useState(false)
  const labId = useRef(1)
  const pushLab = (rows: Omit<LabRow, 'id'>[]) =>
    setLab((prev) => [...rows.map((r) => ({ ...r, id: labId.current++ })), ...prev].slice(0, 12))

  const toRow = (label: string, r: PromiseSettledResult<Awaited<ReturnType<typeof api.createTransfer>>>): Omit<LabRow, 'id'> => {
    if (r.status === 'fulfilled') {
      return { label, status: r.value.status, replayed: r.value.replayed, transferId: r.value.data.id }
    }
    const e = r.reason as ApiError
    return { label, status: e.status, code: e.code, replayed: e.replayed, transferId: e.transfer?.id }
  }

  const resendSame = async () => {
    if (!lastSent.current) return
    setLabBusy(true)
    const { body, key: k } = lastSent.current
    const [r] = await Promise.allSettled([api.createTransfer(body, k)])
    pushLab([toRow('resend, same key', r)])
    invalidate()
    setLabBusy(false)
  }

  const fireTwice = async () => {
    const body = bodyRef.current
    if (!body) return
    setLabBusy(true)
    const k = newKey()
    lastSent.current = { body, key: k }
    const rs = await Promise.allSettled([api.createTransfer(body, k), api.createTransfer(body, k)])
    pushLab([toRow('concurrent #1', rs[0]), toRow('concurrent #2', rs[1])])
    invalidate()
    setLabBusy(false)
  }

  const byId = useMemo(() => new Map(accounts.map((a) => [a.id, a])), [accounts])
  const preview = useMemo(() => {
    const deltas = new Map<number, number>()
    for (const l of built.legs) {
      const a = byId.get(l.account_id)
      if (a) deltas.set(a.id, (deltas.get(a.id) ?? 0) + effect(a, l.direction, l.amount))
    }
    return [...deltas].map(([id, d]) => {
      const a = byId.get(id)!
      return { a, d, after: a.balance + d, overdraw: !a.allow_negative && a.balance + d < 0 }
    })
  }, [built.legs, byId])

  const debitTotal = built.legs.filter((l) => l.direction === 'debit').reduce((s, l) => s + l.amount, 0)
  const creditTotal = built.legs.filter((l) => l.direction === 'credit').reduce((s, l) => s + l.amount, 0)

  const accountOptions = accounts.map((a) => (
    <option key={a.id} value={a.id}>
      #{a.id} {a.name} · {formatMoney(a.balance, a.currency)}
    </option>
  ))

  return (
    <div className="grid-2">
      <div>
        <form className="box" onSubmit={onSubmit} aria-label="New transfer">
          <div className="panel-header" style={{ marginTop: 0 }}>
            <span className="title">NEW TRANSFER</span>
            <span className="seg" role="group" aria-label="Transfer type">
              <button type="button" aria-pressed={mode === 'simple'} onClick={() => change(setMode)('simple')}>
                TWO-LEG
              </button>
              <button type="button" aria-pressed={mode === 'multi'} onClick={() => change(setMode)('multi')}>
                MULTI-LEG
              </button>
            </span>
          </div>

          {mode === 'simple' ? (
            <div className="form-grid two-leg">
              <label className="field">
                <span className="label">Debit account</span>
                <select className="select" value={debitId} onChange={(e) => change(setDebitId)(e.target.value)}>
                  <option value="">Select…</option>
                  {accountOptions}
                </select>
              </label>
              <label className="field">
                <span className="label">Credit account</span>
                <select className="select" value={creditId} onChange={(e) => change(setCreditId)(e.target.value)}>
                  <option value="">Select…</option>
                  {accountOptions}
                </select>
              </label>
              <label className="field">
                <span className="label">Amount</span>
                <input
                  className="input num"
                  inputMode="decimal"
                  placeholder="0.00"
                  value={amount}
                  onChange={(e) => change(setAmount)(e.target.value)}
                />
              </label>
            </div>
          ) : (
            <div>
              {drafts.map((d, i) => (
                <div className="leg-row" key={i}>
                  <select
                    className="select"
                    aria-label={`Entry ${i + 1} account`}
                    value={d.accountId}
                    onChange={(e) => change(setDrafts)(drafts.map((x, j) => (j === i ? { ...x, accountId: e.target.value } : x)))}
                  >
                    <option value="">Account…</option>
                    {accountOptions}
                  </select>
                  <select
                    className="select"
                    aria-label={`Entry ${i + 1} direction`}
                    value={d.direction}
                    onChange={(e) => change(setDrafts)(drafts.map((x, j) => (j === i ? { ...x, direction: e.target.value as Side } : x)))}
                  >
                    <option value="debit">debit</option>
                    <option value="credit">credit</option>
                  </select>
                  <input
                    className="input num"
                    aria-label={`Entry ${i + 1} amount`}
                    inputMode="decimal"
                    placeholder="0.00"
                    value={d.amount}
                    onChange={(e) => change(setDrafts)(drafts.map((x, j) => (j === i ? { ...x, amount: e.target.value } : x)))}
                  />
                  <button
                    type="button"
                    className="icon-btn"
                    aria-label={`Remove entry ${i + 1}`}
                    disabled={drafts.length <= 2}
                    onClick={() => change(setDrafts)(drafts.filter((_, j) => j !== i))}
                  >
                    ×
                  </button>
                </div>
              ))}
              <div className="balance-check">
                <span>DEBITS <strong>{formatMoney(debitTotal)}</strong></span>
                <span>CREDITS <strong>{formatMoney(creditTotal)}</strong></span>
                <span className={debitTotal === creditTotal && debitTotal > 0 ? 'pos' : 'neg'}>
                  {debitTotal === creditTotal && debitTotal > 0 ? '■ BALANCED' : '■ UNBALANCED'}
                </span>
              </div>
              <button
                type="button"
                className="btn ghost sm"
                style={{ marginTop: 8 }}
                disabled={drafts.length >= 8}
                onClick={() => change(setDrafts)([...drafts, { accountId: '', direction: 'credit', amount: '' }])}
              >
                + Add entry
              </button>
            </div>
          )}

          {preview.length > 0 && (
            <div className="preview" aria-label="Balance preview">
              <div className="label" style={{ marginBottom: 6 }}>Balance after posting</div>
              {preview.map(({ a, d, after, overdraw }) => (
                <div className="preview-row" key={a.id}>
                  <span>
                    {a.name} <span className="mono">{a.normal_balance}-normal</span>
                  </span>
                  <span className="mono">
                    {formatMoney(a.balance, a.currency)} <span className={d < 0 ? 'neg' : 'pos'}>{formatSigned(d, a.currency)}</span> →{' '}
                    <strong className={overdraw ? 'neg' : ''}>{formatMoney(after, a.currency)}</strong>
                    {overdraw && <span className="badge orange" style={{ marginLeft: 8 }}>Would overdraw</span>}
                  </span>
                </div>
              ))}
            </div>
          )}

          <div className="btn-row">
            <button className="btn" type="submit" disabled={pending || !built.body}>
              {pending ? 'Posting…' : 'Post transfer'}
            </button>
            {built.error && <span className="dim mono" style={{ fontSize: '0.75rem' }}>{built.error}</span>}
          </div>
          <div className="keyline">
            IDEMPOTENCY-KEY <code data-testid="idem-key">{key}</code>
          </div>
        </form>

        {outcome && <TransferResult outcome={outcome} accounts={accounts} onOpenTransfer={onOpenTransfer} />}
      </div>

      <aside>
        <div className="box tint">
          <div className="label" style={{ color: 'var(--orange)', marginBottom: 8 }}>■ Idempotency lab</div>
          <p style={{ margin: '0 0 6px', fontSize: '0.9rem', lineHeight: 1.55 }}>
            Break it on purpose. Each button below goes around the form’s double-submit guard and hits the API directly.
          </p>
          <div className="btn-row" style={{ marginTop: 10 }}>
            <button type="button" className="btn ghost sm" disabled={labBusy || !lastSent.current} onClick={resendSame}>
              Resend last, same key
            </button>
            <button type="button" className="btn ghost sm" disabled={labBusy || !built.body} onClick={fireTwice}>
              Fire twice at once
            </button>
          </div>
          {lab.length > 0 ? (
            <ul className="lab-list" aria-label="Lab results">
              {lab.map((r) => (
                <li key={r.id}>
                  <span className={`badge ${r.status === 201 ? 'solid' : 'orange'}`}>{r.status || 'NET'}</span>
                  {r.replayed && <span className="badge">replayed</span>}
                  {r.code && <span className="badge orange">{r.code}</span>}
                  <span className="dim">{r.label}</span>
                  {r.transferId && onOpenTransfer && (
                    <button type="button" className="link" onClick={() => onOpenTransfer(r.transferId!)}>
                      {r.transferId.slice(0, 8)}…
                    </button>
                  )}
                </li>
              ))}
            </ul>
          ) : (
            <p className="dim" style={{ fontSize: '0.8rem', margin: '12px 0 0', lineHeight: 1.5 }}>
              Expect one <span className="mono">201</span> and one <span className="mono">409</span>, or one <span className="mono">201</span> plus
              a <span className="mono">replayed</span>. Never two postings.
            </p>
          )}
        </div>
        <div className="callout neutral">
          <strong>How keys work here.</strong> The form makes one key for each thing you submit. Retries after a network error, a 5xx or a{' '}
          <code>409</code> reuse that key. A new key is made after a final answer, or when you edit the form.
        </div>
      </aside>
    </div>
  )
}
