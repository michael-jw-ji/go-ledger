import { balanceOf, createAccount, dollars, flushRedis, invariantsHold, newKey, transferId, type Scenario } from './common'

const AMOUNT = 2500

export const redisWiped: Scenario = {
  id: 'redis-wiped',
  title: 'Redis wiped',
  chaos: true,
  summary: 'Posts a transfer, wipes the idempotency cache for your sandbox, then retries the same key. Postgres has to answer instead.',
  run: async (io) => {
    const funding = await createAccount(io, 'Funding', true)
    const payee = await createAccount(io, 'Payee', false)
    const key = newKey()
    const body = { debit_account_id: funding, credit_account_id: payee, amount: AMOUNT }

    const first = await io.call('POST', '/transfers', { key, body })
    const deleted = await flushRedis(io)
    const retry = await io.call('POST', '/transfers', { key, body })
    const balance = await balanceOf(io, payee)
    const clean = await invariantsHold(io)

    const same = transferId(first) !== undefined && transferId(first) === transferId(retry)
    const pass = first.status === 201 && retry.status === 201 && retry.replayed && same && balance === AMOUNT && clean
    return {
      steps: [
        `Posted ${dollars(AMOUNT)} to Payee with key ${key.slice(0, 8)}….`,
        `Deleted ${deleted} cached response${deleted === 1 ? '' : 's'} for your sandbox from Redis.`,
        `Retried the same key. Redis has no record of it now.`,
        `Read Payee's balance and the sandbox invariants.`,
      ],
      expected: `201, then 201 replayed by the Postgres backstop with the same transfer id; Payee at ${dollars(AMOUNT)}`,
      actual: `${first.status}, then ${retry.status}${retry.replayed ? ' replayed' : ''}${same ? ', same transfer id' : ', different transfer id'}; Payee at ${dollars(balance)}, invariants ${clean ? 'hold' : 'broken'}`,
      pass,
    }
  },
}
