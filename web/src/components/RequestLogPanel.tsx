import { Fragment, useState } from 'react'
import { clearLog, useRequestLog, type LogEntry } from '../log'

function statusClass(s: number): string {
  if (s >= 200 && s < 300) return 'solid'
  if (s === 409 || (s >= 400 && s < 500)) return 'orange'
  return 'muted'
}

export function RequestLogPanel() {
  const log = useRequestLog()
  const [open, setOpen] = useState<number | null>(null)

  return (
    <div>
      <div className="panel-header" style={{ marginTop: 0 }}>
        <span>
          <span className="orange">■</span> <span className="title">REQUEST LOG</span> · WRITES THIS SESSION, NEWEST FIRST
        </span>
        <button type="button" className="btn ghost sm" onClick={clearLog} disabled={log.length === 0}>
          Clear
        </button>
      </div>
      {log.length === 0 ? (
        <div className="empty">
          <p>No requests yet. Create an account or post a transfer and it shows up here.</p>
        </div>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Time</th>
                <th>Request</th>
                <th>Idempotency-Key</th>
                <th>Status</th>
                <th className="num">ms</th>
              </tr>
            </thead>
            <tbody>
              {log.map((e) => (
                <Fragment key={e.id}>
                  <tr className="clickable" onClick={() => setOpen(open === e.id ? null : e.id)} aria-expanded={open === e.id}>
                    <td className="mono dim">{e.at.toLocaleTimeString()}</td>
                    <td className="mono">
                      {e.method} {e.path}
                    </td>
                    <td className="mono dim" title={e.idempotencyKey}>
                      {e.idempotencyKey ? `${e.idempotencyKey.slice(0, 13)}…` : '—'}
                    </td>
                    <td>
                      <span className="badges">
                        <span className={`badge ${statusClass(e.status)}`}>{e.status || 'NET'}</span>
                        {e.replayed && <span className="badge">replayed</span>}
                        {errorCode(e) && <span className="badge orange">{errorCode(e)}</span>}
                      </span>
                    </td>
                    <td className="num dim">{e.ms}</td>
                  </tr>
                  {open === e.id && (
                    <tr className="log-detail">
                      <td colSpan={5}>
                        <div className="cols">
                          <div>
                            <div className="label" style={{ marginBottom: 6 }}>Request body</div>
                            <pre className="json">{e.requestBody === undefined ? '—' : JSON.stringify(e.requestBody, null, 2)}</pre>
                          </div>
                          <div>
                            <div className="label" style={{ marginBottom: 6 }}>Response</div>
                            <pre className="json">{e.responseBody == null ? '—' : JSON.stringify(e.responseBody, null, 2)}</pre>
                          </div>
                        </div>
                      </td>
                    </tr>
                  )}
                </Fragment>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}

function errorCode(e: LogEntry): string | undefined {
  return (e.responseBody as { error?: { code?: string } } | null)?.error?.code
}
