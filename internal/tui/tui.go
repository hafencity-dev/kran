// Package tui implements `kran top`.
package tui

import (
	"context"
	"errors"

	"github.com/hafencity-dev/kran/internal/client"
)

// Run shows the live terminal view until ctx ends or the user quits.
func Run(ctx context.Context, c client.Client) error { return errors.New("not implemented") }
