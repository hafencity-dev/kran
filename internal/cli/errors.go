package cli

import (
	"errors"
	"runtime"
)

func asUsage(err error, target **UsageError) bool { return errors.As(err, target) }

func runtimeGOOS() string { return runtime.GOOS }
