package client

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

type sseEvent struct {
	Event string
	ID    string
	Data  string
}

// readSSE calls fn for every complete event until r ends or fn returns an error.
func readSSE(r io.Reader, fn func(sseEvent) error) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var ev sseEvent
	var data strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return io.ErrUnexpectedEOF
			}
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if data.Len() > 0 || ev.Event != "" {
				ev.Data = data.String()
				if err := fn(ev); err != nil {
					return err
				}
			}
			ev = sseEvent{}
			data.Reset()
			continue
		}
		if line[0] == ':' {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			ev.Event = value
		case "id":
			ev.ID = value
		case "data":
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(value)
		}
	}
}

// DecodeEvent parses the JSON of one SSE data line into an Event whose Data
// holds the typed payload: proto.State, proto.NodeState,
// proto.NodeMetricsEvent, proto.Job, proto.QueueEvent, proto.Stats or
// proto.LogEvent (unknown types keep the raw json.RawMessage).
func DecodeEvent(b []byte) (proto.Event, error) {
	var raw struct {
		Type proto.EventType `json:"type"`
		Seq  uint64          `json:"seq"`
		Time time.Time       `json:"time"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return proto.Event{}, err
	}
	ev := proto.Event{Type: raw.Type, Seq: raw.Seq, Time: raw.Time}
	var err error
	switch raw.Type {
	case proto.EventState:
		var v proto.State
		err = json.Unmarshal(raw.Data, &v)
		ev.Data = v
	case proto.EventNode:
		var v proto.NodeState
		err = json.Unmarshal(raw.Data, &v)
		ev.Data = v
	case proto.EventNodeMetrics:
		var v proto.NodeMetricsEvent
		err = json.Unmarshal(raw.Data, &v)
		ev.Data = v
	case proto.EventJob:
		var v proto.Job
		err = json.Unmarshal(raw.Data, &v)
		ev.Data = v
	case proto.EventQueue:
		var v proto.QueueEvent
		err = json.Unmarshal(raw.Data, &v)
		ev.Data = v
	case proto.EventStats:
		var v proto.Stats
		err = json.Unmarshal(raw.Data, &v)
		ev.Data = v
	case proto.EventLog:
		var v proto.LogEvent
		err = json.Unmarshal(raw.Data, &v)
		ev.Data = v
	default:
		ev.Data = raw.Data
	}
	return ev, err
}

var errResync = errors.New("event sequence gap")

func (c *httpClient) Events(ctx context.Context) (<-chan proto.Event, error) {
	body, err := c.openEvents(ctx)
	if err != nil {
		return nil, err
	}
	out := make(chan proto.Event, 64)
	go func() {
		defer close(out)
		backoff := 200 * time.Millisecond
		for {
			gotState := c.pumpEvents(ctx, body, out)
			body.Close()
			if ctx.Err() != nil {
				return
			}
			if gotState {
				backoff = 200 * time.Millisecond
			}
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				if backoff < 5*time.Second {
					backoff *= 2
				}
				body, err = c.openEvents(ctx)
				if err == nil {
					break
				}
				if ctx.Err() != nil {
					return
				}
			}
		}
	}()
	return out, nil
}

func (c *httpClient) openEvents(ctx context.Context) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/events", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, c.transportErr(err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, readAPIError(resp)
	}
	return resp.Body, nil
}

// pumpEvents forwards events until the stream breaks or shows a gap (the
// daemon dropped us as a slow consumer); the caller reconnects and the
// daemon starts over with a fresh state event.
func (c *httpClient) pumpEvents(ctx context.Context, body io.Reader, out chan<- proto.Event) (gotState bool) {
	var last uint64
	readSSE(body, func(se sseEvent) error {
		if se.Data == "" {
			return nil
		}
		ev, err := DecodeEvent([]byte(se.Data))
		if err != nil {
			return err
		}
		if ev.Type == proto.EventState {
			gotState = true
		} else if !gotState || (last != 0 && ev.Seq != last+1) {
			return errResync
		}
		last = ev.Seq
		select {
		case out <- ev:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	return gotState
}

// Log copies a job's output (stdout and stderr interleaved) to w. With
// follow it streams until the job ends, reconnecting from the last offset
// if the connection drops.
func (c *httpClient) Log(ctx context.Context, id string, follow bool, w io.Writer) error {
	path := "/api/v1/jobs/" + url.PathEscape(id) + "/log"
	if !follow {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
		if err != nil {
			return err
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			return c.transportErr(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return readAPIError(resp)
		}
		_, err = io.Copy(w, resp.Body)
		return err
	}
	var offset int64
	failures := 0
	for {
		done, err := c.followLog(ctx, path, &offset, w)
		if done {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ae *APIError
		if errors.As(err, &ae) {
			return err
		}
		failures++
		if failures > 20 {
			return fmt.Errorf("follow log of %s: %w", id, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

var errLogEnd = errors.New("log end")

func (c *httpClient) followLog(ctx context.Context, path string, offset *int64, w io.Writer) (bool, error) {
	u := path + "?follow=1&offset=" + strconv.FormatInt(*offset, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+u, nil)
	if err != nil {
		return true, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.hc.Do(req)
	if err != nil {
		return false, c.transportErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, readAPIError(resp)
	}
	err = readSSE(resp.Body, func(se sseEvent) error {
		switch se.Event {
		case "", "message":
			var ch proto.OutputChunk
			if err := json.Unmarshal([]byte(se.Data), &ch); err != nil {
				return err
			}
			b := []byte(ch.Data)
			if ch.B64 != "" {
				var err error
				if b, err = base64.StdEncoding.DecodeString(ch.B64); err != nil {
					return err
				}
			}
			// Offset is after the chunk; skip what we already wrote.
			start := ch.Offset - int64(len(b))
			if skip := *offset - start; skip > 0 {
				if skip >= int64(len(b)) {
					return nil
				}
				b = b[skip:]
			}
			if _, err := w.Write(b); err != nil {
				return err
			}
			*offset += int64(len(b))
		case "end", "exit":
			return errLogEnd
		}
		return nil
	})
	if errors.Is(err, errLogEnd) {
		return true, nil
	}
	return false, err
}
