import { useState, type FormEvent } from 'react'
import { api, ApiError, newKey, type Account, type Side } from '../api'
import { formatMoney } from '../money'
import { useInvalidateLedger } from '../queries'
import { ErrorMessage } from './ErrorMessage'

const DEMO: { name: string; allow_negative: boolean; hot?: boolean; fund?: number }[] = [
  { name: 'Funding', allow_negative: true },
  { name: 'Alice', allow_negative: false, fund: 100_000 },
  { name: 'Bob', allow_negative: false, fund: 50_000 },
  { name: 'Merchant', allow_negative: false, hot: true },
]

export function AccountsPanel({
  accounts,
  selectedId,
  onSelect,
}: {
  accounts: Account[]
  selectedId: number | null
  onSelect: (id: number) => void
}) {
  const invalidate = useInvalidateLedger()
  const [name, setName] = useState('')
  const [currency, setCurrency] = useState('USD')
  const [normal, setNormal] = useState<Side>('credit')
  const [allowNegative, setAllowNegative] = useState(false)
  const [hot, setHot] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<ApiError | null>(null)

  const create = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      await api.createAccount({ name: name.trim(), currency: currency.trim().toUpperCase(), normal_balance: normal, allow_negative: allowNegative, hot })
      setName('')
      setAllowNegative(false)
      setHot(false)
    } catch (err) {
      setError(err as ApiError)
    } finally {
      invalidate()
      setBusy(false)
    }
  }

  const seed = async () => {
    setBusy(true)
    setError(null)
    try {
      const created: Account[] = []
      for (const d of DEMO) {
        created.push((await api.createAccount({ name: d.name, currency: 'USD', normal_balance: 'credit', allow_negative: d.allow_negative, hot: d.hot })).data)
      }
      const funding = created[0]
      for (const [i, d] of DEMO.entries()) {
        if (d.fund) {
          await api.createTransfer({ debit_account_id: funding.id, credit_account_id: created[i].id, amount: d.fund }, newKey())
        }
      }
    } catch (err) {
      setError(err as ApiError)
    } finally {
      invalidate()
      setBusy(false)
    }
  }

  return (
    <div className="grid-2">
      <div>
        <div className="panel-header" style={{ marginTop: 0 }}>
          <span>
            <span className="orange">■</span> <span className="title">ACCOUNTS</span> · CLICK A ROW TO OPEN ITS LEDGER
          </span>
          {accounts.length > 0 && (
            <button type="button" className="btn ghost sm" onClick={seed} disabled={busy}>
              Seed demo set
            </button>
          )}
        </div>
        {accounts.length === 0 ? (
          <div className="empty">
            <p>No accounts yet. Seed a demo set: a funding account that may go negative, Alice ($1,000), Bob ($500) and a hot merchant account.</p>
            <button type="button" className="btn" onClick={seed} disabled={busy}>
              {busy ? 'Seeding…' : 'Seed demo accounts'}
            </button>
          </div>
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>ID</th>
                  <th>Name</th>
                  <th>Normal</th>
                  <th>Rules</th>
                  <th className="num">Balance</th>
                  <th className="num" title="Entries in the balance (lock_version); + entries waiting for the rollup">Ver</th>
                </tr>
              </thead>
              <tbody>
                {accounts.map((a) => (
                  <tr
                    key={a.id}
                    className={`clickable${a.id === selectedId ? ' selected' : ''}`}
                    onClick={() => onSelect(a.id)}
                    tabIndex={0}
                    onKeyDown={(e) => e.key === 'Enter' && onSelect(a.id)}
                  >
                    <td className="mono dim">#{a.id}</td>
                    <td>{a.name}</td>
                    <td className="mono">{a.normal_balance}</td>
                    <td>
                      <span className="badges">
                        {a.allow_negative ? <span className="badge muted">may go negative</span> : <span className="badge">no overdraft</span>}
                        {a.hot && (
                          <span className="badge orange" title="Credits skip the row lock and are rolled up every ~100ms">
                            hot
                          </span>
                        )}
                      </span>
                    </td>
                    <td className={`num${a.balance < 0 ? ' neg' : ''}`}>{formatMoney(a.balance, a.currency)}</td>
                    <td className="num dim">
                      {a.lock_version}
                      {a.pending_entries > 0 && (
                        <span className="orange" title={`${a.pending_entries} entries waiting for the rollup worker`}>
                          {' '}+{a.pending_entries}
                        </span>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      <aside>
        <form className="box" onSubmit={create} aria-label="New account">
          <div className="panel-header" style={{ marginTop: 0 }}>
            <span className="title">NEW ACCOUNT</span>
          </div>
          <div className="form-grid" style={{ gridTemplateColumns: '1fr 90px' }}>
            <label className="field">
              <span className="label">Name</span>
              <input className="input" value={name} onChange={(e) => setName(e.target.value)} placeholder="e.g. Carol" required />
            </label>
            <label className="field">
              <span className="label">Currency</span>
              <input className="input num" value={currency} maxLength={3} onChange={(e) => setCurrency(e.target.value)} />
            </label>
          </div>
          <div className="form-grid" style={{ marginTop: 12 }}>
            <label className="field">
              <span className="label">Normal balance</span>
              <select className="select" value={normal} onChange={(e) => setNormal(e.target.value as Side)}>
                <option value="credit">credit (wallet, liability)</option>
                <option value="debit">debit (asset, cash)</option>
              </select>
            </label>
          </div>
          <label className="check" style={{ marginTop: 8 }}>
            <input type="checkbox" checked={allowNegative} onChange={(e) => setAllowNegative(e.target.checked)} />
            Allow negative balance
          </label>
          <label className="check">
            <input type="checkbox" checked={hot} onChange={(e) => setHot(e.target.checked)} />
            Hot account (many concurrent credits)
          </label>
          <div className="btn-row" style={{ marginTop: 8 }}>
            <button className="btn" type="submit" disabled={busy || !name.trim()}>
              Create account
            </button>
          </div>
        </form>
        {error && <ErrorMessage error={error} />}
        <div className="callout neutral">
          <strong>Normal balance</strong> decides which direction makes the balance go up. A credit-normal wallet grows when
          it is credited. A debit-normal cash account grows when it is debited.
        </div>
        <div className="callout neutral">
          <strong>Hot accounts</strong> take credits without locking their row. Each entry commits with its transfer, and a
          background job folds them into the cached balance about every 100ms. Balances shown here already include them.
        </div>
      </aside>
    </div>
  )
}
