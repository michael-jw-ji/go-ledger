import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { InvariantsPanel } from './InvariantsPanel'

const clean = {
  checked_at: '',
  accounts: 3,
  trial_balance: [{ currency: 'USD', debits: 1000, credits: 1000, difference: 0 }],
  held: [{ currency: 'USD', amount: 1000 }],
  recon: { accounts_checked: 3, mismatches: [], finished_at: new Date(0).toISOString() },
  keys_with_multiple_transfers: 0,
}

function renderWith(body: unknown) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } })),
  )
  render(
    <QueryClientProvider client={new QueryClient()}>
      <InvariantsPanel />
    </QueryClientProvider>,
  )
}

describe('InvariantsPanel', () => {
  beforeEach(() => vi.clearAllMocks())
  afterEach(() => vi.unstubAllGlobals())

  it('shows the guarantees green when they hold', async () => {
    renderWith(clean)
    expect(await screen.findByText('CLEAN')).toHaveClass('value', 'ok')
    expect(screen.getByText('Σ CREDIT − Σ DEBIT = 0')).toBeInTheDocument()
  })

  it('turns a reconciliation mismatch red', async () => {
    renderWith({
      ...clean,
      recon: {
        ...clean.recon,
        mismatches: [{ kind: 'account', account_id: 2, cached: 1007, computed: 1000 }],
      },
    })
    expect(await screen.findByText('1 OFF')).toHaveClass('value', 'bad')
  })

  it('turns the trial balance red when money is off by some amount', async () => {
    renderWith({
      ...clean,
      trial_balance: [{ currency: 'USD', debits: 1007, credits: 1000, difference: 7 }],
    })
    expect(await screen.findByText('$0.07 off')).toHaveClass('value', 'bad')
  })
})
