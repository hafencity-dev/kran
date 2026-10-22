package execenv

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// dockerAPI is a minimal Docker Engine API client over the daemon's unix
// socket, used for cheap queries (version, info, stats, list). Everything
// that streams or builds goes through the docker CLI, which handles TTYs,
// signal proxying and BuildKit for us.
type dockerAPI struct {
	sock string
	hc   *http.Client
}

func newDockerAPI(sock string) *dockerAPI {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		MaxIdleConns:    2,
		IdleConnTimeout: 30 * time.Second,
	}
	return &dockerAPI{sock: sock, hc: &http.Client{Transport: tr, Timeout: 20 * time.Second}}
}

func (d *dockerAPI) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := d.hc.Do(req)
	if err != nil {
		return fmt.Errorf("docker api %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(b, &e) == nil && e.Message != "" {
			return &dockerError{status: resp.StatusCode, msg: e.Message}
		}
		return &dockerError{status: resp.StatusCode, msg: strings.TrimSpace(string(b))}
	}
	if v == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

type dockerError struct {
	status int
	msg    string
}

func (e *dockerError) Error() string { return fmt.Sprintf("docker: %s (HTTP %d)", e.msg, e.status) }

func isNotFound(err error) bool {
	de, ok := err.(*dockerError)
	return ok && de.status == http.StatusNotFound
}

type dockerVersion struct {
	Version    string `json:"Version"`
	APIVersion string `json:"ApiVersion"`
	Os         string `json:"Os"`
	Arch       string `json:"Arch"`
}

type dockerInfo struct {
	ID            string `json:"ID"`
	NCPU          int    `json:"NCPU"`
	MemTotal      int64  `json:"MemTotal"`
	OSType        string `json:"OSType"`
	Architecture  string `json:"Architecture"`
	ServerVersion string `json:"ServerVersion"`
	Name          string `json:"Name"`
	// SecurityOptions lists e.g. "name=rootless" for a rootless daemon.
	SecurityOptions []string `json:"SecurityOptions"`
}

// rootless reports a rootless Docker daemon: it cannot raise limits above
// its own.
func (i dockerInfo) rootless() bool {
	for _, o := range i.SecurityOptions {
		if o == "name=rootless" || strings.HasPrefix(o, "name=rootless,") {
			return true
		}
	}
	return false
}

type containerSummary struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Labels map[string]string `json:"Labels"`
	State  string            `json:"State"`
}

// containers lists containers (all states) carrying the label filter.
func (d *dockerAPI) containers(ctx context.Context, label string) ([]containerSummary, error) {
	return d.containersWhere(ctx, map[string][]string{"label": {label}})
}

// containersWhere lists containers (all states) matching Docker filters.
func (d *dockerAPI) containersWhere(ctx context.Context, filters map[string][]string) ([]containerSummary, error) {
	f, _ := json.Marshal(filters)
	var out []containerSummary
	err := d.get(ctx, "/containers/json?all=1&filters="+url.QueryEscape(string(f)), &out)
	return out, err
}

type statsSample struct {
	mem int64
	cpu float64 // seconds
}

// stats returns one stats sample of a running container.
func (d *dockerAPI) stats(ctx context.Context, name string) (statsSample, error) {
	var st struct {
		MemoryStats struct {
			Usage int64            `json:"usage"`
			Stats map[string]int64 `json:"stats"`
		} `json:"memory_stats"`
		CPUStats struct {
			CPUUsage struct {
				TotalUsage int64 `json:"total_usage"`
			} `json:"cpu_usage"`
		} `json:"cpu_stats"`
	}
	if err := d.get(ctx, "/containers/"+url.PathEscape(name)+"/stats?stream=false&one-shot=true", &st); err != nil {
		return statsSample{}, err
	}
	// Like `docker stats`: page cache that can be reclaimed is not "used".
	mem := st.MemoryStats.Usage - st.MemoryStats.Stats["inactive_file"]
	if mem < 0 {
		mem = st.MemoryStats.Usage
	}
	return statsSample{mem: mem, cpu: float64(st.CPUStats.CPUUsage.TotalUsage) / 1e9}, nil
}

// apiVersionAtLeast compares "1.45"-style API versions.
func apiVersionAtLeast(v string, major, minor int) bool {
	var a, b int
	if _, err := fmt.Sscanf(v, "%d.%d", &a, &b); err != nil {
		return false
	}
	return a > major || a == major && b >= minor
}

// dockerStats is the supervisor's usage probe for container jobs.
func (s *supervisor) dockerStats(name string) (statsSample, bool) {
	d := s.spec.Docker
	if d == nil || d.API == "" {
		return statsSample{}, false
	}
	s.statMu.Lock()
	if s.api == nil {
		s.api = newDockerAPI(d.API)
	}
	api := s.api
	s.statMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := api.stats(ctx, name)
	return st, err == nil
}
