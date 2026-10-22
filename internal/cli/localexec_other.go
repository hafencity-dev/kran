//go:build !linux

package cli

import (
	"os/signal"
	"syscall"
)

// macOS reports ru_maxrss in bytes.
func maxRSSBytes(v int64) int64 { return v }

func ignoreTTOU() { signal.Ignore(syscall.SIGTTOU) }

// No cgroups here: the daemon's watchdog enforces caps on macOS.
func scopeUsable(int64) bool     { return false }
func scopeOOMKilled(string) bool { return false }
func totalMemory() int64         { return 0 }
