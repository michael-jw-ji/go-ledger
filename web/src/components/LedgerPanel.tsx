import { useCallback } from 'react'
import { useInfiniteQuery, useQuery } from '@tanstack/react-query'
import { api, type Account, type Transfer } from '../api'
import { formatMoney, formatSigned } from '../money'
import { accountName, effect, useInvalidateLedger } from '../queries'
import { useIdempotentSubmit } from '../useIdempotentSubmit'
import { TransferResult } from './TransferResult'

export function LedgerPanel({
  accounts,
  accountId,
  onSelectAccount,
  transferId,
  onOpenTransfer,
}: {
  accounts: Account[]
  accountId: number | null
  onSelectAccount: (id: number) => void
  transferId: string | null
  onOpenTransfer: (id: string) => void
}) {
  const account = accounts.find((a) => a.id === accountId)

  return (
    <div className="grid-2">
      <div>
        <div className="panel-header" style={{ marginTop: 0 }}>
          <span>
            <span className="orange">■</span> <span className="title">ENTRIES</span> · APPEND-ONLY, NEWEST FIRST
          </span>
          <select
            className="select"
            style={{ width: 'auto', minHeight: 32, padding: '4px 8px', fontSize: '0.8rem' }}
            aria-label="Account"
            value={accountId ?? ''}
            onChange={(e) => onSelectAccount(Number(e.target.value))}
          >
            <option value="" disabled>
              Choose an account…
            </option>
            {accounts.map((a) => (
              <option key={a.id} value={a.id}>
                #{a.id} {a.name}
              </option>
            ))}
          </select>
        </div>
        {account ? (
          <Entries account={account} selectedTransfer={transferId} onOpenTransfer={onOpenTransfer} />
        ) : (
          <div className="empty">
            <p>Pick an account to see its entries.</p>
          </div>
        )}
      </div>
      <aside>
        {transferId ? (
          <TransferDetail key={transferId} id={transferId} accounts={accounts} onOpenTransfer={onOpenTransfer} />
        ) : (
          <div className="callout neutral">
            Click a transfer id to inspect it and reverse it. A reversal posts mirror-image entries and never edits the
            originals.
          </div>
        )}
      </aside>
    </div>
  )
}

function Entries({
  account,
  selectedTransfer,
  onOpenTransfer,
}: {
  account: Account
  selectedTransfer: string | null
  onOpenTransfer: (id: string) => void
}) {
  const q = useInfiniteQuery({
    queryKey: ['entries', account.id],
    queryFn: async ({ pageParam }) => (await api.listEntries(account.id, pageParam, 15)).data,
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => last.next_cursor ?? undefined,
  })
  const entries = q.data?.pages.flatMap((p) => p.entries) ?? []

  return (
    <>
      <div className="metrics" style={{ gridTemplateColumns: 'repeat(3, 1fr)', marginTop: 0 }}>
        <div className="metric">
          <div className="label">Balance</div>
          <div className={`value${account.balance < 0 ? ' accent' : ''}`}>{formatMoney(account.balance, account.currency)}</div>
        </div>
        <div className="metric">
          <div className="label">Normal side</div>
          <div className="value">{account.normal_balance.toUpperCase()}</div>
        </div>
        <div className="metric">
          <div className="label">Lock version</div>
          <div className="value">{account.lock_version}</div>
          <div className="hint">= ENTRIES APPLIED</div>
        </div>
      </div>
      {q.isPending ? (
        <p className="dim mono">Loading…</p>
      ) : entries.length === 0 ? (
        <div className="empty">
          <p>No entries yet. Post a transfer that touches this account.</p>
        </div>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Entry</th>
                <th>Time</th>
                <th>Transfer</th>
                <th>Side</th>
                <th className="num">Effect</th>
              </tr>
            </thead>
            <tbody>
              {entries.map((e) => {
                const fx = effect(account, e.direction, e.amount)
                return (
                  <tr key={e.id} className={e.transfer_id === selectedTransfer ? 'selected' : ''}>
                    <td className="mono dim">#{e.id}</td>
                    <td className="mono dim">{new Date(e.created_at).toLocaleTimeString()}</td>
                    <td>
                      <button type="button" className="link" onClick={() => onOpenTransfer(e.transfer_id)}>
                        {e.transfer_id.slice(0, 8)}…
                      </button>
                    </td>
                    <td className="mono">{e.direction}</td>
                    <td className={`num ${fx < 0 ? 'neg' : 'pos'}`}>{formatSigned(fx, account.currency)}</td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      )}
      {q.hasNextPage && (
        <div className="load-more">
          <button type="button" className="btn ghost sm" onClick={() => void q.fetchNextPage()} disabled={q.isFetchingNextPage}>
            {q.isFetchingNextPage ? 'Loading…' : 'Load more (cursor)'}
          </button>
        </div>
      )}
    </>
  )
}

function TransferDetail({
  id,
  accounts,
  onOpenTransfer,
}: {
  id: string
  accounts: Account[]
  onOpenTransfer: (id: string) => void
}) {
  const invalidate = useInvalidateLedger()
  const q = useQuery({ queryKey: ['transfer', id], queryFn: async () => (await api.getTransfer(id)).data })
  const send = useCallback(async (key: string) => {
    try {
      return await api.reverseTransfer(id, key)
    } finally {
      invalidate()
    }
  }, [id, invalidate])
  const reverse = useIdempotentSubmit<Transfer>(send)

  if (q.isPending) return <p className="dim mono">Loading…</p>
  if (q.isError) return <div className="callout">Transfer not found.</div>
  const t = q.data

  return (
    <div className="box">
      <div className="panel-header" style={{ marginTop: 0 }}>
        <span className="title">TRANSFER</span>
        <span className="badges">
          <span className={`badge ${t.status === 'posted' ? 'solid' : 'orange'}`}>{t.status}</span>
          {t.reverses_transfer_id && <span className="badge muted">reversal</span>}
        </span>
      </div>
      <dl className="kv" style={{ marginTop: 0 }}>
        <dt>ID</dt>
        <dd>{t.id}</dd>
        <dt>Created</dt>
        <dd>{new Date(t.created_at).toLocaleString()}</dd>
        {t.error_code && (
          <>
            <dt>Error</dt>
            <dd className="neg">{t.error_code}</dd>
          </>
        )}
        {t.reverses_transfer_id && (
          <>
            <dt>Reverses</dt>
            <dd>
              <button type="button" className="link" onClick={() => onOpenTransfer(t.reverses_transfer_id!)}>
                {t.reverses_transfer_id}
              </button>
            </dd>
          </>
        )}
      </dl>
      {t.entries.length > 0 ? (
        <ul className="legs">
          {t.entries.map((e) => (
            <li key={e.id}>
              <span>
                {accountName(accounts, e.account_id)} <span className="dim mono">{e.direction}</span>
              </span>
              <span className="mono">{formatMoney(e.amount)}</span>
            </li>
          ))}
        </ul>
      ) : (
        <p className="dim" style={{ fontSize: '0.85rem' }}>No entries: rejected transfers never touch balances.</p>
      )}
      {t.status === 'posted' && !t.reverses_transfer_id && (
        <div className="btn-row">
          <button type="button" className="btn" disabled={reverse.pending} onClick={() => void reverse.submit()}>
            {reverse.pending ? 'Reversing…' : 'Reverse transfer'}
          </button>
        </div>
      )}
      {reverse.outcome && <TransferResult outcome={reverse.outcome} accounts={accounts} onOpenTransfer={onOpenTransfer} />}
    </div>
  )
}
