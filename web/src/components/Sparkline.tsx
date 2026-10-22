import { useState } from 'preact/hooks'
import type { Sample } from '../store'

const W = 160
const H = 36
const MAX_WINDOW = 10 * 60 * 1000
const MIN_WINDOW = 3 * 60 * 1000

interface Props {
  samples: readonly Sample[]
  pick: (s: Sample) => number
  max: number
  label: string
  format: (v: number) => string
}

/**
 * Up to ten minutes of one measure. The value axis is fixed (0..max) so
 * shapes are comparable between machines; the current value is always
 * printed next to it, the line only adds the trend.
 */
export function Sparkline({ samples, pick, max, label, format }: Props) {
  const [hover, setHover] = useState<number | null>(null)
  if (samples.length < 2 || max <= 0) {
    return (
      <svg class="spark" viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" aria-hidden="true">
        <line x1="0" x2={W} y1={H - 0.5} y2={H - 0.5} class="spark-base" />
      </svg>
    )
  }
  const end = samples[samples.length - 1].t
  // Grows from 3 to 10 minutes as history accumulates, so a freshly
  // opened page shows a readable line instead of a sliver at the edge.
  const WINDOW = Math.min(MAX_WINDOW, Math.max(MIN_WINDOW, end - samples[0].t))
  const start = end - WINDOW
  const x = (t: number) => ((t - start) / WINDOW) * W
  const y = (v: number) => H - 2 - Math.max(0, Math.min(1, v / max)) * (H - 4)
  let d = ''
  for (let i = 0; i < samples.length; i++) {
    const s = samples[i]
    d += `${i ? 'L' : 'M'}${x(s.t).toFixed(1)} ${y(pick(s)).toFixed(1)}`
  }
  const area = `${d}L${x(end).toFixed(1)} ${H}L${x(samples[0].t).toFixed(1)} ${H}Z`
  let lo = Infinity
  let hi = -Infinity
  for (const s of samples) {
    const v = pick(s)
    if (v < lo) lo = v
    if (v > hi) hi = v
  }
  const h = hover != null ? samples[hover] : null
  const onMove = (e: PointerEvent) => {
    const r = (e.currentTarget as SVGElement).getBoundingClientRect()
    const t = start + ((e.clientX - r.left) / r.width) * WINDOW
    let best = 0
    for (let i = 1; i < samples.length; i++) if (Math.abs(samples[i].t - t) < Math.abs(samples[best].t - t)) best = i
    setHover(best)
  }
  const last = samples[samples.length - 1]
  return (
    <div class="spark-wrap">
      <svg class="spark" viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" aria-hidden="true" onPointerMove={onMove} onPointerLeave={() => setHover(null)}>
        <line x1="0" x2={W} y1={H - 0.5} y2={H - 0.5} class="spark-base" />
        <path d={area} class="spark-area" />
        <path d={d} class="spark-line" vector-effect="non-scaling-stroke" />
        {h && <line x1={x(h.t)} x2={x(h.t)} y1="0" y2={H} class="spark-cross" vector-effect="non-scaling-stroke" />}
      </svg>
      {h ? (
        <span class="spark-tip" style={{ left: `${(x(h.t) / W) * 100}%` }}>
          {format(pick(h))}, {Math.round((last.t - h.t) / 1000)} s ago
        </span>
      ) : null}
      <span class="sr-only">
        {label} over the last {Math.round((end - samples[0].t) / 60000) || 1} minutes: lowest {format(lo)}, highest {format(hi)}.
      </span>
    </div>
  )
}
