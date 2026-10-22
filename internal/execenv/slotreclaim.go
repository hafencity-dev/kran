package execenv

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// Slots under disk pressure (SPEC §6.2): idle slots are kran's to delete
// (a sync recreates them), so when a disk kran fills keeps less free than
// its reserve, the reaper deletes idle slots there, least recently used
// first, until the reserve is back. A slot in use is never touched; the
// 3-day TTL (gcSlots) still applies on top.

// slotReclaimMax bounds the slots one pass deletes.
const slotReclaimMax = 8

// volumeDiskEvery spaces the Docker disk checks of the volume layout
// (each is a docker exec).
const volumeDiskEvery = 5 * time.Minute

// reserveFor is the free space kran keeps on a disk of total bytes.
func (m *manager) reserveFor(total int64) int64 {
	if m.opts.DiskReserve > 0 {
		return m.opts.DiskReserve
	}
	return diskReserve(total)
}

// reclaimHostSlots: slots on this machine's disk (native slots, and
// container slots on Linux).
func (m *manager) reclaimHostSlots(ctx context.Context) {
	m.reclaimSlots(ctx, locHost, "the disk", func() (int64, int64, error) { return diskSpace(m.l.slotsDir()) })
}

// reclaimVolumeSlots: slots in the kran-slots volume, on the Docker disk
// (macOS). It measures that disk through the sync helper, at most every
// volumeDiskEvery unless now.
func (m *manager) reclaimVolumeSlots(ctx context.Context, now bool) {
	if !m.useVolume() {
		return
	}
	dk, _ := m.docker()
	if dk == nil {
		return
	}
	m.ct.mu.Lock()
	due := now || m.now().Sub(m.ct.slotCheck) >= volumeDiskEvery
	if due {
		m.ct.slotCheck = m.now()
	}
	m.ct.mu.Unlock()
	if !due {
		return
	}
	m.reclaimSlots(ctx, locVolume, "the Docker disk", func() (int64, int64, error) {
		free, total, err := m.volumeDiskSpace(ctx, dk)
		if err == nil {
			m.setDockerDisk(free, total)
		}
		return free, total, err
	})
}

// volumeDiskSpace asks the running sync helper how full the Docker disk
// is (it mounts kran-slots). An error when it does not run: then no slot
// was synced since Docker started, and nothing grew.
func (m *manager) volumeDiskSpace(ctx context.Context, dk *dockerEnv) (free, total int64, err error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := dk.run(ctx, nil, "exec", m.syncName(), "df", "-B1", "--output=avail,size", ctrSlots)
	if err != nil {
		return 0, 0, err
	}
	return parseDF(string(out))
}

// parseDF reads `df -B1 --output=avail,size` (a header, then one line).
func parseDF(out string) (free, total int64, err error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	f := strings.Fields(lines[len(lines)-1])
	if len(f) != 2 {
		return 0, 0, strconv.ErrSyntax
	}
	if free, err = strconv.ParseInt(f[0], 10, 64); err != nil {
		return 0, 0, err
	}
	if total, err = strconv.ParseInt(f[1], 10, 64); err != nil {
		return 0, 0, err
	}
	return free, total, nil
}

func (m *manager) reclaimSlots(ctx context.Context, loc slotLoc, disk string, space func() (int64, int64, error)) {
	free, total, err := space()
	if err != nil || total <= 0 {
		return
	}
	reserve := m.reserveFor(total)
	for i := 0; i < slotReclaimMax && free < reserve && ctx.Err() == nil; i++ {
		s := m.pool.evictLRU(loc)
		if s == nil {
			m.logf("disk: %s on %s has %s free, below the %s kran keeps free, and no idle slot is left to remove", disk, m.opts.NodeName, fmtGiB(free), fmtGiB(reserve))
			return
		}
		m.logf("disk: removing idle slot %s/%d (last used %s): %s has %s free, kran keeps %s free",
			short(s.repo), s.index, m.slotLastUsed(s).Format(time.DateTime), disk, fmtGiB(free), fmtGiB(reserve))
		if err := m.removeSlot(ctx, s); err != nil {
			m.logf("disk: removing slot %s/%d: %v", short(s.repo), s.index, err)
			return
		}
		if free, total, err = space(); err != nil {
			return
		}
	}
}

func (m *manager) slotLastUsed(s *slot) time.Time {
	m.pool.mu.Lock()
	defer m.pool.mu.Unlock()
	return m.pool.lastUsed(s)
}

// setDockerDisk records a measurement of the Docker disk (the disk floor
// of container jobs reads it).
func (m *manager) setDockerDisk(free, total int64) {
	m.ct.mu.Lock()
	m.ct.diskFree, m.ct.diskTotal, m.ct.diskAt = free, total, m.now()
	m.ct.mu.Unlock()
}
