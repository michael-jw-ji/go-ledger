import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const KEY = 'ledger-lab:sandbox'
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/

// The id is memoized per module, so each test loads a fresh copy.
async function freshSandboxId() {
  vi.resetModules()
  const mod = await import('./sandbox')
  return mod.sandboxId
}

describe('sandboxId', () => {
  beforeEach(() => localStorage.clear())
  afterEach(() => vi.restoreAllMocks())

  it('creates an id once and keeps it across reloads', async () => {
    const first = (await freshSandboxId())()
    expect(first).toMatch(UUID)
    expect(localStorage.getItem(KEY)).toBe(first)
    expect((await freshSandboxId())()).toBe(first)
  })

  it('replaces a stored value that is not a UUID', async () => {
    localStorage.setItem(KEY, 'not-a-uuid')
    const id = (await freshSandboxId())()
    expect(id).toMatch(UUID)
    expect(localStorage.getItem(KEY)).toBe(id)
  })

  it('still works when storage is blocked', async () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('blocked')
    })
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('blocked')
    })
    expect((await freshSandboxId())()).toMatch(UUID)
  })
})
