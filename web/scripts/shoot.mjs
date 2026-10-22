// Screenshots of the dashboard against the mock daemon, for design review.
//
//   go run ./internal/ui/mockserver &      # serves API + built UI on :7271
//   node web/scripts/shoot.mjs [base] [outdir]
//
// Uses playwright-core with an already installed Chromium (no download).
import { chromium } from 'playwright-core'
import { mkdirSync } from 'node:fs'
import { join } from 'node:path'

const base = process.argv[2] ?? 'http://127.0.0.1:7271/'
const out = process.argv[3] ?? new URL('../screenshots/', import.meta.url).pathname
const only = process.env.ONLY ? new Set(process.env.ONLY.split(',')) : null
mkdirSync(out, { recursive: true })

const sizes = {
  desktop: { width: 1440, height: 1000 },
  phone: { width: 390, height: 844 },
}

const emptyState = {
  version: { version: '0.4.0', commit: 'x', build_date: '', api: 'v1', protocol: '1.0' },
  self: 'gx10',
  nodes: [],
  jobs: [],
  queue: [],
  owners: [],
  stats: { jobs_today: 0, offloaded_today: 0, fallbacks_today: 0, failed_infra_today: 0, saved_seconds_today: 0, cpu_seconds_by_node: {} },
}

const browser = await chromium.launch()
const errors = []

async function page(size, scheme) {
  const ctx = await browser.newContext({ viewport: sizes[size], colorScheme: scheme, deviceScaleFactor: size === 'phone' ? 2 : 1, reducedMotion: 'reduce' })
  const p = await ctx.newPage()
  p.on('pageerror', e => errors.push(`${size}/${scheme}: ${e.message}`))
  p.on('console', m => m.type() === 'error' && !m.text().startsWith('Failed to load resource') && errors.push(`${size}/${scheme} console: ${m.text()}`))
  return { ctx, p }
}

async function shot(name, size, scheme, fn, full = true) {
  if (only && !only.has(name)) return
  const { ctx, p } = await page(size, scheme)
  try {
    await fn(p)
    const file = join(out, `${name}.png`)
    await p.screenshot({ path: file, fullPage: full })
    console.log(file)
  } finally {
    await ctx.close()
  }
}

const settle = async p => {
  await p.goto(base)
  await p.waitForSelector('.berth')
  // Sparklines are client-side history; give them some samples.
  await p.waitForTimeout(Number(process.env.SETTLE ?? 2500))
  await assertRowsVisible(p)
}

// Every list the page claims to have must actually be painted: a row that
// exists in the DOM but is skipped (content-visibility), hidden, transparent
// or empty is a bug, even if nothing throws. Counts must match the headers.
async function assertRowsVisible(p) {
  const problems = await p.evaluate(() => {
    const out = []
    const count = id => Number(document.querySelector(`#${id} .count`)?.textContent ?? 'NaN')
    const check = (sel, want) => {
      const els = [...document.querySelectorAll(sel)]
      if (want != null && els.length !== want) out.push(`${sel}: ${els.length} rows rendered, header says ${want}`)
      els.forEach((el, i) => {
        const r = el.getBoundingClientRect()
        // content-visibility skips an element's contents, not the element:
        // test the row and the text inside it.
        const inner = el.querySelector('code, .node-name, .state') ?? el.firstElementChild ?? el
        const opts = { contentVisibilityAuto: true, opacityProperty: true, visibilityProperty: true }
        const painted = el.checkVisibility(opts) && inner.checkVisibility(opts)
        if (!painted || r.height < 12 || r.width < 60 || !el.innerText.trim()) out.push(`${sel} #${i + 1} is not visible (${Math.round(r.width)}x${Math.round(r.height)}, painted=${painted})`)
      })
    }
    check('.berth', count('machines-h') || document.querySelectorAll('.berth').length)
    check('.running tbody tr', count('running-h'))
    check('.qitem', Math.min(count('waiting-h'), 40))
    check('.history tbody tr', null)
    if (count('waiting-h') > 0 && !document.querySelector('.qreason')?.innerText.includes('waiting')) out.push('queue rows show no wait reason')
    return out
  })
  if (problems.length) throw new Error('rows not visible:\n' + problems.join('\n'))
}

await Promise.all(
  ['desktop', 'phone'].flatMap(size => ['light', 'dark'].map(scheme => shot(`${size}-${scheme}`, size, scheme, settle))),
)

await shot('desktop-drawer-dark', 'desktop', 'dark', async p => {
  await settle(p)
  await p.locator('.running .cmd-link').first().click()
  await p.waitForSelector('.log-body .ll')
  await p.waitForTimeout(1500)
}, false)

await shot('phone-drawer-light', 'phone', 'light', async p => {
  await settle(p)
  await p.locator('.history .cmd-link').first().click()
  await p.waitForSelector('.log-body .ll')
  await p.waitForTimeout(800)
}, false)

await shot('desktop-tooltip-light', 'desktop', 'light', async p => {
  await settle(p)
  await p.locator('.strip-cpu .blk').first().hover()
  await p.waitForTimeout(300)
}, false)

await shot('desktop-confirm-light', 'desktop', 'light', async p => {
  await settle(p)
  await p.locator('.berth .btn').nth(1).click()
  await p.waitForSelector('dialog.confirm[open]')
}, false)

// Daemon dies while the page is open: run a private mock and kill it.
if (process.env.MOCK)
  await shot('desktop-down-light', 'desktop', 'light', async p => {
    const { spawn } = await import('node:child_process')
    const proc = spawn(process.env.MOCK, ['-addr', '127.0.0.1:7279'], { stdio: 'ignore' })
    try {
      await new Promise(r => setTimeout(r, 1500))
      await p.goto('http://127.0.0.1:7279/')
      await p.waitForSelector('.berth')
      await p.waitForTimeout(2500)
    } finally {
      proc.kill('SIGKILL')
    }
    await p.waitForSelector('.banner', { timeout: 15000 })
    await p.waitForTimeout(1200)
  }, false)

await shot('desktop-empty-dark', 'desktop', 'dark', async p => {
  const one = async () => {
    const r = await fetch(base + 'api/v1/state')
    const st = await r.json()
    return { ...emptyState, nodes: [st.nodes[0]] }
  }
  const st = await one()
  await p.route('**/api/v1/state', r => r.fulfill({ json: st }))
  await p.route('**/api/v1/jobs?*', r => r.fulfill({ json: [] }))
  await p.route('**/api/v1/events*', r =>
    r.fulfill({ status: 200, headers: { 'content-type': 'text/event-stream' }, body: `data: ${JSON.stringify({ type: 'state', seq: 1, time: new Date().toISOString(), data: st })}\n\n` }),
  )
  await p.goto(base)
  await p.waitForSelector('.berth')
  await p.waitForTimeout(800)
})

await browser.close()
if (errors.length) {
  console.error('page errors:\n' + errors.join('\n'))
  process.exitCode = 1
}
