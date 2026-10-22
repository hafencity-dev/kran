import { memo } from 'preact/compat'
import { drainNode, resumeNode } from '../actions'
import { ago, bytes, command, cores, duration, gib, ms, osLabel, pct, rate, shortCommand } from '../format'
import type { Sample, Snapshot } from '../store'
import type { Job, NodeState, OwnerKind } from '../types'
import { CopyCommand, ownerKindLabel, ownerKinds, stateLabel, useNow } from './bits'
import { startedAt } from './Jobs'
import { Sparkline } from './Sparkline'

interface Props {
  snap: Snapshot
  onOpenJob: (id: string) => void
}

export function Machines({ snap, onOpenJob }: Props) {
  const byNode = new Map<string, Job[]>()
  for (const j of snap.placed) {
    const k = j.node ?? ''
    const arr = byNode.get(k)
    if (arr) arr.push(j)
    else byNode.set(k, [j])
  }
  const kinds = new Set<OwnerKind>(snap.placed.map(j => j.spec.owner.kind))
  const alone = snap.nodes.length <= 1
  return (
    <section class="section machines" id="machines" aria-labelledby="machines-h">
      <div class="section-head">
        <h2 id="machines-h">Machines</h2>
        <Legend kinds={kinds} />
      </div>
      <div class="berths">
        {snap.nodes.map(n => (
          <Berth key={n.info.name} node={n} jobs={byNode.get(n.info.name) ?? NONE} samples={snap.samples.get(n.info.name) ?? NONE} onOpenJob={onOpenJob} />
        ))}
      </div>
      {alone && (
        <div class="teach">
          <p>
            Only {snap.self || 'this machine'} is here, so every job runs locally. Link another machine you can reach over SSH and heavy jobs move to
            whichever has room:
          </p>
          <CopyCommand cmd="kran link <host>" />
        </div>
      )}
    </section>
  )
}

const NONE: never[] = []

function Legend({ kinds }: { kinds: Set<OwnerKind> }) {
  return (
    <ul class="legend" aria-label="Legend">
      {ownerKinds
        .filter(k => kinds.has(k))
        .map(k => (
          <li key={k}>
            <span class={`swatch own-${k}`} aria-hidden="true" />
            {ownerKindLabel[k]}
          </li>
        ))}
      <li>
        <span class="swatch sw-others" aria-hidden="true" />
        Jobs from other machines
      </li>
      <li>
        <span class="swatch sw-phase" aria-hidden="true" />
        Syncing or setting up
      </li>
      <li>
        <span class="swatch sw-outside" aria-hidden="true" />
        Busy outside kran
      </li>
      <li>
        <span class="swatch sw-free" aria-hidden="true" />
        Free
      </li>
      <li>
        <span class="swatch sw-reserve" aria-hidden="true" />
        Kept for you
      </li>
    </ul>
  )
}

const statusText: Record<string, string> = { online: 'Online', offline: 'Offline', draining: 'Draining', degraded: 'Degraded' }

const Berth = memo(function Berth({ node: n, jobs, samples, onOpenJob }: { node: NodeState; jobs: Job[]; samples: readonly Sample[]; onOpenJob: (id: string) => void }) {
  const i = n.info
  const m = n.metrics
  const offline = n.status === 'offline'
  const gpu = i.gpus?.[0]
  const vm = i.os === 'darwin' ? i.runtimes?.find(r => r.kind === 'container' && r.available) : undefined
  const vmLive = m?.runtimes?.container
  return (
    <article class={`berth is-${n.status}`} aria-labelledby={`node-${i.name}`}>
      <div class="berth-id">
        <h3 id={`node-${i.name}`} class="node-name">
          {i.name}
        </h3>
        <p class="node-status">
          <span class={`dot dot-${n.status}`} aria-hidden="true" />
          <span>{statusText[n.status] ?? n.status}</span>
          {n.self && <span class="tag">this machine</span>}
        </p>
        {n.reason && <NodeReason node={n} />}
        <p class="node-spec">
          {i.os_version || osLabel(i.os, i.arch)}, {i.arch}
        </p>
        <p class="node-spec">
          {i.cpus} cores{i.perf_cpus && i.perf_cpus !== i.cpus ? ` (${i.perf_cpus} performance)` : ''}, {bytes(i.mem_total)}
        </p>
        {gpu && (
          <p class="node-spec">
            {gpu.model}
            {gpu.unified ? ', unified memory' : gpu.mem_total ? `, ${bytes(gpu.mem_total)}` : ''}
          </p>
        )}
        <p class="node-spec node-link">
          {n.self ? (
            'Jobs start here without syncing'
          ) : n.link?.connected ? (
            <>
              {ms(n.link.rtt_ms)} away, {rate(n.link.bandwidth_bps)}
            </>
          ) : (
            'Not connected'
          )}
        </p>
      </div>

      <div class="berth-strips">
        <Strip kind="cpu" node={n} jobs={jobs} onOpenJob={onOpenJob} />
        <Strip kind="mem" node={n} jobs={jobs} onOpenJob={onOpenJob} />
        {jobs.length > 0 && <OnNode jobs={jobs} onOpen={onOpenJob} />}
        {vm && !offline && (
          <p class="vm-note">
            Linux jobs run in its Docker VM: {vm.cpus} cores, {bytes(vm.mem)}
            {vmLive ? `, ${bytes(vmLive.mem_available)} available there now` : ''}.
          </p>
        )}
      </div>

      <div class="berth-vitals">
        <div class="vital">
          <span class="vital-label">CPU busy</span>
          <span class="vital-value num">{offline ? '–' : pct(m?.cpu_busy)}</span>
          <Sparkline samples={samples} pick={s => s.cpu} max={1} label="CPU busy" format={v => pct(v)} />
        </div>
        <div class="vital">
          <span class="vital-label">Memory used</span>
          <span class="vital-value num">{offline ? '–' : `${gib(m?.mem_used ?? 0)} of ${bytes(i.mem_total)}`}</span>
          <Sparkline samples={samples} pick={s => s.mem} max={i.mem_total} label="Memory used" format={v => bytes(v)} />
        </div>
        {!offline && <VitalsList node={n} />}
        <NodeAction node={n} />
      </div>
    </article>
  )
})

function NodeReason({ node: n }: { node: NodeState }) {
  const now = useNow()
  return (
    <p class="node-reason">
      {n.reason}
      {n.status === 'offline' && n.last_seen ? <span class="muted"> Last seen {ago(n.last_seen, now)}.</span> : null}
    </p>
  )
}

function VitalsList({ node: n }: { node: NodeState }) {
  const m = n.metrics
  const linux = n.info.os === 'linux'
  return (
    <dl class="vitals">
      <div>
        <dt>Load</dt>
        <dd class="num">{m.load1?.toFixed(1) ?? '–'}</dd>
      </div>
      {linux ? (
        <div>
          <dt title="Pressure stall information, share of time tasks waited (avg10)">Pressure</dt>
          <dd class="num">
            cpu {Math.round(m.cpu_pressure ?? 0)}, mem {Math.round(m.mem_pressure ?? 0)}, io {Math.round(m.io_pressure ?? 0)}
          </dd>
        </div>
      ) : (
        <div>
          <dt>Mem pressure</dt>
          <dd class="num">{Math.round(m.mem_pressure ?? 0)}</dd>
        </div>
      )}
      {m.gpu_util != null && m.gpu_util > 0 ? (
        <div>
          <dt>GPU</dt>
          <dd class="num">{pct(m.gpu_util)}</dd>
        </div>
      ) : null}
      {m.swap_used > 0 && (
        <div>
          <dt>Swap</dt>
          <dd class="num">{bytes(m.swap_used)}</dd>
        </div>
      )}
      <div>
        <dt>Disk free</dt>
        <dd class="num">{bytes(m.disk_free)}</dd>
      </div>
    </dl>
  )
}

function NodeAction({ node: n }: { node: NodeState }) {
  if (n.status === 'draining')
    return (
      <button type="button" class="btn btn-small" onClick={() => resumeNode(n)}>
        Resume {n.info.name}
      </button>
    )
  if (n.status === 'online' || n.status === 'degraded')
    return (
      <button type="button" class="btn btn-small" onClick={() => drainNode(n)}>
        Drain {n.info.name}
      </button>
    )
  return null
}

// ---------------------------------------------------------------------------
// The strip: one track per resource, scaled to capacity + reserve. Jobs are
// blocks sized by what the scheduler allocated to them; then work outside
// kran, free room, and the owner's reserve.

interface Seg {
  key: string
  v: number
  cls: string
  job?: Job
}

function Strip({ kind, node: n, jobs, onOpenJob }: { kind: 'cpu' | 'mem'; node: NodeState; jobs: Job[]; onOpenJob: (id: string) => void }) {
  const cpu = kind === 'cpu'
  const val = (r: { cpus: number; mem: number } | undefined) => (r ? (cpu ? r.cpus : r.mem) : 0) || 0
  const cap = val(n.capacity)
  const reserve = val(n.reserve)
  const alloc = val(n.allocated)
  const offline = n.status === 'offline'
  const free = offline ? 0 : val(n.free)
  const jobsSum = jobs.reduce((s, j) => s + val(j.alloc), 0)
  const used = Math.max(alloc, jobsSum)
  const outside = offline ? 0 : Math.max(0, cap - used - free)
  const total = Math.max(cap, used + outside + free) + reserve
  const segs: Seg[] = jobs.map(j => ({ key: j.spec.id, v: val(j.alloc), cls: '', job: j }))
  // Jobs submitted on other machines run here too; this daemon only sees
  // them as part of the node's allocation.
  const others = offline ? 0 : Math.max(0, alloc - jobsSum)
  if (others > (cpu ? 0.05 : 64 * 1024 * 1024)) segs.push({ key: 'others', v: others, cls: 'seg-others' })
  if (outside > 0) segs.push({ key: 'outside', v: outside, cls: 'seg-outside' })
  if (free > 0) segs.push({ key: 'free', v: free, cls: 'seg-free' })
  if (reserve > 0) segs.push({ key: 'reserve', v: reserve, cls: 'seg-reserve' })
  const fmt = (v: number) => (cpu ? cores(v) : gib(v))
  const unit = cpu ? 'cores' : 'GiB'
  const over = used > cap
  const summary = offline
    ? `${n.info.name} is offline`
    : `${fmt(used)} ${unit} allocated to ${jobs.length} ${jobs.length === 1 ? 'job' : 'jobs'}${others > 0 ? ' from here and more from other machines' : ''}${over ? `, ${fmt(used - cap)} over capacity` : ''}, ${fmt(free)} free, ${fmt(reserve)} kept for you`
  let x = 0
  return (
    <div class={`strip-row strip-${kind}`}>
      <div class="strip-head">
        <span class="strip-label">{cpu ? 'CPU' : 'Memory'}</span>
        <span class="strip-sum num" aria-hidden="true">
          {offline ? (
            'offline'
          ) : (
            <>
              <b>{fmt(free)}</b> of {fmt(cap)} {unit} free
              {over && <span class="over">, overcommitted by {fmt(used - cap)}</span>}
            </>
          )}
        </span>
      </div>
      <div
        class={`strip${offline ? ' strip-off' : ''}`}
        role="group"
        aria-label={`${cpu ? 'CPU' : 'Memory'}: ${summary}`}
        style={cpu ? { '--cells': String(total) } : undefined}
      >
        {segs.map(s => {
          const left = (x / total) * 100
          const width = (s.v / total) * 100
          x += s.v
          const style = { left: `${left}%`, width: `max(2px, calc(${width}% - 2px))` }
          if (!s.job) return <span key={s.key} class={`seg ${s.cls}`} style={style} />
          return <JobBlock key={s.key} job={s.job} kind={kind} style={style} right={left + width / 2 > 60} onOpen={onOpenJob} />
        })}
      </div>
    </div>
  )
}

function JobBlock({ job: j, kind, style, right, onOpen }: { job: Job; kind: 'cpu' | 'mem'; style: Record<string, string>; right: boolean; onOpen: (id: string) => void }) {
  const cpu = kind === 'cpu'
  const alloc = cpu ? j.alloc.cpus : j.alloc.mem
  const live = j.live ? (cpu ? j.live.cpus : j.live.mem) : 0
  const ratio = alloc > 0 ? Math.min(1, live / alloc) : 0
  const phase = j.state !== 'running'
  const cmd = command(j.spec.argv, j.spec.shell)
  const label = `${cmd}, ${stateLabel[j.state].toLowerCase()} for ${j.spec.owner.name}. Allocated ${cores(j.alloc.cpus)} cores and ${bytes(j.alloc.mem)}${j.live ? `, using ${cores(j.live.cpus)} cores and ${bytes(j.live.mem)}` : ''}.`
  const cls = `seg blk own-${j.spec.owner.kind}${phase ? ' blk-phase' : ''}`
  const tip = (
    <span class={`tip${right ? ' tip-right' : ''}`} aria-hidden="true">
      <code>{cmd}</code>
      <span>
        {j.spec.owner.name}, {stateLabel[j.state].toLowerCase()}
      </span>
      <span class="num">{cpu ? `${cores(live)} of ${cores(alloc)} cores in use` : `${bytes(live)} of ${bytes(alloc)} in use`}</span>
    </span>
  )
  if (!cpu) {
    // The memory strip repeats the CPU strip's jobs; one keyboard stop per
    // job is enough, so this one is pointer-only.
    return (
      <span class={cls} style={style} aria-hidden="true" onClick={() => onOpen(j.spec.id)}>
        {tip}
      </span>
    )
  }
  return (
    <button type="button" class={cls} style={style} aria-label={label} onClick={() => onOpen(j.spec.id)}>
      <span class="blk-label">{phase ? stateLabel[j.state] : shortCommand(j.spec.argv, j.spec.shell)}</span>
      {!phase && <span class="blk-live" style={{ transform: `scaleX(${ratio})` }} />}
      {tip}
    </button>
  )
}

const ON_NODE = 5

/** The jobs in the strip as text: what runs here and what it uses right now. */
function OnNode({ jobs, onOpen }: { jobs: Job[]; onOpen: (id: string) => void }) {
  const shown = jobs.slice(0, ON_NODE)
  return (
    <ul class="onnode" aria-label="Jobs on this machine">
      {shown.map(j => (
        <li key={j.spec.id}>
          <span class={`swatch own-${j.spec.owner.kind}`} aria-hidden="true" />
          <button type="button" class="cmd-link" onClick={() => onOpen(j.spec.id)} title={`${command(j.spec.argv, j.spec.shell)} (${j.spec.owner.name})`}>
            <code>{command(j.spec.argv, j.spec.shell)}</code>
          </button>
          {j.state === 'running' && j.live ? (
            <>
              <span class="num on-cpu">
                {cores(j.live.cpus)}
                <span class="muted">/{cores(j.alloc.cpus)} cores</span>
              </span>
              <span class="num on-mem">
                {gib(j.live.mem)}
                <span class="muted">/{bytes(j.alloc.mem)}</span>
              </span>
            </>
          ) : (
            <span class="on-phase muted">{stateLabel[j.state].toLowerCase()}</span>
          )}
          <OnNodeElapsed job={j} />
        </li>
      ))}
      {jobs.length > ON_NODE && (
        <li class="onnode-more muted">
          and {jobs.length - ON_NODE} more under <a href="#running">Running</a>
        </li>
      )}
    </ul>
  )
}

function OnNodeElapsed({ job }: { job: Job }) {
  const now = useNow()
  const s = startedAt(job)
  return <span class="num on-time muted">{s ? duration((now - s) / 1000) : ''}</span>
}
