package mock

import (
	"context"
	"errors"
	"io"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/proto"
)

// Client adapts a Sim to client.Client so `kran top` can run against the
// simulation (go run ./internal/ui/mockserver -top) and in tests.
type Client struct{ S *Sim }

var _ client.Client = Client{}

func (c Client) Version(context.Context) (proto.VersionInfo, error) {
	return c.S.State().Version, nil
}

func (c Client) State(context.Context) (*proto.State, error) { return c.S.State(), nil }

func (c Client) Events(ctx context.Context) (<-chan proto.Event, error) {
	src, cancel := c.S.Subscribe()
	out := make(chan proto.Event, 64)
	go func() {
		defer close(out)
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-src:
				if !ok {
					return
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

func (c Client) Jobs(_ context.Context, limit int) ([]proto.Job, error) {
	return c.S.Jobs(limit, "", "", ""), nil
}

func (c Client) Job(_ context.Context, id string) (*proto.Job, error) {
	j, err := c.S.Job(id)
	if err != nil {
		return nil, err
	}
	return &j, nil
}

func (c Client) Log(ctx context.Context, id string, follow bool, w io.Writer) error {
	offset := 0
	for {
		chunk, done, wait, err := c.S.LogChunk(id, offset)
		if err != nil {
			return err
		}
		if len(chunk) > 0 {
			offset += len(chunk)
			if _, err := w.Write(chunk); err != nil {
				return err
			}
		}
		if done || !follow {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wait:
		}
	}
}

func (c Client) Cancel(_ context.Context, id string) error { return c.S.Cancel(id) }

func (c Client) SetPriority(_ context.Context, id string, p proto.Priority) error {
	return c.S.SetPriority(id, p)
}

func (c Client) Drain(_ context.Context, node string) error  { return c.S.Drain(node) }
func (c Client) Resume(_ context.Context, node string) error { return c.S.Resume(node) }

func (c Client) Run(context.Context) (client.RunSession, error) {
	return nil, errors.New("the mock daemon cannot run jobs")
}

func (c Client) Close() error { return nil }
