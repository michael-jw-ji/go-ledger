import { balanceOf, createAccount, dollars, errorCode, invariantsHold, newKey, transferId, type Scenario } from './common'

const AMOUNT = 1000

export const keyReuse: Scenario = {
  id: 'key-reuse',
  title: 'Key reuse',
  summary: 'Reuses a key for a different amount, then resends the original with different formatting.',
  run: async (io) => {
    const funding = await createAccount(io, 'Funding', true)
    const payee = await createAccount(io, 'Payee', false)
    const key = newKey()
    const original = { debit_account_id: funding, credit_account_id: payee, amount: AMOUNT }

    const first = await io.call('POST', '/transfers', { key, body: original })
    const reused = await io.call('POST', '/transfers', {
      key,
      body: { ...original, amount: AMOUNT * 2 },
    })
    // Same request, different whitespace and indentation: must count as the
    // same request, so it replays the original transfer.
    const reformatted = await io.call('POST', '/transfers', {
      key,
      raw: JSON.stringify(original, null, 2),
    })
    const balance = await balanceOf(io, payee)
    const clean = await invariantsHold(io)

    const sameTransfer = transferId(first) !== undefined && transferId(first) === transferId(reformatted)
    const pass =
      first.status === 201 &&
      reused.status === 422 &&
      errorCode(reused.body) === 'idempotency_key_reused' &&
      reformatted.status === 201 &&
      reformatted.replayed &&
      sameTransfer &&
      balance === AMOUNT &&
      clean
    return {
      steps: [
        `Posted ${dollars(AMOUNT)} to Payee with key ${key.slice(0, 8)}….`,
        `Sent the same key for ${dollars(AMOUNT * 2)}.`,
        `Resent the original body with different whitespace.`,
        `Read Payee's balance and the sandbox invariants.`,
      ],
      expected: `201, then 422 idempotency_key_reused, then 201 replayed with the same transfer id; Payee at ${dollars(AMOUNT)}`,
      actual: `${first.status}, then ${reused.status} ${errorCode(reused.body) ?? ''}, then ${reformatted.status}${reformatted.replayed ? ' replayed' : ''}${sameTransfer ? ', same transfer id' : ', different transfer id'}; Payee at ${dollars(balance)}`,
      pass,
    }
  },
}
