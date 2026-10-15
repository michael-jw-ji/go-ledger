import { balanceOf, createAccount, dollars, flushRedis, invariantsHold, newKey, type Scenario } from './common'

const AMOUNT = 4000

export const crashAfterCommit: Scenario = {
  id: 'crash-after-commit',
  title: 'Crash after commit',
  chaos: true,
  summary: 'The server commits the transfer, then dies before answering. The client never learns whether it worked, so it retries.',
  run: async (io) => {
    const funding = await createAccount(io, 'Funding', true)
    const payee = await createAccount(io, 'Payee', false)
    const key = newKey()
    const body = { debit_account_id: funding, credit_account_id: payee, amount: AMOUNT }

    const crashed = await io.call('POST', '/transfers', { key, body, headers: { 'X-Chaos-Crash': 'after-commit' } })
    // No answer means the request may or may not have posted: the case
    // idempotency keys exist for.
    const noAnswer = crashed.status !== 201 && crashed.status !== 422
    const during = await io.call('POST', '/transfers', { key, body })
    // The dead server left its claim in Redis, which would expire after 30
    // seconds. Deleting it skips the wait.
    await flushRedis(io)
    const after = await io.call('POST', '/transfers', { key, body })
    const balance = await balanceOf(io, payee)
    const clean = await invariantsHold(io)

    const pass =
      noAnswer &&
      during.status === 409 &&
      during.code === 'request_in_progress' &&
      after.status === 201 &&
      after.replayed &&
      balance === AMOUNT &&
      clean
    return {
      steps: [
        `Posted ${dollars(AMOUNT)} to Payee with a header telling the server to crash after committing.`,
        `Retried at once with the same key.`,
        `Let the crashed request's claim expire (deleted it instead of waiting 30 seconds).`,
        `Retried again, then read Payee's balance and the sandbox invariants.`,
      ],
      expected: `no answer, then 409 request_in_progress, then 201 replayed; Payee at ${dollars(AMOUNT)} (posted once)`,
      actual: `${noAnswer ? 'no answer' : `HTTP ${crashed.status}`}, then ${during.status} ${during.code ?? ''}, then ${after.status}${after.replayed ? ' replayed' : ''}; Payee at ${dollars(balance)}, invariants ${clean ? 'hold' : 'broken'}`,
      pass,
    }
  },
}
