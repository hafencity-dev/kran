package scheduler

import (
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	mib = int64(1) << 20
	gib = int64(1) << 30
)

// fmtBytes renders memory the way reasons show it: "6.0 GiB", "512 MiB".
func fmtBytes(b int64) string {
	if b < 0 {
		b = 0
	}
	if b >= gib {
		return fmt.Sprintf("%.1f GiB", float64(b)/float64(gib))
	}
	return fmt.Sprintf("%d MiB", b/mib)
}

// fmtCPUs renders a CPU amount: "8 CPUs", "1 CPU", "2.5 CPUs".
func fmtCPUs(c float64) string {
	n := fmtNum(c)
	if n == "1" {
		return "1 CPU"
	}
	return n + " CPUs"
}

// fmtNum renders a non-negative quantity with at most one decimal.
func fmtNum(c float64) string {
	if c < 0 {
		c = 0
	}
	r := math.Round(c*10) / 10
	if r == math.Trunc(r) {
		return fmt.Sprintf("%d", int64(r))
	}
	return fmt.Sprintf("%.1f", r)
}

// fmtDur renders seconds as a short duration: "41s", "3m10s", "1h5m".
func fmtDur(sec float64) string {
	if math.IsInf(sec, 0) || math.IsNaN(sec) {
		return "?"
	}
	d := time.Duration(math.Round(sec)) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		m := d / time.Minute
		s := (d % time.Minute) / time.Second
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm%ds", m, s)
	default:
		h := d / time.Hour
		m := (d % time.Hour) / time.Minute
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	}
}

// joinList renders "a", "a and b", "a, b and c".
func joinList(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}
