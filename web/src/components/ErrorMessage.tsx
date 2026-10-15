import type { ApiError } from '../api'

// Plain-language explanations for every error the API can return.
// The key is kept on a final answer that the ledger recorded, and left free
// when the request never reached it. Each message says which, since that is
// what decides whether retrying the same key is useful.
const KEPT = 'The rejection is recorded under this idempotency key, so retrying the same key returns the same answer.'
const FREE = 'The ledger never saw it and nothing was recorded, so fix the request and send it again.'

// Plain-language explanations for every error code the API can return.
const COPY: Record<string, (e: ApiError) => string> = {
  // Business rejections, recorded by the ledger.
  insufficient_funds: () => `Insufficient funds. An account would go below zero and does not allow negative balances. ${KEPT}`,
  currency_mismatch: () => `All accounts in a transfer must use the same currency. ${KEPT}`,
  account_not_found: () => `One or more of the accounts does not exist. ${KEPT}`,
  // Validation, rejected before the ledger.
  unbalanced: (e) => `Debits and credits must be equal (${e.message.replace(/^.*?: /, '')}). ${FREE}`,
  validation_error: (e) => `${e.message.replace(/^invalid request: /, '')}. ${FREE}`,
  malformed_json: () => `The request body is not valid JSON for this endpoint. ${FREE}`,
  missing_idempotency_key: () => 'Writes need an Idempotency-Key header. The console always sends one, so this points at a bug.',
  body_too_large: () => `The request body is over 1 MB. ${FREE}`,
  invalid_limit: () => 'Page size must be between 1 and 500.',
  invalid_cursor: () => 'The pagination cursor is not one the API issued. Start again from the first page.',
  // Idempotency and reversal conflicts.
  idempotency_key_reused: () =>
    'This idempotency key was already used with a different request. A retry must resend the same request; ' +
    'send a new key for a new transfer.',
  request_in_progress: () =>
    'Another request with this key is still being processed. The key is kept, so retrying is safe.',
  already_reversed: () => 'This transfer has already been reversed. A transfer can be reversed at most once.',
  not_reversible: (e) => `This transfer can’t be reversed: ${e.message.replace(/^.*?: /, '')}.`,
  not_found: () => 'That account or transfer does not exist.',
  // Demo limits and sandboxes, checked before the ledger records anything.
  sandbox_limit: () =>
    `Your sandbox is full (it caps accounts and transfers so the demo stays small). Reset it on the Break It tab to start a fresh one. ${FREE}`,
  rate_limited: () => 'Too many writes from your address in a short time. Wait a few seconds and retry; the key is still free.',
  invalid_sandbox: () => 'The sandbox id this page sent is not a UUID. Reload the page to get a fresh one.',
  missing_sandbox: () => 'This action needs a sandbox, and the request did not name one. Reload the page.',
  // Transport and server failures: the outcome is unknown, so keep the key.
  internal: () => 'The API hit an internal error. The key is kept, so retrying is safe and cannot post twice.',
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
