package execenv

import "errors"

// Replaced by the real implementation (owner B).
func newManager(opts Options) (Manager, error) { return nil, errors.New("execenv: not implemented") }

// Supervise is the entry point of `kran internal supervise <job dir>` (SPEC §6.2).
func Supervise(args []string) int { return 1 }
