//go:build !unix

package transport

import "context"

func autostart(ctx context.Context, sock string) error { return nil }

// HoldStartLock is a no-op where no daemon is ever started on demand.
func HoldStartLock(ctx context.Context) (func(), error) { return func() {}, nil }
