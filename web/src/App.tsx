import { useMemo, useState } from 'react'
import { formatMoney } from './money'
import { useRequestLog } from './log'
import { useAccounts, useHealth } from './queries'
import { AccountsPanel } from './components/AccountsPanel'
import { TransferForm } from './components/TransferForm'
import { LedgerPanel } from './components/LedgerPanel'
import { RequestLogPanel } from './components/RequestLogPanel'

type Tab = 'accounts' | 'transfer' | 'ledger' | 'log'

export default function App() {
  const health = useHealth()
  const accountsQ = useAccounts()
  const accounts = useMemo(() => accountsQ.data ?? [], [accountsQ.data])
  const log = useRequestLog()

  const [tab, setTab] = useState<Tab>('accounts')
  const [accountId, setAccountId] = useState<number | null>(null)
  const [transferId, setTransferId] = useState<string | null>(null)

  const openAccount = (id: number) => {
    setAccountId(id)
    setTab('ledger')
  }
  const openTransfer = (id: string, account?: number) => {
    setTransferId(id)
    if (account !== undefined) setAccountId(account)
    setTab('ledger')
  }

  // Σ credit-normal − Σ debit-normal, per currency. Double-entry keeps it at 0.
  const trial = useMemo(() => {
    const byCur = new Map<string, number>()
    for (const a of accounts) {
      byCur.set(a.currency, (byCur.get(a.currency) ?? 0) + (a.normal_balance === 'credit' ? a.balance : -a.balance))
    }
    const off = [...byCur].filter(([, v]) => v !== 0)
    return { ok: off.length === 0, off }
  }, [accounts])

  const replays = log.filter((e) => e.replayed).length
  const online = health.isSuccess

  const tabs: { id: Tab; label: string; count?: number }[] = [
    { id: 'accounts', label: 'ACCOUNTS', count: accounts.length },
    { id: 'transfer', label: 'TRANSFER' },
    { id: 'ledger', label: 'LEDGER' },
    { id: 'log', label: 'REQUEST LOG', count: log.length },
  ]

  return (
    <div className="page">
      <header className="nav">
        <div className="brand">
          LEDGER LAB<span className="sq">■</span>
        </div>
        <div className="nav-meta">
          <span>GO · POSTGRES · REDIS</span>
          <span className={`status${online ? '' : ' off'}`}>
            <span className="dot" />
            {health.isPending ? 'CONNECTING' : online ? 'API ONLINE' : 'API OFFLINE'}
          </span>
        </div>
      </header>

      <div className="tag">DOUBLE-ENTRY PAYMENTS LEDGER</div>
      <h1 className="headline">Move money. Prove it moved exactly once.</h1>
      <p className="sub">
        A console for testing the Go ledger. Every write sends an <code>Idempotency-Key</code>. The request log shows
        which requests posted, which were rejected, and which were replayed.
      </p>

      {!online && !health.isPending && (
        <div className="callout" role="alert">
          <strong>API not reachable.</strong> Start it with <code>make up</code> then <code>make run</code> (it listens on{' '}
          <code>:8088</code>). This page retries every 5 seconds.
        </div>
      )}

      <section className="metrics" aria-label="Summary">
        <div className="metric">
          <div className="label">Accounts</div>
          <div className="value">{accounts.length}</div>
        </div>
        <div className="metric">
          <div className="label">Trial balance</div>
          <div className={`value${trial.ok ? ' accent' : ''}`}>
            {trial.ok ? formatMoney(0) : trial.off.map(([c, v]) => formatMoney(v, c)).join(' · ')}
          </div>
          <div className="hint">Σ CREDIT − Σ DEBIT {trial.ok ? '· BALANCED' : '· DRIFT!'}</div>
        </div>
        <div className="metric">
          <div className="label">Requests</div>
          <div className="value">{log.length}</div>
          <div className="hint">THIS SESSION</div>
        </div>
        <div className="metric">
          <div className="label">Replays served</div>
          <div className="value">{replays}</div>
          <div className="hint">DUPLICATES NOT POSTED</div>
        </div>
      </section>

      <nav className="steps" role="tablist" aria-label="Sections">
        {tabs.map((t, i) => (
          <button
            key={t.id}
            type="button"
            role="tab"
            className="step"
            aria-selected={tab === t.id}
            onClick={() => setTab(t.id)}
          >
            <span className="num">{String(i + 1).padStart(2, '0')}</span>
            <span>
              {t.label}
              {t.count !== undefined && <span className="count"> · {t.count}</span>}
            </span>
          </button>
        ))}
      </nav>

      <main role="tabpanel">
        {tab === 'accounts' && <AccountsPanel accounts={accounts} selectedId={accountId} onSelect={openAccount} />}
        {tab === 'transfer' &&
          (accounts.length < 2 ? (
            <div className="empty">
              <p>You need at least two accounts to post a transfer.</p>
              <button type="button" className="btn" onClick={() => setTab('accounts')}>
                Go to accounts
              </button>
            </div>
          ) : (
            <TransferForm accounts={accounts} onOpenTransfer={openTransfer} />
          ))}
        {tab === 'ledger' && (
          <LedgerPanel
            accounts={accounts}
            accountId={accountId}
            onSelectAccount={setAccountId}
            transferId={transferId}
            onOpenTransfer={setTransferId}
          />
        )}
        {tab === 'log' && <RequestLogPanel />}
      </main>

      <footer className="footer">
        <span>AMOUNTS IN MINOR UNITS · ENTRIES APPEND-ONLY</span>
        <span>API → localhost:8088 via /api</span>
      </footer>
    </div>
  )
}
