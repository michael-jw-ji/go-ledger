// Each visitor works in their own sandbox: the API scopes accounts, transfers
// and idempotency keys to the id sent in X-Sandbox-Id. The id is kept in
// localStorage so it survives a reload. If storage is blocked, it lasts for
// this page only, which still works, just without the reload survival.

const STORAGE_KEY = 'ledger-lab:sandbox'
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

let memo: string | null = null

export function sandboxId(): string {
  if (memo) return memo
  try {
    const stored = localStorage.getItem(STORAGE_KEY)
    if (stored && UUID_RE.test(stored)) {
      memo = stored
      return stored
    }
  } catch {
    // Storage is unavailable (private window, blocked site data). Fall through.
  }
  memo = crypto.randomUUID()
  try {
    localStorage.setItem(STORAGE_KEY, memo)
  } catch {
    // Keep the in-memory id.
  }
  return memo
}
