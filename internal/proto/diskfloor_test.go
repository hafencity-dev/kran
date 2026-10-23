package proto

import "testing"

func TestDiskFloor(t *testing.T) {
	const gib = int64(1) << 30
	for _, c := range []struct{ total, want int64 }{
		{50 * gib, 10 * gib},  // small disk: the 10 GiB minimum
		{300 * gib, 15 * gib}, // 5%
		{916 * gib, 20 * gib}, // large disk: capped
		{4000 * gib, 20 * gib},
	} {
		if got := DiskFloor(c.total); got != c.want {
			t.Errorf("DiskFloor(%d GiB) = %d GiB, want %d GiB", c.total/gib, got/gib, c.want/gib)
		}
	}
}
