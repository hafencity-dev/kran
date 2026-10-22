import { render } from 'preact'
import { useCallback, useEffect, useState } from 'preact/hooks'
import { initToken } from './api'
import { ConfirmHost, NowProvider, Toasts } from './components/bits'
import { Banner, Header, Today } from './components/Header'
import { History } from './components/History'
import { JobDrawer } from './components/JobDrawer'
import { Running, Waiting } from './components/Jobs'
import { Machines } from './components/Machines'
import { store, useSnapshot } from './store'
import './styles.css'

// Deep link: #job=<id> opens that job's details.
function readJobHash(): string | null {
  return new URLSearchParams(location.hash.replace(/^#/, '')).get('job')
}

function App() {
  const snap = useSnapshot()
  const [open, setOpen] = useState<string | null>(readJobHash)

  useEffect(() => {
    const onHash = () => setOpen(readJobHash())
    window.addEventListener('hashchange', onHash)
    return () => window.removeEventListener('hashchange', onHash)
  }, [])

  const openJob = useCallback((id: string) => {
    history.pushState(null, '', `#job=${encodeURIComponent(id)}`)
    setOpen(id)
  }, [])
  const closeJob = useCallback(() => {
    if (readJobHash()) history.replaceState(null, '', location.pathname + location.search)
    setOpen(null)
  }, [])

  const dim = snap.conn === 'down' || snap.conn === 'stale' || snap.conn === 'unauthorized'
  return (
    <>
      <a class="skip" href="#running">
        Skip to running jobs
      </a>
      <Header snap={snap} />
      <Banner snap={snap} />
      <main class={dim ? 'is-stale' : undefined} aria-busy={!snap.loaded}>
        {!snap.loaded ? (
          snap.conn === 'connecting' ? <p class="loading">Connecting to the kran daemon.</p> : null
        ) : (
          <>
            <Today snap={snap} />
            <Machines snap={snap} onOpenJob={openJob} />
            <Running jobs={snap.placed} onOpen={openJob} />
            <Waiting jobs={snap.queued} onOpen={openJob} />
            <History jobs={snap.finished} nodes={snap.nodes} onOpen={openJob} />
          </>
        )}
      </main>
      <footer class="foot">
        {snap.version && (
          <p class="muted">
            kran {snap.version.version}, API {snap.version.api}
          </p>
        )}
      </footer>
      <JobDrawer id={open} job={open ? snap.jobs.get(open) : undefined} onClose={closeJob} />
      <ConfirmHost />
      <Toasts />
    </>
  )
}

initToken()
store.start()
render(
  <NowProvider>
    <App />
  </NowProvider>,
  document.getElementById('app')!,
)
