package cli

import "errors"

func asUsage(err error, target **UsageError) bool { return errors.As(err, target) }
