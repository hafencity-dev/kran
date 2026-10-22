import type { ComponentChildren } from 'preact'
import { createContext } from 'preact'
import { useContext, useEffect, useRef, useState } from 'preact/hooks'
import type { Job, JobState, Owner, OwnerKind } from '../types'

// ---------------------------------------------------------------------------
// Clock: one interval for everything that shows elapsed time.

const NowContext = createContext(Date.now())

export function NowProvider({ children }: { children: ComponentChildren }) {
  const [now, setNow] = useState(Date.now())
  useEffect(() => {
    const t = window.setInterval(() => setNow(Date.now()), 1000)
    return () => window.clearInterval(t)
  }, [])
  return <NowContext.Provider value={now}>{children}</NowContext.Provider>
}

export const useNow = () => useContext(NowContext)

// ---------------------------------------------------------------------------
// Icons (16px grid, currentColor).

const paths: Record<string, string> = {
  running: 'M4 3.5v9l8-4.5z',
  syncing: 'M3 6h8.5L9 3.5M13 10H4.5L7 12.5',
  setup: 'M10.5 2.5a3 3 0 0 0-3.2 4L2.8 11a1.2 1.2 0 0 0 1.7 1.7l4.5-4.5a3 3 0 0 0 4-3.2l-1.8 1.8-1.7-.4-.4-1.7z',
  queued: 'M8 2.5a5.5 5.5 0 1 0 0 11 5.5 5.5 0 0 0 0-11zM8 5v3.2l2 1.3',
  succeeded: 'M3 8.5l3 3 7-7',
  failed: 'M4 4l8 8M12 4l-8 8',
  canceled: 'M8 2.5a5.5 5.5 0 1 0 0 11 5.5 5.5 0 0 0 0-11zM4.2 11.8l7.6-7.6',
  infra_error: 'M8 2l6.5 11.5h-13zM8 6.5v3.2M8 11.6v.1',
  lost: 'M6.5 9.5l3-3M5 7.5L3.5 9a2.5 2.5 0 0 0 3.5 3.5L8.5 11M11 8.5L12.5 7A2.5 2.5 0 0 0 9 3.5L7.5 5',
  close: 'M4 4l8 8M12 4l-8 8',
  copy: 'M5.5 5.5h7v7h-7zM3.5 10.5v-7h7',
  check: 'M3 8.5l3 3 7-7',
  log: 'M3 4h10M3 8h10M3 12h6',
  retry: 'M12.5 8A4.5 4.5 0 1 1 11 4.6M12.5 2.5v3h-3',
}

export function Icon({ name, label }: { name: keyof typeof paths | string; label?: string }) {
  const filled = name === 'running'
  return (
    <svg class="icon" viewBox="0 0 16 16" width="16" height="16" aria-hidden={label ? undefined : 'true'} role={label ? 'img' : undefined} aria-label={label}>
      <path d={paths[name] ?? paths.queued} fill={filled ? 'currentColor' : 'none'} stroke="currentColor" stroke-width={filled ? 0 : 1.6} stroke-linecap="round" stroke-linejoin="round" />
    </svg>
  )
}

// ---------------------------------------------------------------------------
// Owners and states.

export const ownerKinds: OwnerKind[] = ['human', 'claude', 'codex', 'agent', 'ci']
export const ownerKindLabel: Record<OwnerKind, string> = {
  human: 'Person at a terminal',
  claude: 'Claude Code',
  codex: 'Codex',
  agent: 'Other agent or IDE',
  ci: 'CI',
}

export function OwnerChip({ owner }: { owner: Owner }) {
  return (
    <span class="owner" title={`${ownerKindLabel[owner.kind] ?? owner.kind}: ${owner.name}`}>
      <span class={`swatch own-${owner.kind}`} aria-hidden="true" />
      <span class="owner-name">{owner.name || owner.id}</span>
    </span>
  )
}

export const stateLabel: Record<JobState, string> = {
  queued: 'Waiting',
  syncing: 'Syncing',
  setup: 'Setting up',
  running: 'Running',
  succeeded: 'Succeeded',
  failed: 'Failed',
  canceled: 'Canceled',
  infra_error: 'kran error',
  lost: 'Lost',
}

export function StateBadge({ job }: { job: Job }) {
  const s = job.state
  let label = stateLabel[s]
  if (s === 'failed' && job.exit_code != null) label = `Exit ${job.exit_code}`
  if (job.signal && (s === 'failed' || s === 'canceled')) label += ` (${job.signal})`
  return (
    <span class={`state st-${s}`}>
      <Icon name={s} />
      <span>{label}</span>
    </span>
  )
}

// ---------------------------------------------------------------------------
// Toasts: polite announcements for action results.

interface Toast {
  id: number
  text: string
  hint?: string
  bad?: boolean
}
let toasts: Toast[] = []
let toastSeq = 0
const toastListeners = new Set<(t: Toast[]) => void>()

export function toast(text: string, opts: { hint?: string; bad?: boolean } = {}) {
  const t = { id: ++toastSeq, text, ...opts }
  toasts = [...toasts, t].slice(-3)
  toastListeners.forEach(fn => fn(toasts))
  window.setTimeout(
    () => {
      toasts = toasts.filter(x => x.id !== t.id)
      toastListeners.forEach(fn => fn(toasts))
    },
    opts.bad ? 9000 : 4000,
  )
}

export function Toasts() {
  const [list, set] = useState(toasts)
  useEffect(() => {
    toastListeners.add(set)
    return () => void toastListeners.delete(set)
  }, [])
  return (
    <div class="toasts" role="status" aria-live="polite">
      {list.map(t => (
        <div key={t.id} class={`toast${t.bad ? ' bad' : ''}`}>
          <p>{t.text}</p>
          {t.hint && <p class="toast-hint">{t.hint}</p>}
        </div>
      ))}
    </div>
  )
}

// ---------------------------------------------------------------------------
// Confirmation: a native modal dialog (focus trap and Escape for free).

interface Ask {
  title: string
  body: ComponentChildren
  action: string
  resolve: (ok: boolean) => void
}
let ask: ((a: Ask) => void) | null = null

export function confirmAction(title: string, body: ComponentChildren, action: string): Promise<boolean> {
  return new Promise(resolve => {
    if (!ask) return resolve(window.confirm(title))
    ask({ title, body, action, resolve })
  })
}

export function ConfirmHost() {
  const ref = useRef<HTMLDialogElement>(null)
  const [cur, setCur] = useState<Ask | null>(null)
  useEffect(() => {
    ask = a => setCur(a)
    return () => {
      ask = null
    }
  }, [])
  useEffect(() => {
    const d = ref.current
    if (cur && d && !d.open) d.showModal()
  }, [cur])
  const done = (ok: boolean) => {
    ref.current?.close()
    cur?.resolve(ok)
    setCur(null)
  }
  return (
    <dialog ref={ref} class="confirm" aria-labelledby="confirm-title" onCancel={e => (e.preventDefault(), done(false))}>
      {cur && (
        <form method="dialog" onSubmit={e => (e.preventDefault(), done(true))}>
          <h2 id="confirm-title">{cur.title}</h2>
          <div class="confirm-body">{cur.body}</div>
          <div class="confirm-actions">
            <button type="button" class="btn" onClick={() => done(false)} autofocus>
              Keep it
            </button>
            <button type="submit" class="btn btn-danger">
              {cur.action}
            </button>
          </div>
        </form>
      )}
    </dialog>
  )
}

// ---------------------------------------------------------------------------
// Copyable command, used by the empty states that teach.

export function CopyCommand({ cmd }: { cmd: string }) {
  const [copied, setCopied] = useState(false)
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(cmd)
      setCopied(true)
      window.setTimeout(() => setCopied(false), 1600)
    } catch {
      /* clipboard blocked (http on a non-loopback host); the text is selectable */
    }
  }
  return (
    <span class="copycmd">
      <code>{cmd}</code>
      <button type="button" class="btn btn-quiet btn-icon" onClick={copy} aria-label={copied ? 'Copied' : `Copy ${cmd}`}>
        <Icon name={copied ? 'check' : 'copy'} />
      </button>
    </span>
  )
}
