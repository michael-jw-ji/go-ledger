// Money is integer minor units (cents) everywhere. Parsing works on the
// string so "0.1" becomes exactly 10, never 10.000000000000002.

const AMOUNT_RE = /^(\d+)?(?:\.(\d{1,2}))?$/

/** Parses "12.34" to 1234 cents. Returns null for anything invalid or zero. */
export function parseAmount(input: string): number | null {
  const s = input.trim()
  const m = AMOUNT_RE.exec(s)
  if (!m || (m[1] === undefined && m[2] === undefined)) return null
  const whole = m[1] ?? '0'
  const frac = (m[2] ?? '').padEnd(2, '0')
  if (whole.length > 13) return null // keeps us far below MAX_SAFE_INTEGER
  const cents = Number(whole) * 100 + Number(frac)
  return cents > 0 ? cents : null
}

const formatters = new Map<string, Intl.NumberFormat>()

/** Formats cents for display, e.g. 123456 USD -> "$1,234.56". */
export function formatMoney(cents: number, currency = 'USD'): string {
  let f = formatters.get(currency)
  if (!f) {
    f = new Intl.NumberFormat('en-US', { style: 'currency', currency })
    formatters.set(currency, f)
  }
  return f.format(cents / 100)
}

/** Formats with an explicit sign, for balance effects. */
export function formatSigned(cents: number, currency = 'USD'): string {
  if (cents === 0) return formatMoney(0, currency)
  return (cents > 0 ? '+' : '−') + formatMoney(Math.abs(cents), currency)
}
