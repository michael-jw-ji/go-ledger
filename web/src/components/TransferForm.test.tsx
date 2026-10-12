import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { Account } from '../api'
import { TransferForm } from './TransferForm'

const accounts: Account[] = [
  { id: 1, name: 'Funding', currency: 'USD', normal_balance: 'credit', allow_negative: true, balance: 0, lock_version: 0, created_at: '' },
  { id: 2, name: 'Alice', currency: 'USD', normal_balance: 'credit', allow_negative: false, balance: 1000, lock_version: 0, created_at: '' },
]

const posted = {
  id: '11111111-1111-1111-1111-111111111111',
  status: 'posted',
  entries: [],
  created_at: '',
}

function json(status: number, body: unknown, headers: Record<string, string> = {}) {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json', ...headers } })
}

type FetchMock = ReturnType<typeof vi.fn<typeof fetch>>
let fetchMock: FetchMock

beforeEach(() => {
  fetchMock = vi.fn<typeof fetch>()
  vi.stubGlobal('fetch', fetchMock)
})
afterEach(() => vi.unstubAllGlobals())

function transferPosts() {
  return fetchMock.mock.calls.filter(([url, init]) => url === '/api/transfers' && init?.method === 'POST')
}

function keyOf(call: Parameters<typeof fetch>) {
  return (call[1]?.headers as Record<string, string>)['Idempotency-Key']
}

async function fillForm() {
  const user = userEvent.setup()
  render(
    <QueryClientProvider client={new QueryClient()}>
      <TransferForm accounts={accounts} retryDelayMs={1} />
    </QueryClientProvider>,
  )
  await user.selectOptions(screen.getByLabelText('Debit account'), '1')
  await user.selectOptions(screen.getByLabelText('Credit account'), '2')
  await user.type(screen.getByLabelText('Amount'), '10.00')
  return user
}

describe('TransferForm', () => {
  it('posts once on a double-click', async () => {
    let resolve!: (r: Response) => void
    fetchMock.mockImplementation(() => new Promise((r) => (resolve = r)))
    const user = await fillForm()
    const button = screen.getByRole('button', { name: 'Post transfer' })

    await user.dblClick(button)
    expect(transferPosts()).toHaveLength(1)
    expect(screen.getByRole('button', { name: 'Posting…' })).toBeDisabled()

    resolve(json(201, posted))
    await screen.findByRole('status')
    expect(transferPosts()).toHaveLength(1)
    expect(JSON.parse(String(transferPosts()[0][1]?.body))).toEqual({
      debit_account_id: 1,
      credit_account_id: 2,
      amount: 1000,
    })
  })

  it('reuses the same key when retrying after a network error', async () => {
    fetchMock.mockRejectedValueOnce(new TypeError('Failed to fetch')).mockResolvedValueOnce(json(201, posted))
    const user = await fillForm()
    const keyBefore = screen.getByTestId('idem-key').textContent

    await user.click(screen.getByRole('button', { name: 'Post transfer' }))
    await screen.findByRole('alert')
    expect(screen.getByTestId('idem-key').textContent).toBe(keyBefore)

    await user.click(screen.getByRole('button', { name: 'Post transfer' }))
    await screen.findByRole('status')
    const [first, second] = transferPosts()
    expect(keyOf(first)).toBe(keyBefore)
    expect(keyOf(second)).toBe(keyBefore)
    // A final answer starts a new intent with a new key.
    expect(screen.getByTestId('idem-key').textContent).not.toBe(keyBefore)
  })

  it('retries a 409 in-progress answer automatically with the same key', async () => {
    fetchMock
      .mockResolvedValueOnce(json(409, { error: { code: 'request_in_progress', message: 'busy' } }))
      .mockResolvedValueOnce(json(201, posted, { 'Idempotent-Replayed': 'true' }))
    const user = await fillForm()

    await user.click(screen.getByRole('button', { name: 'Post transfer' }))
    await screen.findByText(/Replayed/)
    const calls = transferPosts()
    expect(calls).toHaveLength(2)
    expect(keyOf(calls[0])).toBe(keyOf(calls[1]))
  })

  it('shows insufficient funds clearly and rotates the key', async () => {
    fetchMock.mockResolvedValueOnce(
      json(422, {
        error: { code: 'insufficient_funds', message: 'nope' },
        transfer: { ...posted, status: 'rejected', error_code: 'insufficient_funds' },
      }),
    )
    const user = await fillForm()
    const keyBefore = screen.getByTestId('idem-key').textContent

    await user.click(screen.getByRole('button', { name: 'Post transfer' }))
    expect(await screen.findByText(/Insufficient funds/)).toBeInTheDocument()
    expect(screen.getByText('rotated (final answer)')).toBeInTheDocument()
    await waitFor(() => expect(screen.getByTestId('idem-key').textContent).not.toBe(keyBefore))
  })

  it('makes a new key when the form changes', async () => {
    const user = await fillForm()
    const keyBefore = screen.getByTestId('idem-key').textContent
    await user.type(screen.getByLabelText('Amount'), '5')
    expect(screen.getByTestId('idem-key').textContent).not.toBe(keyBefore)
  })
})
