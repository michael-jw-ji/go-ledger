import type { Account, Transfer } from '../api'
import { formatSigned } from '../money'
import { accountName, effect } from '../queries'
import { describeError } from './ErrorMessage'
import type { Outcome } from '../useIdempotentSubmit'

export function TransferResult({
  outcome,
  accounts,
  onOpenTransfer,
}: {
  outcome: Outcome<Transfer>
  accounts: Account[] | undefined
  onOpenTransfer?: (id: string, accountId?: number) => void
}) {
  if (outcome.kind === 'error') {
    const { error } = outcome
    const rejected = error.transfer
    return (
      <div className="result rejected" role="alert">
        <div className="result-head">
          <span className="badges">
            <span className="badge orange solid">{rejected ? 'Rejected' : error.status || 'Network'}</span>
            <span className="badge orange">{error.code}</span>
            {error.replayed && <span className="badge">Replayed</span>}
          </span>
          {rejected && onOpenTransfer && (
            <button type="button" className="link" onClick={() => onOpenTransfer(rejected.id)}>
              {rejected.id.slice(0, 8)}…
            </button>
          )}
        </div>
        <p>{describeError(error)}</p>
        <dl className="kv">
          <dt>Key</dt>
          <dd>{outcome.key}</dd>
          <dt>Key next</dt>
          <dd>{outcome.final ? 'rotated (final answer)' : 'kept (safe to retry)'}</dd>
        </dl>
      </div>
    )
  }

  const t = outcome.result.data
  return (
    <div className="result" role="status">
      <div className="result-head">
        <span className="badges">
          <span className="badge solid">{t.status}</span>
          <span className="badge">{outcome.result.status}</span>
          {outcome.result.replayed && <span className="badge orange">Replayed · not posted again</span>}
          {t.reverses_transfer_id && <span className="badge muted">Reversal</span>}
        </span>
        {onOpenTransfer && (
          <button type="button" className="link" onClick={() => onOpenTransfer(t.id, t.entries[0]?.account_id)}>
            {t.id.slice(0, 8)}…
          </button>
        )}
      </div>
      <ul className="legs">
        {t.entries.map((e) => {
          const acct = accounts?.find((a) => a.id === e.account_id)
          const fx = acct ? effect(acct, e.direction, e.amount) : 0
          return (
            <li key={e.id}>
              <span>
                {accountName(accounts, e.account_id)} <span className="dim mono">{e.direction}</span>
              </span>
              <span className={`mono ${fx < 0 ? 'neg' : 'pos'}`}>{formatSigned(fx, acct?.currency)}</span>
            </li>
          )
        })}
      </ul>
      <dl className="kv">
        <dt>Key</dt>
        <dd>{outcome.key}</dd>
      </dl>
    </div>
  )
}
