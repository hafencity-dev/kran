# Follow-ups

Open items that are agreed but not built yet. Each one says what was seen,
what is wanted, and when it may be built.

## Placement is re-evaluated while a job waits; a failed start only backs a node off

**Seen (09.11.2026, kran 0.1.10, 5b15c77):** the lokalaise land gate's docs job
`0hefb0119h` (`lk docs check`, delegated, estimate 12.4 GiB) tried devmac once
while devmac had 7.2 GiB free. The start failed. From then on the job showed
`queued devmac … devmac: failed to start this job (… has 21.6 CPUs and 7.2 GiB
free); next in line on gx10` and waited for gx10, which had only 4.5 GiB free.
devmac later had 19.7 GiB free and no jobs, but the job stayed queued for more
than 1 h 48 min. The gate batch it belonged to stalled with it. `kran status`
summarised it as "ran here after a remote attempt failed".

**Wanted:**
- While a job waits, its placement is evaluated again on every scheduler tick.
  The job takes any node that now has room, by the same rules as a new job.
- A failed start on a node excludes that node for this job only for a bounded
  backoff (for example 1, 2 and 4 minutes, capped), not for the rest of the
  job's life.
- The waiting reason names the backoff and when the node is tried again.
- A test covers the shape above: a node fails a start, later has room, and the
  waiting job runs there.

**When:** build after the lokalaise land batch 1101 has finished. Rolling the
update out to gx10 and devmac is its own small step, released by the lokalaise
release session.
