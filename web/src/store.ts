import { useEffect, useState } from 'preact/hooks'
import { api, ApiError, DownError, sseURL } from './api'
import { validTime } from './format'
import type { Job, KranEvent, NodeMetrics, NodeState, OwnerStat, State, Stats, VersionInfo } from './types'
import { isActive, isPlaced } from './types'

export type Conn = 'connecting' | 'live' | 'stale' | 'reconnecting' | 'down' | 'unauthorized'

export interface Sample {
  t: number
  cpu: number // 0..1
  mem: number // bytes used
}

export interface Activity {
  time: number
  level: string
  msg: string
}

export interface Snapshot {
  conn: Conn
  connDetail: string
  retryAt: number
  lastEventAt: number
  loaded: boolean
  self: string
  version?: VersionInfo
  nodes: NodeState[]
  jobs: ReadonlyMap<string, Job>
  /** syncing / setup / running, oldest first */
  placed: Job[]
  /** queued, in dispatch order */
  queued: Job[]
  /** terminal, newest first */
  finished: Job[]
  stats?: Stats
  owners: OwnerStat[]
  samples: ReadonlyMap<string, readonly Sample[]>
  activity: Activity[]
}

const WINDOW_MS = 10 * 60 * 1000
const STALE_MS = 9_000
const RESTART_MS = 30_000
const FLUSH_MS = 200
const MAX_FINISHED = 600

/**
 * Keeps one live copy of the daemon state. Events are applied to mutable
 * maps as they arrive and published to the UI at most every FLUSH_MS, so a
 * burst of events costs one render.
 */
class Store {
  private nodes = new Map<string, NodeState>()
  private jobs = new Map<string, Job>()
  private queue: string[] = []
  private stats?: Stats
  private owners: OwnerStat[] = []
  private self = ''
  private version?: VersionInfo
  private samples = new Map<string, Sample[]>()
  private activity: Activity[] = []
  private conn: Conn = 'connecting'
  private connDetail = ''
  private retryAt = 0
  private lastEventAt = 0
  private loaded = false
  private lastSeq = 0

  private es: EventSource | null = null
  private retryTimer = 0
  private flushTimer = 0
  private attempt = 0
  private gen = 0
  private listeners = new Set<(s: Snapshot) => void>()
  private snap: Snapshot = this.build()

  get snapshot() {
    return this.snap
  }

  subscribe(fn: (s: Snapshot) => void) {
    this.listeners.add(fn)
    return () => this.listeners.delete(fn)
  }

  start() {
    window.setInterval(() => this.checkStale(), 1000)
    document.addEventListener('visibilitychange', () => {
      // Background tabs may have had their stream throttled or dropped.
      if (document.visibilityState === 'visible' && (this.conn === 'down' || this.conn === 'stale')) this.connect()
    })
    void this.connect()
  }

  /** Reconnect now (user asked, or tab came back). */
  retryNow() {
    void this.connect()
  }

  private async connect() {
    const gen = ++this.gen
    window.clearTimeout(this.retryTimer)
    this.es?.close()
    this.es = null
    this.setConn(this.loaded ? 'reconnecting' : 'connecting', '')
    try {
      const st = await api.state()
      if (gen !== this.gen) return
      this.applyState(st)
      this.attempt = 0
      this.openStream()
    } catch (e) {
      if (gen === this.gen) this.fail(e)
    }
  }

  private fail(e: unknown) {
    this.es?.close()
    this.es = null
    if (e instanceof ApiError && (e.status === 401 || e.status === 403)) {
      this.setConn('unauthorized', e.hint ?? e.message)
      return // retrying cannot help without a token
    }
    this.attempt++
    const delay = Math.min(10_000, 1000 * 2 ** Math.min(this.attempt - 1, 4))
    this.retryAt = Date.now() + delay
    const detail = e instanceof ApiError ? `${e.message}${e.hint ? `. ${e.hint}` : ''}` : e instanceof DownError ? '' : String(e)
    this.setConn('down', detail)
    this.retryTimer = window.setTimeout(() => void this.connect(), delay)
  }

  private openStream() {
    const es = new EventSource(sseURL('/api/v1/events'))
    this.es = es
    this.lastSeq = 0
    const onData = (raw: string, name?: string) => {
      if (this.es !== es) return
      let ev: KranEvent
      try {
        const parsed = JSON.parse(raw)
        // Accept both a full proto.Event per message and named SSE events
        // whose data is only the payload.
        ev = parsed && typeof parsed === 'object' && 'type' in parsed && 'data' in parsed ? parsed : { type: name as KranEvent['type'], seq: 0, time: '', data: parsed }
      } catch {
        return
      }
      this.onEvent(ev)
    }
    es.onmessage = m => onData(m.data)
    for (const t of ['state', 'node', 'node.metrics', 'job', 'queue', 'stats', 'log'])
      es.addEventListener(t, m => onData((m as MessageEvent).data, t))
    es.onopen = () => {
      if (this.es === es) this.lastEventAt = Date.now()
    }
    es.onerror = () => {
      if (this.es !== es) return
      // EventSource retries on its own, but without a fresh state and with
      // no backoff we control; take over.
      es.close()
      this.es = null
      this.setConn('reconnecting', '')
      this.retryTimer = window.setTimeout(() => {
        api.version().then(
          () => void this.connect(),
          e => this.fail(e),
        )
      }, 500)
    }
  }

  private checkStale() {
    if (!this.es || this.lastEventAt === 0) return
    const quiet = Date.now() - this.lastEventAt
    if (quiet > RESTART_MS) {
      void this.connect()
    } else if (quiet > STALE_MS && this.conn === 'live') {
      this.setConn('stale', '')
    }
  }

  private setConn(c: Conn, detail: string) {
    if (c === this.conn && detail === this.connDetail) return
    this.conn = c
    this.connDetail = detail
    this.publish(true)
  }

  private onEvent(ev: KranEvent) {
    this.lastEventAt = Date.now()
    if (ev.seq && this.lastSeq && ev.type !== 'state' && ev.seq > this.lastSeq + 1) {
      // We missed events; only a fresh state is trustworthy now.
      this.lastSeq = 0
      void this.connect()
      return
    }
    if (ev.seq) this.lastSeq = ev.seq
    if (this.conn !== 'live') {
      this.conn = 'live'
      this.connDetail = ''
    }
    const d = ev.data as never
    switch (ev.type) {
      case 'state':
        this.applyState(d)
        break
      case 'node':
        this.putNode(d)
        break
      case 'node.metrics': {
        const { name, metrics } = d as { name: string; metrics: NodeMetrics }
        const n = this.nodes.get(name)
        if (n) {
          this.nodes.set(name, { ...n, metrics })
          this.sample(name, n.info.mem_total, metrics)
        }
        break
      }
      case 'job':
        this.putJob(d)
        break
      case 'queue':
        this.queue = (d as { queue: string[] | null }).queue ?? []
        break
      case 'stats':
        this.stats = d
        break
      case 'log': {
        const l = d as { level?: string; msg?: string; time?: string }
        this.activity = [{ time: validTime(l.time) || Date.now(), level: l.level ?? 'info', msg: l.msg ?? '' }, ...this.activity].slice(0, 30)
        break
      }
    }
    this.publish(false)
  }

  private applyState(st: State) {
    this.self = st.self
    this.version = st.version
    const names = new Set<string>()
    for (const n of st.nodes ?? []) {
      names.add(n.info.name)
      this.putNode(n)
    }
    for (const name of [...this.nodes.keys()]) if (!names.has(name)) this.nodes.delete(name)
    // Active jobs not in the snapshot have ended while we were away; drop
    // them rather than show ghosts. Finished jobs we already know stay.
    const ids = new Set((st.jobs ?? []).map(j => j.spec.id))
    for (const [id, j] of this.jobs) if (isActive(j.state) && !ids.has(id)) this.jobs.delete(id)
    for (const j of st.jobs ?? []) this.jobs.set(j.spec.id, j)
    this.queue = st.queue ?? []
    this.stats = st.stats
    this.owners = st.owners ?? []
    this.loaded = true
    this.lastEventAt = Date.now()
    this.publish(true)
  }

  private putNode(n: NodeState) {
    this.nodes.set(n.info.name, n)
    this.sample(n.info.name, n.info.mem_total, n.metrics)
  }

  private sample(name: string, memTotal: number, m: NodeMetrics) {
    const t = validTime(m?.time)
    if (!t || !memTotal) return
    let arr = this.samples.get(name)
    if (!arr) this.samples.set(name, (arr = []))
    if (arr.length && arr[arr.length - 1].t >= t) return
    arr.push({ t, cpu: m.cpu_busy, mem: m.mem_used })
    const cut = t - WINDOW_MS
    let drop = 0
    while (drop < arr.length && arr[drop].t < cut) drop++
    if (drop) arr.splice(0, drop)
  }

  private putJob(j: Job) {
    this.jobs.set(j.spec.id, j)
    if (!isActive(j.state)) this.trimFinished()
  }

  private trimFinished() {
    let finished = 0
    for (const j of this.jobs.values()) if (!isActive(j.state)) finished++
    if (finished <= MAX_FINISHED) return
    const ids = [...this.jobs.values()].filter(j => !isActive(j.state)).map(j => j.spec.id).sort()
    for (const id of ids.slice(0, finished - MAX_FINISHED)) this.jobs.delete(id)
  }

  /** History beyond what the state carries (filters, deep links). */
  mergeJobs(jobs: Job[]) {
    for (const j of jobs) {
      const cur = this.jobs.get(j.spec.id)
      // Live events are newer than a history page for active jobs.
      if (cur && isActive(cur.state)) continue
      this.jobs.set(j.spec.id, j)
    }
    this.trimFinished()
    this.publish(true)
  }

  private publish(now: boolean) {
    if (now) {
      window.clearTimeout(this.flushTimer)
      this.flushTimer = 0
      this.flush()
      return
    }
    if (this.flushTimer) return
    this.flushTimer = window.setTimeout(() => {
      this.flushTimer = 0
      this.flush()
    }, FLUSH_MS)
  }

  private flush() {
    this.snap = this.build()
    for (const fn of this.listeners) fn(this.snap)
  }

  private build(): Snapshot {
    const placed: Job[] = []
    const finished: Job[] = []
    const queuedSet: Job[] = []
    for (const j of this.jobs.values()) {
      if (isPlaced(j.state)) placed.push(j)
      else if (j.state === 'queued') queuedSet.push(j)
      else finished.push(j)
    }
    placed.sort((a, b) => (a.spec.id < b.spec.id ? -1 : 1))
    // Newest result first; ended_at can be missing on odd records.
    const endKey = (j: Job) => validTime(j.ended_at) || validTime(j.queued_at) || 0
    finished.sort((a, b) => endKey(b) - endKey(a) || (a.spec.id < b.spec.id ? 1 : -1))
    const order = new Map(this.queue.map((id, i) => [id, i]))
    queuedSet.sort((a, b) => (order.get(a.spec.id) ?? 1e9) - (order.get(b.spec.id) ?? 1e9) || (a.spec.id < b.spec.id ? -1 : 1))
    const nodes = [...this.nodes.values()].sort((a, b) => Number(b.self) - Number(a.self) || a.info.name.localeCompare(b.info.name))
    return {
      conn: this.conn,
      connDetail: this.connDetail,
      retryAt: this.retryAt,
      lastEventAt: this.lastEventAt,
      loaded: this.loaded,
      self: this.self,
      version: this.version,
      nodes,
      jobs: this.jobs,
      placed,
      queued: queuedSet,
      finished,
      stats: this.stats,
      owners: this.owners,
      samples: this.samples,
      activity: this.activity,
    }
  }
}

export const store = new Store()

export function useSnapshot(): Snapshot {
  const [snap, set] = useState(store.snapshot)
  useEffect(() => {
    set(store.snapshot)
    return store.subscribe(set)
  }, [])
  return snap
}
