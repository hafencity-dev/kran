// Mirrors internal/proto/types.go (JSON). Keep in sync with the Go types.

export type NodeStatus = 'online' | 'offline' | 'draining' | 'degraded'
export type RuntimeKind = 'native' | 'container'

export interface GPU {
  vendor: string
  model: string
  mem_total?: number
  unified: boolean
}

export interface Runtime {
  kind: RuntimeKind
  os: string
  arch: string
  cpus: number
  mem: number
  available: boolean
  reason?: string
  toolchains?: Record<string, string>
  docker: boolean
}

export interface NodeInfo {
  id: string
  name: string
  hostname: string
  os: string
  arch: string
  os_version: string
  version: string
  cpu_model: string
  cpus: number
  perf_cpus: number
  mem_total: number
  disk_total: number
  gpus?: GPU[]
  runtimes: Runtime[]
  single_core_score: number
  labels?: Record<string, string>
  started_at: string
}

export interface RuntimeMetrics {
  cpu_busy: number
  mem_available: number
}

export interface NodeMetrics {
  time: string
  load1: number
  cpu_busy: number
  cpu_pressure?: number
  mem_used: number
  mem_available: number
  mem_pressure?: number
  swap_used: number
  swap_total: number
  io_pressure?: number
  disk_free: number
  gpu_util?: number
  gpu_mem_used?: number
  net_rx_bps: number
  net_tx_bps: number
  runtimes?: Partial<Record<RuntimeKind, RuntimeMetrics>>
}

export interface Resources {
  cpus: number
  mem: number
}

export interface LinkInfo {
  address?: string
  direction?: string
  rtt_ms?: number
  bandwidth_bps?: number
  connected: boolean
}

export interface NodeState {
  info: NodeInfo
  metrics: NodeMetrics
  status: NodeStatus
  reason?: string
  self: boolean
  link: LinkInfo
  capacity: Resources
  reserve: Resources
  allocated: Resources
  free: Resources
  running: number
  accepts: boolean
  last_seen?: string
}

export type JobState =
  | 'queued'
  | 'syncing'
  | 'setup'
  | 'running'
  | 'succeeded'
  | 'failed'
  | 'canceled'
  | 'infra_error'
  | 'lost'

export type Priority = 'high' | 'normal' | 'low'
export type OwnerKind = 'human' | 'claude' | 'codex' | 'agent' | 'ci'

export interface Owner {
  kind: OwnerKind
  id: string
  name: string
}

export interface Requirements {
  cpus?: number
  mem?: number
  gpu?: boolean
  os: string
  node?: string
  prefer: string
  origin_only?: boolean
  origin_only_reason?: string
  toolchains?: Record<string, string>
}

export interface Workspace {
  root: string
  repo_id: string
  rel: string
  git: boolean
  branch?: string
  name: string
}

export interface JobSpec {
  id: string
  argv: string[]
  shell?: string
  cwd: string
  workspace: Workspace
  env?: Record<string, string>
  req: Requirements
  tty?: { rows: number; cols: number } | null
  stdin: boolean
  owner: Owner
  priority: Priority
  labels?: Record<string, string>
  no_fallback?: boolean
  submitted_at: string
  client_pid: number
}

export interface Estimate {
  cpus: number
  mem: number
  duration_s: number
  samples: number
  source: string
  duration_source?: string
  repo_samples?: number
}

export interface Usage {
  cpu_seconds: number
  peak_mem: number
  wall_seconds: number
  sync_bytes?: number
  sync_seconds?: number
  setup_seconds?: number
}

export interface Job {
  spec: JobSpec
  state: JobState
  node?: string
  runtime?: RuntimeKind
  local: boolean
  fallback: boolean
  reason?: string
  estimate: Estimate
  alloc: Resources
  usage: Usage
  live?: Resources | null
  exit_code?: number | null
  signal?: string
  fingerprint: string
  queued_at: string
  started_at?: string
  ended_at?: string
  origin: string
  queue_pos?: number
}

export interface Stats {
  jobs_today: number
  offloaded_today: number
  fallbacks_today: number
  failed_infra_today: number
  saved_seconds_today: number
  cpu_seconds_by_node: Record<string, number> | null
}

export interface VersionInfo {
  version: string
  commit: string
  build_date: string
  api: string
  protocol: string
}

export interface OwnerStat {
  owner: Owner
  running: number
  queued: number
}

export interface State {
  version: VersionInfo
  self: string
  nodes: NodeState[] | null
  jobs: Job[] | null
  queue: string[] | null
  owners: OwnerStat[] | null
  stats: Stats
}

export type EventType = 'state' | 'node' | 'node.metrics' | 'job' | 'queue' | 'stats' | 'log'

export interface KranEvent {
  type: EventType
  seq: number
  time: string
  data: unknown
}

export interface APIError {
  error: string
  hint?: string
}

export const ACTIVE: ReadonlySet<JobState> = new Set(['queued', 'syncing', 'setup', 'running'])
export const isActive = (s: JobState) => ACTIVE.has(s)
export const isPlaced = (s: JobState) => s === 'syncing' || s === 'setup' || s === 'running'
