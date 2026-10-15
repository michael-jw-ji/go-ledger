import {
  balanceOf,
  createAccount,
  dollars,
  invariantsHold,
  newKey,
  transferId,
  type Call,
  type Io,
  type Scenario,
} from './common'

export const STORM_SIZE = 20
const AMOUNT = 1000

/** Posts with one key and retries 409 "in progress" with the same key, as the
 *  console's own submit does, until the server gives a final answer. */
async function postUntilSettled(io: Io, body: unknown, key: string): Promise<Call> {
  let last: Call = { status: 0, replayed: false, body: null }
  for (let attempt = 0; attempt < 10; attempt++) {
    last = await io.call('POST', '/transfers', { key, body })
    if (!(last.status === 409 && last.code === 'request_in_progress')) return last
    await io.sleep(50 * 2 ** Math.min(attempt, 5))
  }
  return last
}

export const retryStorm: Scenario = {
  id: 'retry-storm',
  title: 'Retry storm',
  summary: `Sends the same transfer ${STORM_SIZE} times at once with one idempotency key. Money should move once.`,
  run: async (io) => {
    const funding = await createAccount(io, 'Funding', true)
    const payee = await createAccount(io, 'Payee', false)
    const key = newKey()
    const body = { debit_account_id: funding, credit_account_id: payee, amount: AMOUNT }

    const results = await Promise.all(
      Array.from({ length: STORM_SIZE }, () => postUntilSettled(io, body, key)),
    )
    const posted = results.filter((r) => r.status === 201 && !r.replayed)
    const replayed = results.filter((r) => r.status === 201 && r.replayed)
    const transfers = new Set(results.map(transferId))
    const balance = await balanceOf(io, payee)
    const clean = await invariantsHold(io)

    const pass =
      posted.length === 1 &&
      replayed.length === STORM_SIZE - 1 &&
      transfers.size === 1 &&
      balance === AMOUNT &&
      clean
    return {
      steps: [
        `Created Funding and Payee in your sandbox.`,
        `Sent ${STORM_SIZE} POSTs to /transfers with key ${key.slice(0, 8)}…, all at once.`,
        `Retried any 409 "in progress" answers with the same key.`,
        `Read Payee's balance and the sandbox invariants.`,
      ],
      expected: `1 posted, ${STORM_SIZE - 1} replayed, 1 transfer id, Payee at ${dollars(AMOUNT)}, invariants hold`,
      actual: `${posted.length} posted, ${replayed.length} replayed, ${transfers.size} transfer id(s), Payee at ${dollars(balance)}, invariants ${clean ? 'hold' : 'broken'}`,
      pass,
    }
  },
}
