import { describe, expect, it } from 'vitest'
import { ApiError } from '../api'
import { describeError } from './ErrorMessage'

// Every code the API can send (internal/httpapi and the ledger's error codes).
const API_CODES = [
  'insufficient_funds', 'currency_mismatch', 'account_not_found',
  'unbalanced', 'validation_error', 'malformed_json', 'missing_idempotency_key', 'body_too_large',
  'invalid_limit', 'invalid_cursor', 'idempotency_key_reused', 'request_in_progress',
  'already_reversed', 'not_reversible', 'not_found', 'internal',
]

describe('describeError', () => {
  it.each(API_CODES)('has plain-language copy for %s', (code) => {
    const raw = `raw server message for ${code}`
    const text = describeError(new ApiError(400, code, raw))
    expect(text).not.toBe(raw)
    expect(text.length).toBeGreaterThan(20)
  })

  it('says whether the key was used up', () => {
    expect(describeError(new ApiError(422, 'insufficient_funds', 'x'))).toMatch(/recorded under this idempotency key/)
    expect(describeError(new ApiError(422, 'unbalanced', 'invalid request: debits 5, credits 4'))).toMatch(/nothing was recorded/)
  })

  it('falls back to the server message for unknown codes', () => {
    expect(describeError(new ApiError(418, 'teapot', 'short and stout'))).toBe('short and stout')
  })
})
