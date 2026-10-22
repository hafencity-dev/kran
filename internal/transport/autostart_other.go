//go:build !unix

package transport

import "context"

func autostart(ctx context.Context, sock string) error { return nil }
