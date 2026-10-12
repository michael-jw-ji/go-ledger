import { useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type Account } from './api'

export function useAccounts() {
  return useQuery({
    queryKey: ['accounts'],
    queryFn: async () => (await api.listAccounts()).data.accounts,
  })
}

export function useHealth() {
  return useQuery({
    queryKey: ['health'],
    queryFn: async () => {
      await api.health()
      return true
    },
    refetchInterval: 5000,
    retry: false,
  })
}

/** Refreshes everything a posting can change. */
export function useInvalidateLedger() {
  const qc = useQueryClient()
  return () => {
    void qc.invalidateQueries({ queryKey: ['accounts'] })
    void qc.invalidateQueries({ queryKey: ['entries'] })
    void qc.invalidateQueries({ queryKey: ['transfer'] })
  }
}

export function accountName(accounts: Account[] | undefined, id: number): string {
  return accounts?.find((a) => a.id === id)?.name ?? `#${id}`
}

/** A leg's effect on an account's balance, signed by its normal side. */
export function effect(a: Pick<Account, 'normal_balance'>, direction: 'debit' | 'credit', amount: number): number {
  return direction === a.normal_balance ? amount : -amount
}
