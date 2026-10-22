import type { APIError, Job, Priority, State } from './types'

const TOKEN_KEY = 'kran.token'
let token: string | null = null

/**
 * Reads `#token=` from the URL fragment (SPEC §9), keeps it for this tab in
 * sessionStorage and removes it from the address bar so it does not end up
 * in screenshots, history sync or a copied link.
 */
export function initToken(): void {
  const hash = location.hash.replace(/^#/, '')
  const params = new URLSearchParams(hash)
  const fromHash = params.get('token')
  if (fromHash) {
    token = fromHash
    try {
      sessionStorage.setItem(TOKEN_KEY, fromHash)
    } catch {
      /* storage blocked: keep it in memory for this page */
    }
    params.delete('token')
    const rest = params.toString()
    history.replaceState(history.state, '', location.pathname + location.search + (rest ? `#${rest}` : ''))
    return
  }
  try {
    token = sessionStorage.getItem(TOKEN_KEY)
  } catch {
    token = null
  }
}

export const hasToken = () => token != null

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
    readonly hint?: string,
  ) {
    super(message)
  }
}

/** Network-level failure: the daemon is not answering at all. */
export class DownError extends Error {}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers)
  if (token) headers.set('X-Kran-Token', token)
  let res: Response
  try {
    res = await fetch(path, { ...init, headers, cache: 'no-store' })
  } catch (e) {
    throw new DownError(String(e))
  }
  if (!res.ok) {
    let body: APIError | null = null
    try {
      body = (await res.json()) as APIError
    } catch {
      /* not JSON: a proxy or an old daemon */
    }
    if (res.status === 502 || res.status === 503 || res.status === 504) throw new DownError(`HTTP ${res.status}`)
    throw new ApiError(res.status, body?.error ?? `${res.status} ${res.statusText}`, body?.hint)
  }
  if (res.status === 204) return undefined as T
  const type = res.headers.get('content-type') ?? ''
  if (type.includes('application/json')) return (await res.json()) as T
  return (await res.text()) as T
}

const post = (path: string, body?: unknown) =>
  request<void>(path, {
    method: 'POST',
    headers: body === undefined ? {} : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })

const enc = encodeURIComponent

export const api = {
  state: () => request<State>('/api/v1/state'),
  version: () => request<unknown>('/api/v1/version'),
  jobs: (q: { limit?: number; owner?: string; node?: string }) => {
    const p = new URLSearchParams()
    p.set('limit', String(q.limit ?? 200))
    if (q.owner) p.set('owner', q.owner)
    if (q.node) p.set('node', q.node)
    return request<Job[] | null>(`/api/v1/jobs?${p}`)
  },
  job: (id: string) => request<Job>(`/api/v1/jobs/${enc(id)}`),
  logText: (id: string, offset = 0) => request<string>(`/api/v1/jobs/${enc(id)}/log?offset=${offset}`),
  cancel: (id: string) => post(`/api/v1/jobs/${enc(id)}/cancel`),
  priority: (id: string, priority: Priority) => post(`/api/v1/jobs/${enc(id)}/priority`, { priority }),
  drain: (node: string) => post(`/api/v1/nodes/${enc(node)}/drain`),
  resume: (node: string) => post(`/api/v1/nodes/${enc(node)}/resume`),
}

/** URL for an EventSource; EventSource cannot send headers, so the token rides in the query. */
export function sseURL(path: string): string {
  if (!token) return path
  return path + (path.includes('?') ? '&' : '?') + `token=${enc(token)}`
}
