import { store, type Snapshot } from '../store'
import { clock, duration } from '../format'
import { CopyCommand, useNow } from './bits'

export function Header({ snap }: { snap: Snapshot }) {
  const nav: [string, string, number | null][] = [
    ['machines', 'Machines', snap.nodes.length],
    ['running', 'Running', snap.placed.length],
    ['waiting', 'Waiting', snap.queued.length],
    ['history', 'History', null],
  ]
  return (
    <header class="top">
      <div class="top-inner">
        <a class="brand" href="#machines" aria-label={`kran on ${snap.self || 'this machine'}`}>
          <svg class="mark" viewBox="0 0 32 32" aria-hidden="true">
            <path d="M5 27V5h22v4H9.5v18z" />
            <rect x="16" y="14" width="11" height="13" rx="1.5" />
          </svg>
          <span class="brand-word">kran</span>
          {snap.self && <span class="brand-node">{snap.self}</span>}
        </a>
        <nav class="jump" aria-label="Sections">
          {nav.map(([id, label, n]) => (
            <a key={id} href={`#${id}`}>
              {label}
              {n != null && <span class="num count">{n}</span>}
            </a>
          ))}
        </nav>
        <ConnStatus snap={snap} />
      </div>
    </header>
  )
}

function ConnStatus({ snap }: { snap: Snapshot }) {
  const now = useNow()
  const since = snap.lastEventAt ? Math.round((now - snap.lastEventAt) / 1000) : 0
  let text = 'Live'
  let cls = 'live'
  switch (snap.conn) {
    case 'connecting':
      text = 'Connecting'
      cls = 'wait'
      break
    case 'reconnecting':
      text = 'Reconnecting'
      cls = 'wait'
      break
    case 'stale':
      text = `No updates for ${duration(since)}`
      cls = 'warn'
      break
    case 'down':
      text = 'Daemon not answering'
      cls = 'bad'
      break
    case 'unauthorized':
      text = 'Needs a token'
      cls = 'bad'
      break
  }
  return (
    <p class={`conn conn-${cls}`} role="status">
      <span class="conn-dot" aria-hidden="true" />
      {text}
    </p>
  )
}

/** Explains a broken connection and what to do; the last data stays visible but dimmed. */
export function Banner({ snap }: { snap: Snapshot }) {
  const now = useNow()
  if (snap.conn === 'unauthorized') {
    return (
      <div class="banner banner-bad" role="alert">
        <p>
          <strong>This dashboard needs a token.</strong> The daemon only shows its data to links that carry one. On {snap.self || 'the machine running kran'}, run
        </p>
        <CopyCommand cmd="kran ui --print-url" />
        <p>and open the link it prints.{snap.connDetail ? ` (${snap.connDetail})` : ''}</p>
      </div>
    )
  }
  if (snap.conn !== 'down') return null
  const wait = Math.max(0, Math.ceil((snap.retryAt - now) / 1000))
  return (
    <div class="banner banner-bad" role="alert">
      <p>
        <strong>The kran daemon is not answering.</strong>{' '}
        {snap.loaded ? `What you see is from ${clock(new Date(snap.lastEventAt).toISOString())} and may be out of date. ` : ''}
        Jobs still run locally without it. Start it with
      </p>
      <CopyCommand cmd="kran service start" />
      <p class="banner-retry">
        {snap.connDetail && <span>{snap.connDetail}. </span>}
        {wait > 0 ? `Trying again in ${wait} s.` : 'Trying again.'}{' '}
        <button type="button" class="btn btn-small" onClick={() => store.retryNow()}>
          Try now
        </button>
      </p>
    </div>
  )
}

export function Today({ snap }: { snap: Snapshot }) {
  const st = snap.stats
  const now = useNow()
  if (!st) return null
  const byNode = Object.entries(st.cpu_seconds_by_node ?? {})
    .filter(([, v]) => v > 0)
    .sort((a, b) => b[1] - a[1])
  const top = byNode[0]?.[1] ?? 0
  const self = snap.self || 'this machine'
  const extras: string[] = []
  if (st.fallbacks_today) extras.push(`${st.fallbacks_today} fell back to running here when the other machine failed`)
  if (st.failed_infra_today) extras.push(`${st.failed_infra_today} ended because kran lost track of ${st.failed_infra_today === 1 ? 'it' : 'them'}`)
  return (
    <section class="today" aria-label="Today">
      <div class="today-text">
        <p class="lede">
          {st.jobs_today === 0 ? (
            'No jobs have gone through kran today.'
          ) : (
            <>
              Today kran ran <span class="num">{st.jobs_today}</span> jobs and moved <span class="num">{st.offloaded_today}</span> of them off {self}
              {st.saved_seconds_today > 0 ? (
                <>
                  , saving about <span class="num">{duration(st.saved_seconds_today)}</span> of waiting here
                </>
              ) : null}
              .
            </>
          )}
        </p>
        {extras.length > 0 && <p class="lede-extra">{extras.join('. ')}.</p>}
        {snap.activity.length > 0 && (
          <ul class="activity" aria-label="Recent mesh events">
            {snap.activity.slice(0, 3).map(a => (
              <li key={`${a.time}-${a.msg}`} class={`act-${a.level}`}>
                <span class="num muted">{clock(new Date(a.time).toISOString())}</span> {a.msg}
                <span class="sr-only">, {duration((now - a.time) / 1000)} ago</span>
              </li>
            ))}
          </ul>
        )}
      </div>
      {byNode.length > 0 && (
        <figure class="cpu-by-node">
          <figcaption>CPU time used by kran jobs today</figcaption>
          <table class="hbars">
            <tbody>
              {byNode.map(([name, v]) => (
                <tr key={name}>
                  <th scope="row">{name}</th>
                  <td class="hbar-cell">
                    <span class="hbar" style={{ width: `${(v / top) * 100}%` }} />
                  </td>
                  <td class="r num">{duration(v)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </figure>
      )}
    </section>
  )
}
