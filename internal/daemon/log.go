package daemon

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// rotator is an io.Writer over path that rotates at max bytes, keeping
// `keep` files in total (path, path.1, …). SPEC §4: 10 MB × 3.
type rotator struct {
	mu   sync.Mutex
	path string
	max  int64
	keep int
	f    *os.File
	size int64
}

func newRotator(path string, max int64, keep int) (*rotator, error) {
	r := &rotator{path: path, max: max, keep: keep}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *rotator) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, st.Size()
	return nil
}

func (r *rotator) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	if r.size+int64(len(p)) > r.max && r.size > 0 {
		r.rotate()
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *rotator) rotate() {
	r.f.Close()
	r.f = nil
	for i := r.keep - 1; i >= 1; i-- {
		src := r.path
		if i > 1 {
			src = fmt.Sprintf("%s.%d", r.path, i-1)
		}
		os.Rename(src, fmt.Sprintf("%s.%d", r.path, i))
	}
	if r.keep <= 1 {
		os.Remove(r.path)
	}
	if err := r.open(); err != nil {
		r.f = nil
	}
}

func (r *rotator) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// logger writes timestamped lines to its writers.
type logger struct {
	mu  sync.Mutex
	out []io.Writer
	now func() time.Time
}

func (l *logger) Logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	line := l.now().Format("2006-01-02T15:04:05.000Z07:00") + " " + strings.TrimRight(msg, "\n") + "\n"
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, w := range l.out {
		w.Write([]byte(line))
	}
}
