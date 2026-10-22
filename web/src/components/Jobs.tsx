import { memo } from 'preact/compat'
import { useState } from 'preact/hooks'
import { cancelJob, setPriority } from '../actions'
import { bytes, command, cores, duration, gib, roughDuration, validTime } from '../format'
import type { Job, Priority } from '../types'
import { CopyCommand, OwnerChip, StateBadge, useNow } from './bits'

type Open = (id: string) => void

export function where(j: Job): string {
  if (!j.node) return '–'
  const rt = j.runtime === 'container' ? ', container' : ''
  if (j.fallback) return `${j.node} (fell back to local)`
  return `${j.node}${rt}`
}

/** When the job left the queue; falls back to queued_at for odd daemons. */
export function startedAt(j: Job): number {
  return validTime(j.started_at) || validTime(j.queued_at)
}

// ---------------------------------------------------------------------------

export function Running({ jobs, onOpen }: { jobs: Job[]; onOpen: Open }) {
  return (
    <section class="section" id="running" aria-labelledby="running-h">
      <div class="section-head">
        <h2 id="running-h">
          Running <span class="count num">{jobs.length}</span>
        </h2>
      </div>
      {jobs.length === 0 ? (
        <div class="teach">
          <p>Nothing is running. Put kran in front of a heavy command and it runs on whichever machine has room:</p>
          <CopyCommand cmd="kran run -- go test ./..." />
          <p class="muted">
            Coding agents can do this on their own after <code>kran agents install</code>.
          </p>
        </div>
      ) : (
        <div class="table-wrap">
          <table class="jobs running">
            <thead>
              <tr>
                <th scope="col">Phase</th>
                <th scope="col">Command</th>
                <th scope="col">Owner</th>
                <th scope="col">Where</th>
                <th scope="col">Time</th>
                <th scope="col">Using now</th>
                <th scope="col">
                  <span class="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {jobs.map(j => (
                <RunningRow key={j.spec.id} job={j} onOpen={onOpen} />
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  )
}

const RunningRow = memo(function RunningRow({ job: j, onOpen }: { job: Job; onOpen: Open }) {
  const cmd = command(j.spec.argv, j.spec.shell)
  return (
    <tr onClick={e => !(e.target as HTMLElement).closest('button,select,a') && onOpen(j.spec.id)}>
      <td class="cell-phase" data-label="Phase">
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
        {j.reason && <span class="sub reason-line">{j.reason}</span>}
      </td>
      <td class="cell-owner" data-label="Owner">
        <OwnerChip owner={j.spec.owner} />
      </td>
      <td class="cell-where" data-label="Where">
        <span class="where">{where(j)}</span>
        {j.local && !j.fallback && <span class="sub">here</span>}
      </td>
      <td data-label="Time" class="cell-time">
        <Elapsed job={j} />
      </td>
      <td data-label="Using now" class="cell-live num">
        <LiveUse job={j} />
      </td>
      <td class="cell-actions">
        <button type="button" class="btn btn-small btn-quiet" onClick={() => cancelJob(j)} aria-label={`Cancel ${cmd}`}>
          Cancel
        </button>
      </td>
    </tr>
  )
})

function Elapsed({ job: j }: { job: Job }) {
  const now = useNow()
  const start = startedAt(j)
  const el = start ? Math.max(0, (now - start) / 1000) : 0
  const est = j.estimate?.duration_s ?? 0
  const frac = est > 0 ? Math.min(1, el / est) : 0
  const over = est > 0 && el > est * 1.1
  return (
    <span class="elapsed">
      <span class="num el-main">{duration(el)}</span>
      {est > 0 && (
        <>
          <span class={`bar${over ? ' bar-over' : ''}`} aria-hidden="true">
            <span style={{ transform: `scaleX(${frac})` }} />
          </span>
          <span class="sub num">{over ? `${duration(el - est)} over the ${roughDuration(est)} estimate` : `estimate ${roughDuration(est)}`}</span>
        </>
      )}
    </span>
  )
}

function LiveUse({ job: j }: { job: Job }) {
  if (!j.live || j.state !== 'running') return <span class="muted">{j.state === 'running' ? '–' : 'not yet'}</span>
  return (
    <>
      <span>
        {cores(j.live.cpus)}
        <span class="muted of"> of {cores(j.alloc.cpus)}</span> <span class="muted">cores</span>
      </span>
      <span>
        {gib(j.live.mem)}
        <span class="muted of"> of {gib(j.alloc.mem)}</span> <span class="muted">GiB</span>
      </span>
    </>
  )
}

// ---------------------------------------------------------------------------

const SHOW = 40

export function Waiting({ jobs, onOpen }: { jobs: Job[]; onOpen: Open }) {
  const [all, setAll] = useState(false)
  const shown = all ? jobs : jobs.slice(0, SHOW)
  return (
    <section class="section" id="waiting" aria-labelledby="waiting-h">
      <div class="section-head">
        <h2 id="waiting-h">
          Waiting <span class="count num">{jobs.length}</span>
        </h2>
        {jobs.length > 0 && <p class="section-note">In the order they will start. Each says why it waits.</p>}
      </div>
      {jobs.length === 0 ? (
        <p class="quiet-empty">Nothing is waiting. When no machine has room, jobs line up here with the reason they wait.</p>
      ) : (
        <>
          <ol class="queue">
            {shown.map((j, i) => (
              <QueueItem key={j.spec.id} job={j} pos={i + 1} onOpen={onOpen} />
            ))}
          </ol>
          {jobs.length > SHOW && (
            <button type="button" class="btn btn-quiet more" onClick={() => setAll(!all)}>
              {all ? `Show the first ${SHOW}` : `Show all ${jobs.length}`}
            </button>
          )}
        </>
      )}
    </section>
  )
}

const QueueItem = memo(function QueueItem({ job: j, pos, onOpen }: { job: Job; pos: number; onOpen: Open }) {
  const cmd = command(j.spec.argv, j.spec.shell)
  const need = `${cores(j.spec.req.cpus || j.estimate.cpus)} cores, ${bytes(j.spec.req.mem || j.estimate.mem)}${j.spec.req.gpu ? ', a GPU' : ''}`
  return (
    <li class="qitem">
      <span class="qpos num" aria-label={`Position ${pos}`}>
        {pos}
      </span>
      <div class="qmain">
        <div class="qline">
          <button type="button" class="cmd-link" onClick={() => onOpen(j.spec.id)} title={cmd}>
            <code>{cmd}</code>
          </button>
          <Waited job={j} />
        </div>
        <div class="qmeta">
          <OwnerChip owner={j.spec.owner} />
          <span class="sub q-ws">
            {j.spec.workspace.name}
            {j.spec.workspace.branch ? ` on ${j.spec.workspace.branch}` : ''}
          </span>
          <span class="sub q-need">needs {need}</span>
        </div>
        <p class="qreason">{j.reason || 'waiting for the scheduler'}</p>
      </div>
      <div class="qactions">
        <PrioritySelect job={j} />
        <button type="button" class="btn btn-small btn-quiet" onClick={() => cancelJob(j)} aria-label={`Cancel ${cmd}`}>
          Cancel
        </button>
      </div>
    </li>
  )
})

function Waited({ job: j }: { job: Job }) {
  const now = useNow()
  const q = validTime(j.queued_at)
  return <span class="waited num">{q ? `waiting ${duration((now - q) / 1000)}` : ''}</span>
}

export function PrioritySelect({ job: j }: { job: Job }) {
  return (
    <label class="prio">
      <span class="sr-only">Priority for {command(j.spec.argv, j.spec.shell)}</span>
      <select value={j.spec.priority} onChange={e => setPriority(j, (e.currentTarget as HTMLSelectElement).value as Priority)}>
        <option value="high">High priority</option>
        <option value="normal">Normal priority</option>
        <option value="low">Low priority</option>
      </select>
    </label>
  )
}
