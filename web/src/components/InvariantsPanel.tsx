import { formatMoney } from '../money'
import { useInvariants } from '../queries'

// The guarantees, on screen all the time. A break-it scenario should leave
// every tile green; if one turns red, the ledger has a real problem.
export function InvariantsPanel() {
  const q = useInvariants()
  const inv = q.data

  return (
    <section className="invariants" aria-label="Live invariants">
      <div className="panel-header" style={{ marginTop: 0 }}>
        <span className="title">LIVE INVARIANTS · YOUR SANDBOX</span>
        <span className={`live${q.isError ? ' off' : ''}`}>{q.isError ? 'NOT UPDATING' : 'REFRESHING EVERY SECOND'}</span>
      </div>

      {!inv ? (
        <div className="metrics">
          <Tile label="Trial balance" value="—" hint={q.isPending ? 'LOADING' : 'UNAVAILABLE'} />
        </div>
      ) : (
        <div className="metrics">
          <TrialTile inv={inv} />
          <Tile
            label="Held by customers"
            value={inv.held.length === 0 ? formatMoney(0) : inv.held.map((h) => formatMoney(h.amount, h.currency)).join(' · ')}
            hint={`${inv.accounts} ACCOUNTS · OVERDRAFT ACCOUNTS EXCLUDED`}
          />
          <ReconTile inv={inv} />
          <Tile
            label="Duplicate posts per key"
            value={String(inv.keys_with_multiple_transfers)}
            tone={inv.keys_with_multiple_transfers === 0 ? 'ok' : 'bad'}
            hint="ENFORCED BY A UNIQUE INDEX"
          />
        </div>
      )}
    </section>
  )
}

type Invariants = NonNullable<ReturnType<typeof useInvariants>['data']>

function TrialTile({ inv }: { inv: Invariants }) {
  const off = inv.trial_balance.filter((t) => t.difference !== 0)
  return (
    <Tile
      label="Trial balance"
      value={off.length === 0 ? formatMoney(0) : off.map((t) => `${formatMoney(t.difference, t.currency)} off`).join(' · ')}
      tone={off.length === 0 ? 'ok' : 'bad'}
      hint={off.length === 0 ? 'Σ CREDIT − Σ DEBIT = 0' : 'Σ CREDIT − Σ DEBIT ≠ 0'}
    />
  )
}

function ReconTile({ inv }: { inv: Invariants }) {
  const n = inv.recon.mismatches.length
  return (
    <Tile
      label="Reconciliation"
      value={n === 0 ? 'CLEAN' : `${n} OFF`}
      tone={n === 0 ? 'ok' : 'bad'}
      hint={`${inv.recon.accounts_checked} ACCOUNTS · ${new Date(inv.recon.finished_at).toLocaleTimeString()}`}
    />
  )
}

function Tile({ label, value, hint, tone }: { label: string; value: string; hint: string; tone?: 'ok' | 'bad' }) {
  const cls = tone ? `value ${tone}` : 'value'
  return (
    <div className="metric">
      <div className="label">{label}</div>
      <div className={cls}>{value}</div>
      <div className="hint">{hint}</div>
    </div>
  )
}
