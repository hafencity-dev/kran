package client

import (
	"context"
	"errors"
)

var errNoDaemon = errors.New("kran daemon is not running")

// Replaced by the real implementation (owner A).
func dial(ctx context.Context, socket string) (Client, error) { return nil, errNoDaemon }
