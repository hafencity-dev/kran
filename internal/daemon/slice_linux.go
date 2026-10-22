//go:build linux

package daemon

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// systemdSlice keeps kran.slice's MemoryMax at
//
//	memory.current(slice) + MemAvailable − reserve
//
// capped at MemTotal − reserve, floor 1 GiB, so all local kran jobs
// together can only take what the rest of the machine can spare (the
// algorithm of lokdev-slice-refresh, proven on gx10). MemoryHigh stays
// "infinity": without swap a memory.high breach only throttles a job into
// looking hung; a MemoryMax breach OOM-kills inside the slice instead.
type systemdSlice struct {
	reserve int64
	logf    func(string, ...any)
	name    string // "" = SliceName
	// injectable for tests
	run      func(ctx context.Context, args ...string) (string, error)
	meminfo  func() (total, avail int64, err error)
	readFile func(string) ([]byte, error)

	mu        sync.Mutex
	available bool
	last      int64
	failed    bool
	cgroup    string // the slice's cgroup path, asked once (a systemctl fork)
}

const sliceStep = 64 << 20 // don't bother systemd for smaller changes

func newSliceController(name string, reserve int64, logf func(string, ...any)) SliceController {
	s := &systemdSlice{name: name, reserve: reserve, logf: logf, run: systemctl, meminfo: procMeminfo, readFile: os.ReadFile}
	s.available = s.detect()
	if !s.available {
		logf("kran.slice: no systemd user session; local jobs run in a plain process group")
	}
	return s
}

func (s *systemdSlice) detect() bool {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := s.run(ctx, "show-environment")
	return err == nil
}

func (s *systemdSlice) Name() string {
	if s.name != "" {
		return s.name
	}
	return SliceName
}

func (s *systemdSlice) Available() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.available
}

func (s *systemdSlice) compute(ctx context.Context) (int64, error) {
	total, avail, err := s.meminfo()
	if err != nil {
		return 0, err
	}
	var cur int64
	if s.cgroup == "" {
		if cg, err := s.run(ctx, "show", s.Name(), "-p", "ControlGroup", "--value"); err == nil {
			s.cgroup = strings.TrimSpace(cg)
		}
	}
	if s.cgroup != "" {
		if b, err := s.readFile("/sys/fs/cgroup" + s.cgroup + "/memory.current"); err == nil {
			cur, _ = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		} else {
			s.cgroup = "" // gone (the slice was stopped): ask again next time
		}
	}
	return sliceMax(cur, avail, total, s.reserve), nil
}

func sliceMax(cur, avail, total, reserve int64) int64 {
	max := cur + avail - reserve
	if c := total - reserve; max > c {
		max = c
	}
	if max < 1<<30 {
		max = 1 << 30
	}
	return max
}

func (s *systemdSlice) Refresh() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.available {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	max, err := s.compute(ctx)
	if err != nil {
		s.logf("kran.slice: %v", err)
		return
	}
	if d := max - s.last; s.last != 0 && d < sliceStep && d > -sliceStep {
		return
	}
	out, err := s.run(ctx, "set-property", "--runtime", s.Name(), fmt.Sprintf("MemoryMax=%d", max), "MemoryHigh=infinity", fmt.Sprintf("TasksMax=%d", proto.SliceTasksMax))
	if err != nil {
		if !s.failed {
			s.logf("kran.slice: set MemoryMax: %v %s (retrying every refresh)", err, out)
		}
		s.failed = true
		return
	}
	if s.failed {
		s.logf("kran.slice: MemoryMax set again")
	}
	s.failed = false
	s.last = max
}

func procMeminfo() (total, avail int64, err error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	return parseMeminfo(b)
}

func parseMeminfo(b []byte) (total, avail int64, err error) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseInt(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			total = v * 1024
		case "MemAvailable:":
			avail = v * 1024
		}
	}
	if total == 0 {
		return 0, 0, fmt.Errorf("no MemTotal in /proc/meminfo")
	}
	return total, avail, nil
}
