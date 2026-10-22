// Humane units. Thin spaces between number and unit keep values together.

const GiB = 1024 ** 3
const MiB = 1024 ** 2

export function bytes(n: number | undefined | null): string {
  if (n == null || !isFinite(n)) return '–'
  if (n >= 1024 * GiB) return `${trim(n / (1024 * GiB), 1)} TiB`
  if (n >= GiB) return `${trim(n / GiB, n >= 10 * GiB ? 0 : 1)} GiB`
  if (n >= MiB) return `${trim(n / MiB, 0)} MiB`
  if (n >= 1024) return `${trim(n / 1024, 0)} KiB`
  return `${Math.round(n)} B`
}

/** Bare GiB number, for "41 / 128 GiB" pairs. */
export function gib(n: number, digits?: number): string {
  const v = n / GiB
  return trim(v, digits ?? (v >= 10 ? 0 : 1))
}

export function cores(n: number | undefined | null): string {
  if (n == null || !isFinite(n)) return '–'
  return trim(n, n >= 10 || Number.isInteger(n) ? 0 : 1)
}

function trim(v: number, digits: number): string {
  const s = v.toFixed(digits)
  return digits > 0 ? s.replace(/\.0+$/, '') : s
}

/** "3 m 12 s", "41 s", "1 h 04 m", "2 d 3 h". */
export function duration(sec: number | undefined | null): string {
  if (sec == null || !isFinite(sec) || sec < 0) return '–'
  if (sec < 10) return `${sec < 1 ? sec.toFixed(1) : Math.round(sec)} s`
  const s = Math.round(sec)
  if (s < 60) return `${s} s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m} m ${String(s % 60).padStart(2, '0')} s`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h} h ${String(m % 60).padStart(2, '0')} m`
  return `${Math.floor(h / 24)} d ${h % 24} h`
}

/** Coarse duration for estimates: "about 4 m". */
export function roughDuration(sec: number): string {
  if (sec < 60) return `${Math.max(1, Math.round(sec / 5) * 5)} s`
  if (sec < 3600) return `${Math.round(sec / 60)} m`
  return `${(sec / 3600).toFixed(1).replace(/\.0$/, '')} h`
}

export function ago(iso: string | undefined, now: number): string {
  if (!iso) return '–'
  const t = Date.parse(iso)
  if (!isFinite(t) || t <= 0) return '–'
  const s = Math.max(0, (now - t) / 1000)
  if (s < 5) return 'just now'
  return `${duration(s)} ago`
}

export function clock(iso: string | undefined): string {
  if (!iso) return '–'
  const d = new Date(iso)
  if (!isFinite(d.getTime()) || d.getFullYear() < 2000) return '–'
  return d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false })
}

export function rate(bps: number | undefined): string {
  if (!bps) return '–'
  if (bps >= 1e9) return `${trim(bps / 1e9, 1)} GB/s`
  if (bps >= 1e6) return `${trim(bps / 1e6, bps >= 1e8 ? 0 : 1)} MB/s`
  if (bps >= 1e3) return `${trim(bps / 1e3, 0)} kB/s`
  return `${Math.round(bps)} B/s`
}

export function ms(v: number | undefined): string {
  if (!v) return '–'
  return `${v < 10 ? v.toFixed(1) : Math.round(v)} ms`
}

export function pct(v: number | undefined | null): string {
  if (v == null || !isFinite(v)) return '–'
  return `${Math.round(v * 100)} %`
}

export function command(argv: string[] | undefined, shell?: string): string {
  if (shell) return shell
  if (!argv || argv.length === 0) return '(no command)'
  return argv.map(a => (/^[\w@%+=:,./-]+$/.test(a) ? a : `'${a.replace(/'/g, `'\\''`)}'`)).join(' ')
}

/** Short label for a job block: "go test", "vitest", "next build". */
export function shortCommand(argv: string[] | undefined, shell?: string): string {
  const a = shell ? shell.split(/\s+/) : argv ?? []
  const skip = new Set(['pnpm', 'npx', 'npm', 'yarn', 'bunx', 'uv', 'poetry', 'run', 'exec'])
  const words = a.filter((w, i) => !(skip.has(w) && i < 2) && !w.startsWith('-'))
  return words.slice(0, 2).join(' ') || a[0] || '?'
}

export function osLabel(os: string, arch: string): string {
  const o = os === 'darwin' ? 'macOS' : os === 'linux' ? 'Linux' : os
  return `${o} ${arch}`
}

export const time = (iso?: string) => (iso ? Date.parse(iso) : NaN)
export const validTime = (iso?: string) => {
  const t = time(iso)
  return isFinite(t) && t > 0 ? t : NaN
}
