package daemon

// SliceController maintains the memory limit of kran.slice, the systemd
// user slice that local jobs run in (Linux). Available is false without a
// systemd user session; the CLI then uses a plain process group.
type SliceController interface {
	Name() string
	Available() bool
	// Refresh recomputes and applies MemoryMax (cheap when unchanged).
	Refresh()
}

// SliceName is the slice local jobs run in.
const SliceName = "kran.slice"
