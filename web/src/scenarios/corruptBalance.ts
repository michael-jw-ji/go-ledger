import { balanceOf, createAccount, dollars, failure, findings, newKey, type Scenario } from './common'

const AMOUNT = 1000
const DRIFT = 700

export const corruptBalance: Scenario = {
  id: 'corrupt-balance',
  title: 'Corrupt a balance',
  chaos: true,
  summary: 'Edits one cached balance with raw SQL, behind the ledger’s back. Reconciliation has to catch it, and a repair has to fix it.',
  run: async (io) => {
    const funding = await createAccount(io, 'Funding', true)
    const payee = await createAccount(io, 'Payee', false)
    const posted = await io.call('POST', '/transfers', {
      key: newKey(),
      body: { debit_account_id: funding, credit_account_id: payee, amount: AMOUNT },
    })
    if (posted.status !== 201) throw failure('setup transfer failed', posted)

    const corrupt = await io.call('POST', '/chaos/corrupt-balance', { body: { account_id: payee, delta: DRIFT } })
    if (corrupt.status !== 200) throw failure('corrupting the balance failed', corrupt)
    const seen = await findings(io)
    // Long enough for the invariants panel, which polls every second, to
    // show the mismatch before the repair clears it.
    await io.sleep(2500)

    const repair = await io.call('POST', '/chaos/repair')
    const after = await findings(io)
    const balance = await balanceOf(io, payee)

    const one = seen.length === 1 ? seen[0] : undefined
    const caught = one?.kind === 'account' && one.account_id === payee && one.cached - one.computed === DRIFT
    const pass = caught && repair.status === 200 && after.length === 0 && balance === AMOUNT
    return {
      steps: [
        `Posted ${dollars(AMOUNT)} to Payee.`,
        `Added ${dollars(DRIFT)} to Payee's cached balance directly in the database. No entry backs it.`,
        `Ran the sandbox's reconciliation.`,
        `Repaired: rebuilt the cached balance from the entries.`,
      ],
      expected: `exactly 1 finding: Payee off by ${dollars(DRIFT)}; clean after repair, Payee back at ${dollars(AMOUNT)}`,
      actual: `${seen.length} finding(s)${one ? `: account #${one.account_id} off by ${dollars(one.cached - one.computed)}` : ''}; ${after.length === 0 ? 'clean' : `${after.length} left`} after repair, Payee at ${dollars(balance)}`,
      pass,
    }
  },
}
