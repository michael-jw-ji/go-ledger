import { balanceOf, createAccount, dollars, errorCode, invariantsHold, newKey, type Scenario } from './common'

const PRICE = 10000

export const doubleSpend: Scenario = {
  id: 'double-spend',
  title: 'Double spend',
  summary: 'Two $100 payments race to drain a wallet that holds $100. Only one can succeed.',
  run: async (io) => {
    const funding = await createAccount(io, 'Funding', true)
    const wallet = await createAccount(io, 'Wallet', false)
    const merchant = await createAccount(io, 'Merchant', false)

    const funded = await io.call('POST', '/transfers', {
      key: newKey(),
      body: { debit_account_id: funding, credit_account_id: wallet, amount: PRICE },
    })
    if (funded.status !== 201) {
      return {
        steps: ['Funding the wallet failed, so the race never started.'],
        expected: `Wallet funded with ${dollars(PRICE)}`,
        actual: `funding answered HTTP ${funded.status}`,
        pass: false,
      }
    }

    const pay = () =>
      io.call('POST', '/transfers', {
        key: newKey(),
        body: { debit_account_id: wallet, credit_account_id: merchant, amount: PRICE },
      })
    const [a, b] = await Promise.all([pay(), pay()])
    const results = [a, b]
    const wins = results.filter((r) => r.status === 201).length
    const declined = results.filter((r) => r.status === 422 && errorCode(r.body) === 'insufficient_funds').length

    const walletBalance = await balanceOf(io, wallet)
    const merchantBalance = await balanceOf(io, merchant)
    const clean = await invariantsHold(io)

    const pass =
      wins === 1 &&
      declined === 1 &&
      walletBalance === 0 &&
      merchantBalance === PRICE &&
      clean
    return {
      steps: [
        `Funded Wallet with ${dollars(PRICE)} from Funding.`,
        `Sent two ${dollars(PRICE)} payments from Wallet to Merchant at the same time, with different keys.`,
        `Read both balances and the sandbox invariants.`,
      ],
      expected: `1 posted, 1 insufficient_funds, Wallet at $0.00 (never negative), Merchant at ${dollars(PRICE)}`,
      actual: `${wins} posted, ${declined} insufficient_funds, Wallet at ${dollars(walletBalance)}, Merchant at ${dollars(merchantBalance)}, invariants ${clean ? 'hold' : 'broken'}`,
      pass,
    }
  },
}
