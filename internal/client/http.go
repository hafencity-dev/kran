package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hafencity-dev/kran/internal/paths"
	"github.com/hafencity-dev/kran/internal/proto"
)

const baseURL = "http://kran"

// DialTimeout bounds connecting and the version handshake: a wedged daemon
// must not hold up `kran run`, which then falls back to local execution.
var DialTimeout = 3 * time.Second

type httpClient struct {
	socket string
	hc     *http.Client
	ver    proto.VersionInfo
}

func dial(ctx context.Context, socket string) (Client, error) {
	if socket == "" {
		socket = paths.Socket()
	}
	ctx, cancel := context.WithTimeout(ctx, DialTimeout)
	defer cancel()
	probe, err := dialUnix(ctx, socket)
	if err != nil {
		return nil, err
	}
	probe.Close()

	c := &httpClient{socket: socket}
	c.hc = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialUnix(ctx, socket)
		},
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
		DisableCompression:    true,
		ResponseHeaderTimeout: 0,
	}}
	v, err := c.Version(ctx)
	if err != nil {
		c.Close()
		var ae *APIError
		if errors.As(err, &ae) {
			return nil, fmt.Errorf("%w: version check answered %d: %s (restart it: kran service restart)", ErrIncompatible, ae.Status, ae.Message)
		}
		if errors.Is(err, ErrPermission) || errors.Is(err, errNoDaemon) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: not responding on %s: %v", errNoDaemon, socket, err)
	}
	if err := compatible(v); err != nil {
		c.Close()
		return nil, err
	}
	c.ver = v
	return c, nil
}

func compatible(v proto.VersionInfo) error {
	if v.API != proto.APIVersion {
		return fmt.Errorf("%w: daemon %s speaks API %s, this kran speaks %s (restart it: kran service restart)",
			ErrIncompatible, v.Version, v.API, proto.APIVersion)
	}
	major := v.Protocol
	if i := strings.IndexByte(major, '.'); i >= 0 {
		major = major[:i]
	}
	if major != strconv.Itoa(proto.ProtocolMajor) {
		return fmt.Errorf("%w: daemon %s speaks protocol %s, this kran speaks %d.%d (restart it: kran service restart)",
			ErrIncompatible, v.Version, v.Protocol, proto.ProtocolMajor, proto.ProtocolMinor)
	}
	return nil
}

func dialUnix(ctx context.Context, socket string) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", socket)
	if err == nil {
		return c, nil
	}
	switch {
	case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("%w (no daemon at %s)", errNoDaemon, socket)
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return nil, fmt.Errorf("%w: %s (is the daemon running as another user? check: kran doctor)", ErrPermission, socket)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return nil, fmt.Errorf("%w: not responding on %s", errNoDaemon, socket)
	}
	return nil, fmt.Errorf("%w: %v", errNoDaemon, err)
}

func (c *httpClient) Close() error {
	if t, ok := c.hc.Transport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
	return nil
}

func (c *httpClient) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return c.transportErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return readAPIError(resp)
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func (c *httpClient) transportErr(err error) error {
	if errors.Is(err, errNoDaemon) || errors.Is(err, ErrPermission) {
		return err
	}
	var ue *url.Error
	if errors.As(err, &ue) && (errors.Is(ue.Err, errNoDaemon) || errors.Is(ue.Err, ErrPermission)) {
		return ue.Err
	}
	return err
}

func readAPIError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var ae proto.APIError
	if json.Unmarshal(b, &ae) == nil && ae.Error != "" {
		return &APIError{Status: resp.StatusCode, Message: ae.Error, Hint: ae.Hint}
	}
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		msg = resp.Status
	}
	return &APIError{Status: resp.StatusCode, Message: msg}
}

func (c *httpClient) Version(ctx context.Context) (proto.VersionInfo, error) {
	var v proto.VersionInfo
	err := c.do(ctx, http.MethodGet, "/api/v1/version", nil, &v)
	return v, err
}

func (c *httpClient) State(ctx context.Context) (*proto.State, error) {
	var s proto.State
	if err := c.do(ctx, http.MethodGet, "/api/v1/state", nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (c *httpClient) Jobs(ctx context.Context, limit int) ([]proto.Job, error) {
	return c.JobsQuery(ctx, JobsQuery{Limit: limit})
}

// JobsQuery filters the job history.
type JobsQuery struct {
	Limit int
	Owner string
	Node  string
}

// JobsQuery lists history with filters (not part of the Client interface;
// reach it with a type assertion to interface{ JobsQuery(...) }).
func (c *httpClient) JobsQuery(ctx context.Context, q JobsQuery) ([]proto.Job, error) {
	v := url.Values{}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.Owner != "" {
		v.Set("owner", q.Owner)
	}
	if q.Node != "" {
		v.Set("node", q.Node)
	}
	path := "/api/v1/jobs"
	if len(v) > 0 {
		path += "?" + v.Encode()
	}
	var jobs []proto.Job
	err := c.do(ctx, http.MethodGet, path, nil, &jobs)
	return jobs, err
}

func (c *httpClient) Job(ctx context.Context, id string) (*proto.Job, error) {
	var j proto.Job
	if err := c.do(ctx, http.MethodGet, "/api/v1/jobs/"+url.PathEscape(id), nil, &j); err != nil {
		return nil, err
	}
	return &j, nil
}

func (c *httpClient) Cancel(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/jobs/"+url.PathEscape(id)+"/cancel", nil, nil)
}

func (c *httpClient) SetPriority(ctx context.Context, id string, p proto.Priority) error {
	return c.do(ctx, http.MethodPost, "/api/v1/jobs/"+url.PathEscape(id)+"/priority", map[string]any{"priority": p}, nil)
}

func (c *httpClient) Drain(ctx context.Context, node string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/nodes/"+url.PathEscape(node)+"/drain", nil, nil)
}

func (c *httpClient) Resume(ctx context.Context, node string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/nodes/"+url.PathEscape(node)+"/resume", nil, nil)
}

// DaemonInfo says which process serves the API (not part of the Client
// interface; reach it with a type assertion).
func (c *httpClient) DaemonInfo(ctx context.Context) (proto.DaemonInfo, error) {
	var v proto.DaemonInfo
	err := c.do(ctx, http.MethodGet, "/api/v1/daemon", nil, &v)
	return v, err
}

// StopDaemon asks the daemon to stop "now" or once "idle" (handover to the
// service manager; not part of the Client interface).
func (c *httpClient) StopDaemon(ctx context.Context, when string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/daemon/stop", map[string]string{"when": when}, nil)
}
