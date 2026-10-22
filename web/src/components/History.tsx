import { memo } from 'preact/compat'
import { useEffect, useMemo, useState } from 'preact/hooks'
import { api } from '../api'
import { bytes, clock, command, duration, validTime } from '../format'
import { store } from '../store'
import type { Job, NodeState } from '../types'
import { OwnerChip, StateBadge } from './bits'
import { where } from './Jobs'

type Result = '' | 'succeeded' | 'failed' | 'canceled' | 'infra' | 'fallback'

const resultLabel: Record<Result, string> = {
  '': 'Any result',
  succeeded: 'Succeeded',
  failed: 'Failed',
  canceled: 'Canceled',
  infra: 'kran errors and lost jobs',
  fallback: 'Fell back to local',
}

const PAGE = 20

export function History({ jobs, nodes, onOpen }: { jobs: Job[]; nodes: NodeState[]; onOpen: (id: string) => void }) {
  const [owner, setOwner] = useState('')
  const [node, setNode] = useState('')
  const [result, setResult] = useState<Result>('')
  const [text, setText] = useState('')
  const [limit, setLimit] = useState(PAGE)

  // The state only carries the latest 100 finished jobs; ask for more of
  // what the filters select.
  useEffect(() => {
    let live = true
    api.jobs({ limit: 300, owner: owner || undefined, node: node || undefined }).then(
      list => live && list && store.mergeJobs(list),
      () => {
        /* history is best effort; the live state still shows recent jobs */
      },
    )
    return () => {
      live = false
    }
  }, [owner, node])

  const ownerOptions = useMemo(() => {
    const m = new Map<string, string>()
    for (const j of jobs) m.set(j.spec.owner.id, j.spec.owner.name)
    return [...m].sort((a, b) => a[1].localeCompare(b[1]))
  }, [jobs.length])

  const nodeOptions = useMemo(() => {
    const s = new Set(nodes.map(n => n.info.name))
    for (const j of jobs) if (j.node) s.add(j.node)
    return [...s].sort()
  }, [jobs.length, nodes.length])

  const q = text.trim().toLowerCase()
  const filtered = jobs.filter(j => {
    if (owner && j.spec.owner.id !== owner) return false
    if (node && j.node !== node) return false
    switch (result) {
      case 'infra':
        if (j.state !== 'infra_error' && j.state !== 'lost') return false
        break
      case 'fallback':
        if (!j.fallback) return false
        break
      case '':
        break
      default:
        if (j.state !== result) return false
    }
    if (q && !command(j.spec.argv, j.spec.shell).toLowerCase().includes(q) && !j.spec.workspace.name.toLowerCase().includes(q)) return false
    return true
  })
  const shown = filtered.slice(0, limit)
  const filtering = owner || node || result || q

  return (
    <section class="section" id="history" aria-labelledby="history-h">
      <div class="section-head">
        <h2 id="history-h">History</h2>
      </div>
      <form class="filters" onSubmit={e => e.preventDefault()} aria-label="Filter history">
        <label>
          <span>Owner</span>
          <select value={owner} onChange={e => setOwner((e.currentTarget as HTMLSelectElement).value)}>
            <option value="">Everyone</option>
            {ownerOptions.map(([id, name]) => (
              <option key={id} value={id}>
                {name}
              </option>
            ))}
          </select>
        </label>
        <label>
          <span>Machine</span>
          <select value={node} onChange={e => setNode((e.currentTarget as HTMLSelectElement).value)}>
            <option value="">All machines</option>
            {nodeOptions.map(n => (
              <option key={n} value={n}>
                {n}
              </option>
            ))}
          </select>
        </label>
        <label>
          <span>Result</span>
          <select value={result} onChange={e => setResult((e.currentTarget as HTMLSelectElement).value as Result)}>
            {(Object.keys(resultLabel) as Result[]).map(r => (
              <option key={r} value={r}>
                {resultLabel[r]}
              </option>
            ))}
          </select>
        </label>
        <label class="grow">
          <span>Command</span>
          <input type="search" value={text} placeholder="go test, vitest, a repo name" onInput={e => setText((e.currentTarget as HTMLInputElement).value)} />
        </label>
        {filtering && (
          <button
            type="button"
            class="btn btn-quiet btn-small"
            onClick={() => {
              setOwner('')
              setNode('')
              setResult('')
              setText('')
            }}
          >
            Clear filters
          </button>
        )}
      </form>
      <p class="section-note num" aria-live="polite">
        {filtered.length === jobs.length ? `${jobs.length} finished jobs` : `${filtered.length} of ${jobs.length} finished jobs match`}
      </p>
      {filtered.length === 0 ? (
        <p class="quiet-empty">{jobs.length === 0 ? 'No job has finished yet. They show up here with their result, time and resources.' : 'No finished job matches these filters.'}</p>
      ) : (
        <div class="table-wrap">
          <table class="jobs history">
            <thead>
              <tr>
                <th scope="col">Result</th>
                <th scope="col">Command</th>
                <th scope="col">Owner</th>
                <th scope="col">Machine</th>
                <th scope="col" class="r">
                  Ended
                </th>
                <th scope="col" class="r">
                  Took
                </th>
                <th scope="col" class="r">
                  vs. estimate
                </th>
                <th scope="col" class="r">
                  Peak memory
                </th>
              </tr>
            </thead>
            <tbody>
              {shown.map(j => (
                <HistoryRow key={j.spec.id} job={j} onOpen={onOpen} />
              ))}
            </tbody>
          </table>
        </div>
      )}
      {filtered.length > limit && (
        <button type="button" class="btn btn-quiet more" onClick={() => setLimit(limit + PAGE)}>
          Show {Math.min(PAGE, filtered.length - limit)} more
        </button>
      )}
    </section>
  )
}

const HistoryRow = memo(function HistoryRow({ job: j, onOpen }: { job: Job; onOpen: (id: string) => void }) {
  const cmd = command(j.spec.argv, j.spec.shell)
  const took = j.usage?.wall_seconds || (validTime(j.ended_at) && validTime(j.started_at) ? (validTime(j.ended_at) - validTime(j.started_at)) / 1000 : 0)
  const est = j.estimate?.duration_s ?? 0
  const delta = est > 0 && took > 0 ? took / est - 1 : null
  return (
    <tr onClick={e => !(e.target as HTMLElement).closest('button') && onOpen(j.spec.id)}>
      <td class="cell-result" data-label="Result">
        <StateBadge job={j} />
      </td>
      <td class="cell-cmd" data-label="Command">
        <button type="button" class="cmd-link" onClick={() => onOpen(j.spec.id)} title={cmd}>
          <code>{cmd}</code>
        </button>
        <span class="sub">
          {j.spec.workspace.name}
          {j.spec.workspace.branch ? ` on ${j.spec.workspace.branch}` : ''}
        </span>
      </td>
      <td class="cell-owner" data-label="Owner">
        <OwnerChip owner={j.spec.owner} />
      </td>
      <td class="cell-machine" data-label="Machine">
        {where(j)}
      </td>
      <td data-label="Ended" class="r num cell-ended">
        {clock(j.ended_at)}
      </td>
      <td data-label="Took" class="r num cell-took">
        {took ? duration(took) : '–'}
      </td>
      <td data-label="vs. estimate" class="r num cell-delta">
        {delta == null ? <span class="muted">–</span> : <Delta v={delta} />}
      </td>
      <td data-label="Peak memory" class="r num cell-peak">
        {j.usage?.peak_mem ? bytes(j.usage.peak_mem) : '–'}
      </td>
    </tr>
  )
})

function Delta({ v }: { v: number }) {
  const p = Math.round(v * 100)
  if (Math.abs(p) < 5) return <span class="muted">on estimate</span>
  return <span class={p > 0 ? 'slower' : 'faster'}>{p > 0 ? `${p} % slower` : `${-p} % faster`}</span>
}
