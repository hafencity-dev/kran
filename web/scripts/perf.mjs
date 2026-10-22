// Load check: run the mock with many nodes and jobs, then measure how the
// page keeps up. Usage:
//   go run ./internal/ui/mockserver -addr 127.0.0.1:7272 -nodes 10 -jobs 200 &
//   node web/scripts/perf.mjs http://127.0.0.1:7272/
import { chromium } from 'playwright-core'

const base = process.argv[2] ?? 'http://127.0.0.1:7272/'
const seconds = Number(process.env.SECONDS ?? 20)
const browser = await chromium.launch()
const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } })
await page.addInitScript(() => {
  window.__events = 0
  const ES = window.EventSource
  window.EventSource = class extends ES {
    constructor(url, init) {
      super(url, init)
      if (String(url).includes('/api/v1/events')) this.addEventListener('message', () => window.__events++)
    }
  }
  window.__long = []
  new PerformanceObserver(l => l.getEntries().forEach(e => window.__long.push(e.duration))).observe({ type: 'longtask', buffered: true })
})
await page.goto(base)
await page.waitForSelector('.berth')
await page.waitForTimeout(3000)
const r = await page.evaluate(async secs => {
  window.__long.length = 0
  const ev0 = window.__events
  let frames = 0
  let worst = 0
  let last = performance.now()
  let run = true
  const tick = t => {
    frames++
    worst = Math.max(worst, t - last)
    last = t
    if (run) requestAnimationFrame(tick)
  }
  requestAnimationFrame(tick)
  await new Promise(res => setTimeout(res, secs * 1000))
  run = false
  return {
    eventsPerSec: (window.__events - ev0) / secs,
    fps: frames / secs,
    worstFrameMs: Math.round(worst),
    longTasks: window.__long.length,
    longTaskMs: Math.round(window.__long.reduce((a, b) => a + b, 0)),
    domNodes: document.getElementsByTagName('*').length,
    nodes: document.querySelectorAll('.berth').length,
    running: document.querySelectorAll('.running tbody tr').length,
    waiting: document.querySelectorAll('.qitem').length,
    heapMB: performance.memory ? Math.round(performance.memory.usedJSHeapSize / 1e6) : null,
  }
}, seconds)
console.log(JSON.stringify(r, null, 2))
await browser.close()
