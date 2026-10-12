import type { ApiError } from '../api'

// Plain-language explanations for every error the API can return.
const COPY: Record<string, (e: ApiError) => string> = {
  insufficient_funds: () =>
    'Insufficient funds. An account would go below zero and does not allow negative balances. ' +
    'The rejection is recorded under this idempotency key, so retrying the same key returns the same answer.',
  currency_mismatch: () => 'All accounts in a transfer must use the same currency.',
  account_not_found: () => 'One or more of the accounts does not exist.',
  unbalanced: (e) => `Debits and credits must be equal (${e.message.replace(/^.*?: /, '')}).`,
  validation_error: (e) => e.message.replace(/^invalid request: /, ''),
  idempotency_key_reused: () =>
    'This idempotency key was already used with a different request body. Send a new key for a new transfer.',
  request_in_progress: () =>
    'Another request with this key is still being processed. The key is kept, so retrying is safe.',
  already_reversed: () => 'This transfer has already been reversed.',
  not_reversible: (e) => `This transfer can’t be reversed: ${e.message.replace(/^.*?: /, '')}.`,
  network_error: () => 'Could not reach the API. The key is kept, so retrying is safe once it is back.',
  api_unreachable: () => 'The API did not answer (is `make run` running?). The key is kept, so retrying is safe.',
}

export function describeError(e: ApiError): string {
  return (COPY[e.code] ?? ((x: ApiError) => x.message))(e)
}

export function ErrorMessage({ error }: { error: ApiError }) {
  return (
    <div className="callout" role="alert">
      <span className="badge orange">{error.status || 'NET'} · {error.code}</span>
      <p style={{ margin: '8px 0 0' }}>{describeError(error)}</p>
    </div>
  )
}
