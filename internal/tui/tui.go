// Package tui implements `kran top`: a live terminal view of machines,
// running jobs and the queue, with cancel, drain/resume and log following.
package tui

import (
	"context"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/hafencity-dev/kran/internal/client"
)

// Run shows the live view until the user quits or ctx ends. Colors follow
// the terminal and are off when NO_COLOR is set.
func Run(ctx context.Context, c client.Client) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := New(ctx, c, Options{NoColor: os.Getenv("NO_COLOR") != ""})
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithOutput(os.Stdout))
	_, err := p.Run()
	if err != nil && ctx.Err() != nil {
		// Interrupted from outside (signal, parent exit): a normal end.
		return nil
	}
	return err
}
