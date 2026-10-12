import { describe, expect, it } from 'vitest'
import { formatMoney, formatSigned, parseAmount } from './money'

describe('parseAmount', () => {
  it.each([
    ['12.34', 1234],
    ['12', 1200],
    ['0.1', 10],
    ['0.01', 1],
    ['.5', 50],
    [' 7.00 ', 700],
    ['1000000', 100000000],
  ])('%s -> %i cents', (input, want) => {
    expect(parseAmount(input)).toBe(want)
  })

  it.each(['', '0', '0.00', '-1', '1.234', '1,000', 'abc', '1e3', '.', '12.'])('rejects %j', (input) => {
    expect(parseAmount(input)).toBeNull()
  })
})

describe('formatMoney', () => {
  it('formats cents without float drift', () => {
    expect(formatMoney(123456)).toBe('$1,234.56')
    expect(formatMoney(-5)).toBe('-$0.05')
    expect(formatSigned(250)).toBe('+$2.50')
    expect(formatSigned(-250)).toBe('−$2.50')
  })
})
