//go:build !linux

package daemon

// No systemd outside Linux: local jobs run in their own process group.
func newSliceController(name string, reserve int64, logf func(string, ...any)) SliceController {
	return nil
}
