import { useMemo, useState } from 'react'
import { useRequestLog } from './log'
import { useAccounts, useHealth } from './queries'
import { AccountsPanel } from './components/AccountsPanel'
import { BreakIt } from './components/BreakIt'
import { InvariantsPanel } from './components/InvariantsPanel'
import { TransferForm } from './components/TransferForm'
import { LedgerPanel } from './components/LedgerPanel'
import { RequestLogPanel } from './components/RequestLogPanel'
import './break-it.css'

type Tab = 'break' | 'accounts' | 'transfer' | 'ledger' | 'log'

const isLocal = ['localhost', '127.0.0.1'].includes(location.hostname)

export default function App() {
  const health = useHealth()
  const accountsQ = useAccounts()
  const accounts = useMemo(() => accountsQ.data ?? [], [accountsQ.data])
  const log = useRequestLog()

  const [tab, setTab] = useState<Tab>('break')
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

  const online = health.isSuccess

  const tabs: { id: Tab; label: string; count?: number }[] = [
    { id: 'break', label: 'BREAK IT' },
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
      <h1 className="headline">Try to break it. Watch it hold.</h1>
      <p className="sub">
        A ledger that moves money exactly once, even when requests are retried, race each other, or the cache is wiped.
        Run the attacks below against the live API. The invariants stay on screen so you can see them hold.
      </p>

      {!online && !health.isPending && (
        <div className="callout" role="alert">
          {isLocal ? (
            <>
              <strong>API not reachable.</strong> Start it with <code>make up</code> then <code>make run</code> (it listens
              on <code>:8088</code>). This page retries every 5 seconds.
            </>
          ) : (
            <>
              <strong>The demo server is waking up.</strong> It runs on a free tier that sleeps when idle, so the first
              request can take up to a minute. This page retries every 5 seconds.
            </>
          )}
        </div>
      )}

      <InvariantsPanel />

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
        {tab === 'break' && <BreakIt />}
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
        <span>API → {location.host}/api</span>
      </footer>
    </div>
  )
}
