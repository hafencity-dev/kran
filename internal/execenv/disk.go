package execenv

import (
	"golang.org/x/sys/unix"

	"github.com/hafencity-dev/kran/internal/proto"
)

// diskSpace returns free (for unprivileged users) and total bytes of the
// filesystem holding path.
func diskSpace(path string) (free, total int64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), int64(st.Blocks) * int64(st.Bsize), nil
}

// diskFloor is SPEC §6.2's default: 5% of the disk, 10-20 GiB.
func diskFloor(total int64) int64 { return proto.DiskFloor(total) }

// diskReserve is the free space kran keeps on a disk it fills (caches,
// slots) by trimming its own data: max(25 GiB, 20%), at most 50 GiB (a
// fifth of a big disk shared with the owner's data would be too much),
// never below the floor.
func diskReserve(total int64) int64 {
	return max(min(max(25*gib, total/5), 50*gib), diskFloor(total))
}
