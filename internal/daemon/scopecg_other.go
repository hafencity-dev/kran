//go:build !linux

package daemon

// scopeCgroup: systemd scopes exist on Linux only; elsewhere local jobs are
// measured by their process group.
func scopeCgroup(pid int, scope string) string { return "" }
