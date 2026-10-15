import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { chaosAvailable, liveIo, type Scenario, type ScenarioResult } from '../scenarios/common'
import { scenarios } from '../scenarios'
import { resetSandbox } from '../sandbox'

type Run =
  | { state: 'running' }
  | { state: 'done'; result: ScenarioResult }
  | { state: 'error'; message: string }

// Each button runs a real attack against the API in this visitor's sandbox.
// The invariants panel above should stay green through every run, except
// while "Corrupt a balance" deliberately breaks it.
export function BreakIt() {
  const qc = useQueryClient()
  const [runs, setRuns] = useState<Record<string, Run>>({})
  const [resetting, setResetting] = useState(false)
  const chaos = useQuery({ queryKey: ['chaos'], queryFn: () => chaosAvailable(liveIo), staleTime: Infinity })

  const run = async (s: Scenario) => {
    setRuns((prev) => ({ ...prev, [s.id]: { state: 'running' } }))
    try {
      const result = await s.run(liveIo)
      setRuns((prev) => ({ ...prev, [s.id]: { state: 'done', result } }))
    } catch (e) {
      setRuns((prev) => ({ ...prev, [s.id]: { state: 'error', message: e instanceof Error ? e.message : String(e) } }))
    } finally {
      void qc.invalidateQueries({ queryKey: ['invariants'] })
      void qc.invalidateQueries({ queryKey: ['accounts'] })
    }
  }

  const reset = async () => {
    setResetting(true)
    await resetSandbox()
    setRuns({})
    // Everything on screen belonged to the old sandbox.
    await qc.invalidateQueries()
    setResetting(false)
  }

  const busy = resetting || Object.values(runs).some((r) => r.state === 'running')
  const basic = scenarios.filter((s) => !s.chaos)
  const faults = scenarios.filter((s) => s.chaos)

  const card = (s: Scenario, unavailable: boolean) => {
    const r = runs[s.id]
    const running = r?.state === 'running'
    return (
      <article key={s.id} className="box break-card" aria-label={s.title}>
        <div className="panel-header" style={{ marginTop: 0 }}>
          <span className="title">{s.title.toUpperCase()}</span>
          {r?.state === 'done' && (
            <span className={`badge ${r.result.pass ? 'pass' : 'fail'}`}>{r.result.pass ? 'PASS' : 'FAIL'}</span>
          )}
        </div>
        <p>{s.summary}</p>
        <button type="button" className="btn" disabled={running || resetting || unavailable} onClick={() => void run(s)}>
          {running ? 'Running…' : r ? 'Run again' : 'Run'}
        </button>
        {r?.state === 'error' && (
          <p className="error" role="alert">
            The run stopped: {r.message}
          </p>
        )}
        {r?.state === 'done' && (
          <dl className="result">
            <dt>Steps</dt>
            <dd>
              <ol>
                {r.result.steps.map((step) => (
                  <li key={step}>{step}</li>
                ))}
              </ol>
            </dd>
            <dt>Expected</dt>
            <dd>{r.result.expected}</dd>
            <dt>Actual</dt>
            <dd>{r.result.actual}</dd>
          </dl>
        )}
      </article>
    )
  }

  return (
    <div>
      <div className="break-intro">
        <p className="sub" style={{ margin: 0 }}>
          Each run creates its own accounts in your sandbox, which other visitors can't see. Watch the invariants above
          while it runs.
        </p>
        <button type="button" className="btn ghost" disabled={busy} onClick={() => void reset()}>
          {resetting ? 'Resetting…' : 'Reset my sandbox'}
        </button>
      </div>

      <h2 className="break-group">Races and retries</h2>
      <div className="breaks">{basic.map((s) => card(s, false))}</div>

      <h2 className="break-group">Injected faults</h2>
      {chaos.data === false && (
        <p className="sub">
          This server was built without fault injection, so these are off. Run a chaos build (
          <code>go run -tags chaos ./cmd/ledger</code>) to try them.
        </p>
      )}
      <div className="breaks">{faults.map((s) => card(s, chaos.data !== true))}</div>
    </div>
  )
}
