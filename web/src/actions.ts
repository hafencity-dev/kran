import { createElement as h } from 'preact'
import { api, ApiError, DownError } from './api'
import { confirmAction, toast } from './components/bits'
import { command } from './format'
import type { Job, NodeState, Priority } from './types'

function report(e: unknown, what: string) {
  if (e instanceof ApiError) {
    if (e.status === 401 || e.status === 403)
      toast(`Could not ${what}: this dashboard has no token for changes.`, { bad: true, hint: 'Open the link from kran ui --print-url on the node.' })
    else toast(`Could not ${what}: ${e.message}`, { bad: true, hint: e.hint })
  } else if (e instanceof DownError) {
    toast(`Could not ${what}: the kran daemon is not answering.`, { bad: true, hint: 'Check it with kran service status.' })
  } else toast(`Could not ${what}: ${String(e)}`, { bad: true })
}

export async function cancelJob(job: Job) {
  const cmd = command(job.spec.argv, job.spec.shell)
  const running = job.state !== 'queued'
  const ok = await confirmAction(
    running ? 'Cancel this job?' : 'Cancel this waiting job?',
    h(
      'div',
      null,
      h('p', null, h('code', null, cmd)),
      h(
        'p',
        null,
        running
          ? `It is ${job.state} on ${job.node ?? 'a node'} for ${job.spec.owner.name}. It is stopped and its kran run exits as interrupted.`
          : `${job.spec.owner.name} is waiting for it. Its kran run exits as canceled.`,
      ),
    ),
    'Cancel job',
  )
  if (!ok) return
  try {
    await api.cancel(job.spec.id)
    toast(`Canceled ${cmd}`)
  } catch (e) {
    report(e, 'cancel the job')
  }
}

export async function setPriority(job: Job, p: Priority) {
  if (p === job.spec.priority) return
  try {
    await api.priority(job.spec.id, p)
    toast(`Priority set to ${p} for ${command(job.spec.argv, job.spec.shell)}`)
  } catch (e) {
    report(e, 'change the priority')
  }
}

export async function drainNode(n: NodeState) {
  const name = n.info.name
  const ok = await confirmAction(
    `Drain ${name}?`,
    h(
      'p',
      null,
      n.running > 0
        ? `${name} finishes its ${n.running} running ${n.running === 1 ? 'job' : 'jobs'} and takes no new ones until you resume it. Waiting jobs go elsewhere or wait.`
        : `${name} takes no new jobs until you resume it. Waiting jobs go elsewhere or wait.`,
    ),
    `Drain ${name}`,
  )
  if (!ok) return
  try {
    await api.drain(name)
    toast(`${name} is draining`)
  } catch (e) {
    report(e, `drain ${name}`)
  }
}

export async function resumeNode(n: NodeState) {
  try {
    await api.resume(n.info.name)
    toast(`${n.info.name} takes jobs again`)
  } catch (e) {
    report(e, `resume ${n.info.name}`)
  }
}
