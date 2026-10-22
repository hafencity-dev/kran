import { memo } from 'preact/compat'
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks'
import { cancelJob } from '../actions'
import { api, sseURL } from '../api'
import { Terminal, type Line } from '../ansi'
import { bytes, clock, command, cores, duration, roughDuration, validTime } from '../format'
import { store } from '../store'
import type { Job } from '../types'
import { isActive } from '../types'
import { Icon, OwnerChip, StateBadge, useNow } from './bits'
import { PrioritySelect, startedAt, where } from './Jobs'

export function JobDrawer({ id, job, onClose }: { id: string | null; job?: Job; onClose: () => void }) {
  const ref = useRef<HTMLDialogElement>(null)
  const [missing, setMissing] = useState(false)

  useEffect(() => {
    const d = ref.current
    if (!d) return
    if (id && !d.open) d.showModal()
    if (!id && d.open) d.close()
  }, [id])

  // Deep links and old jobs: fetch what the live state does not have.
  useEffect(() => {
    setMissing(false)
    if (!id || job) return
    let live = true
    api.job(id).then(
      j => live && store.mergeJobs([j]),
      () => live && setMissing(true),
    )
    return () => {
      live = false
    }
  }, [id, !!job])

  return (
    <dialog
      ref={ref}
      class="drawer"
      aria-labelledby="drawer-title"
      onClose={onClose}
      onClick={e => {
        // A click on the backdrop lands on the dialog element itself.
        if (e.target === ref.current) onClose()
      }}
    >
      {id && (
        <div class="drawer-inner">
          <div class="drawer-head">
            <h2 id="drawer-title">{job ? 'Job details' : missing ? 'Job not found' : 'Loading job'}</h2>
            <button type="button" class="btn btn-quiet btn-icon" onClick={onClose} aria-label="Close job details">
              <Icon name="close" />
            </button>
          </div>
          {job ? <Detail job={job} /> : missing ? <p class="quiet-empty">Job {id} is not in this daemon's history any more.</p> : null}
        </div>
      )}
    </dialog>
  )
}

function Detail({ job: j }: { job: Job }) {
  const now = useNow()
  const active = isActive(j.state)
  const cmd = command(j.spec.argv, j.spec.shell)
  const start = startedAt(j)
  const end = validTime(j.ended_at)
  const wall = j.usage?.wall_seconds || (start ? ((end || now) - start) / 1000 : 0)
  const avgCPU = j.usage?.cpu_seconds && wall > 0 ? j.usage.cpu_seconds / wall : 0
  const est = j.estimate
  const source =
    est.source === 'learned'
      ? `learned from ${est.samples} ${est.samples === 1 ? 'run' : 'runs'}`
      : est.source?.startsWith('repo:')
        ? `learned from ${est.repo_samples ?? 0} ${est.source.slice(5).replace('-', ' ')} ${est.repo_samples === 1 ? 'run' : 'runs'} in this repo`
        : est.source?.startsWith('family:')
        ? `default for ${est.source.slice(7).replace('-', ' ')}`
        : est.source === 'flag'
          ? 'from the command flags'
          : est.source === 'rule'
            ? 'from a .kran.toml rule'
            : est.source || 'unknown'
  return (
    <>
      <div class="detail-top">
        <StateBadge job={j} />
        <code class="detail-cmd">{cmd}</code>
        {active && (
          <div class="detail-actions">
            {j.state === 'queued' && <PrioritySelect job={j} />}
            <button type="button" class="btn btn-small btn-danger-quiet" onClick={() => cancelJob(j)}>
              Cancel job
            </button>
          </div>
        )}
      </div>
      {j.reason && (
        <p class={`detail-reason${j.state === 'queued' ? ' is-wait' : ''}`}>
          <span class="sr-only">Reason: </span>
          {j.reason}
        </p>
      )}
      <dl class="facts">
        <div>
          <dt>Owner</dt>
          <dd>
            <OwnerChip owner={j.spec.owner} />
          </dd>
        </div>
        <div>
          <dt>Where</dt>
          <dd>
            {where(j)}
            {j.local && !j.fallback ? ' (here)' : ''}
          </dd>
        </div>
        <div>
          <dt>Workspace</dt>
          <dd>
            {j.spec.workspace.name}
            {j.spec.workspace.branch ? ` on ${j.spec.workspace.branch}` : ''}
          </dd>
        </div>
        <div>
          <dt>Directory</dt>
          <dd>
            <code class="path">{j.spec.cwd}</code>
          </dd>
        </div>
        <div>
          <dt>Priority</dt>
          <dd>{j.spec.priority}</dd>
        </div>
        <div>
          <dt>Queued</dt>
          <dd class="num">{clock(j.queued_at)}</dd>
        </div>
        <div>
          <dt>Started</dt>
          <dd class="num">{start && j.state !== 'queued' ? clock(new Date(start).toISOString()) : '–'}</dd>
        </div>
        <div>
          <dt>{end ? 'Ended' : 'Elapsed'}</dt>
          <dd class="num">{end ? clock(j.ended_at) : j.state === 'queued' ? '–' : duration(wall)}</dd>
        </div>
        {j.exit_code != null && (
          <div>
            <dt>Exit code</dt>
            <dd class="num">
              {j.exit_code}
              {j.signal ? ` (${j.signal})` : ''}
            </dd>
          </div>
        )}
        <div>
          <dt>Job ID</dt>
          <dd>
            <code>{j.spec.id}</code>
          </dd>
        </div>
      </dl>

      <table class="compare">
        <caption>
          Estimate vs. actual <span class="muted">(estimate {source})</span>
        </caption>
        <thead>
          <tr>
            <th scope="col">
              <span class="sr-only">Measure</span>
            </th>
            <th scope="col" class="r">
              Estimate
            </th>
            <th scope="col" class="r">
              Allocated
            </th>
            <th scope="col" class="r">
              {active ? 'So far' : 'Actual'}
            </th>
          </tr>
        </thead>
        <tbody class="num">
          <tr>
            <th scope="row">CPU</th>
            <td class="r">{cores(est.cpus)} cores</td>
            <td class="r">{j.alloc?.cpus ? `${cores(j.alloc.cpus)} cores` : '–'}</td>
            <td class="r">{j.live && active ? `${cores(j.live.cpus)} now` : avgCPU ? `${cores(avgCPU)} on average` : '–'}</td>
          </tr>
          <tr>
            <th scope="row">Memory</th>
            <td class="r">{bytes(est.mem)}</td>
            <td class="r">{j.alloc?.mem ? bytes(j.alloc.mem) : '–'}</td>
            <td class="r">{j.usage?.peak_mem ? `${bytes(j.usage.peak_mem)} peak` : '–'}</td>
          </tr>
          <tr>
            <th scope="row">Duration</th>
            <td class="r">{est.duration_s ? roughDuration(est.duration_s) : '–'}</td>
            <td class="r">–</td>
            <td class="r">{wall && j.state !== 'queued' ? duration(wall) : '–'}</td>
          </tr>
        </tbody>
      </table>
      {(j.usage?.sync_bytes || j.usage?.setup_seconds) && (
        <p class="detail-note num">
          {j.usage.sync_bytes ? `Sent ${bytes(j.usage.sync_bytes)} of changed files in ${duration(j.usage.sync_seconds ?? 0)}.` : ''}
          {j.usage.setup_seconds ? ` Project setup took ${duration(j.usage.setup_seconds)}.` : ''}
        </p>
      )}
      <LogView key={j.spec.id} id={j.spec.id} active={active} />
    </>
  )
}

// ---------------------------------------------------------------------------

function LogView({ id, active }: { id: string; active: boolean }) {
  const term = useRef(new Terminal(4000))
  const pre = useRef<HTMLPreElement>(null)
  const [, setVer] = useState(0)
  const [stick, setStick] = useState(true)
  const [status, setStatus] = useState<'loading' | 'live' | 'done' | 'error'>('loading')
  const activeRef = useRef(active)
  activeRef.current = active

  useEffect(() => {
    let offset = 0
    let es: EventSource | null = null
    let timer = 0
    let raf = 0
    let closed = false
    const render = () => {
      if (!raf)
        raf = requestAnimationFrame(() => {
          raf = 0
          setVer(v => v + 1)
        })
    }
    const open = () => {
      es = new EventSource(sseURL(`/api/v1/jobs/${encodeURIComponent(id)}/log?follow=1&offset=${offset}`))
      es.onmessage = m => {
        setStatus('live')
        let data = m.data as string
        try {
          const p = JSON.parse(m.data)
          if (p && typeof p.data === 'string') {
            data = p.data
            if (typeof p.offset === 'number') offset = p.offset
          }
        } catch {
          // Plain-text SSE lines: each message is one line of output.
          data = m.data + '\n'
          offset += new TextEncoder().encode(data).length
        }
        term.current.write(data)
        render()
      }
      es.addEventListener('end', () => {
        es?.close()
        setStatus('done')
      })
      es.onerror = () => {
        es?.close()
        if (closed) return
        if (!activeRef.current) {
          // Finished job: one plain read is enough.
          api.logText(id, offset).then(
            t => {
              if (closed) return
              term.current.write(t)
              setStatus('done')
              render()
            },
            () => !closed && setStatus('error'),
          )
          return
        }
        setStatus('error')
        timer = window.setTimeout(open, 2000)
      }
    }
    open()
    return () => {
      closed = true
      es?.close()
      window.clearTimeout(timer)
      cancelAnimationFrame(raf)
    }
  }, [id])

  useLayoutEffect(() => {
    const el = pre.current
    if (el && stick) el.scrollTop = el.scrollHeight
  })

  const lines = term.current.view()
  const onScroll = () => {
    const el = pre.current
    if (!el) return
    setStick(el.scrollHeight - el.scrollTop - el.clientHeight < 24)
  }
  return (
    <section class="log" aria-labelledby="log-h">
      <div class="log-head">
        <h3 id="log-h">Output</h3>
        <span class="muted log-status" aria-live="polite">
          {status === 'loading' ? 'Loading' : status === 'live' && active ? 'Following live' : status === 'error' ? (active ? 'Connection lost, retrying' : 'Output is not available') : 'Complete'}
          {term.current.dropped > 0 && `, showing the last ${lines.length} lines`}
        </span>
        {!stick && (
          <button type="button" class="btn btn-small btn-quiet" onClick={() => setStick(true)}>
            Jump to latest
          </button>
        )}
      </div>
      <pre ref={pre} class="log-body" tabIndex={0} onScroll={onScroll} aria-label="Job output">
        {lines.length === 0 ? <span class="muted">{status === 'loading' ? '' : 'No output yet.'}</span> : lines.map((l, i) => <LogLine key={i} line={l} n={i === lines.length - 1 ? textLen(l) : 0} />)}
      </pre>
    </section>
  )
}

const LogLine = memo(
  function LogLine({ line }: { line: Line; n: number }) {
    return (
      <div class="ll">
        {line.length === 0 ? ' ' : line.map((s, i) => (s.cls ? <span key={i} class={s.cls}>{s.text}</span> : s.text))}
      </div>
    )
  },
  // The line being written is mutated in place, so its text length is
  // passed in; finished lines never change.
  (a, b) => a.line === b.line && a.n === b.n,
)

function textLen(l: Line) {
  let n = 0
  for (const s of l) n += s.text.length
  return n
}
