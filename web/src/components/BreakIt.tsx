import { useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { liveIo, type Scenario, type ScenarioResult } from '../scenarios/common'
import { scenarios } from '../scenarios'

type Run =
  | { state: 'running' }
  | { state: 'done'; result: ScenarioResult }
  | { state: 'error'; message: string }

// Each button runs a real attack against the API in this visitor's sandbox.
// The invariants panel above should stay green through every run.
export function BreakIt() {
  const qc = useQueryClient()
  const [runs, setRuns] = useState<Record<string, Run>>({})

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

  return (
    <div>
      <p className="sub" style={{ marginTop: 0 }}>
        Each run creates its own accounts in your sandbox, which other visitors can't see. Watch the invariants above while it runs.
      </p>
      <div className="breaks">
        {scenarios.map((s) => {
          const r = runs[s.id]
          const busy = r?.state === 'running'
          return (
            <article key={s.id} className="box break-card" aria-label={s.title}>
              <div className="panel-header" style={{ marginTop: 0 }}>
                <span className="title">{s.title.toUpperCase()}</span>
                {r?.state === 'done' && (
                  <span className={`badge ${r.result.pass ? 'pass' : 'fail'}`}>{r.result.pass ? 'PASS' : 'FAIL'}</span>
                )}
              </div>
              <p>{s.summary}</p>
              <button type="button" className="btn" disabled={busy} onClick={() => void run(s)}>
                {busy ? 'Running…' : r ? 'Run again' : 'Run'}
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
        })}
      </div>
    </div>
  )
}
