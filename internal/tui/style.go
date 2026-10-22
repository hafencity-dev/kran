package tui

import (
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/muesli/termenv"
)

// styles are built on a renderer bound to the output, so NO_COLOR and
// non-tty output strip every escape without touching global state.
type styles struct {
	noColor bool
	title   lipgloss.Style
	active  lipgloss.Style
	muted   lipgloss.Style
	bold    lipgloss.Style
	sel     lipgloss.Style
	good    lipgloss.Style
	warn    lipgloss.Style
	bad     lipgloss.Style
	crane   lipgloss.Style
	reason  lipgloss.Style
	owner   map[proto.OwnerKind]lipgloss.Style
	others  lipgloss.Style
	outside lipgloss.Style
	free    lipgloss.Style
	reserve lipgloss.Style
}

func newStyles(w io.Writer, noColor bool) styles {
	r := lipgloss.NewRenderer(w)
	if noColor {
		r.SetColorProfile(termenv.Ascii)
	}
	c := func(s string) lipgloss.Color { return lipgloss.Color(s) }
	st := styles{
		noColor: noColor,
		title:   r.NewStyle().Bold(true),
		active:  r.NewStyle().Bold(true).Foreground(c("220")),
		muted:   r.NewStyle().Foreground(c("245")),
		bold:    r.NewStyle().Bold(true),
		sel:     r.NewStyle().Reverse(true),
		good:    r.NewStyle().Foreground(c("35")),
		warn:    r.NewStyle().Foreground(c("214")),
		bad:     r.NewStyle().Foreground(c("203")),
		crane:   r.NewStyle().Foreground(c("220")).Bold(true),
		reason:  r.NewStyle().Foreground(c("223")),
		owner: map[proto.OwnerKind]lipgloss.Style{
			proto.OwnerHuman:  r.NewStyle().Foreground(c("33")),
			proto.OwnerClaude: r.NewStyle().Foreground(c("208")),
			proto.OwnerCodex:  r.NewStyle().Foreground(c("36")),
			proto.OwnerAgent:  r.NewStyle().Foreground(c("99")),
			proto.OwnerCI:     r.NewStyle().Foreground(c("205")),
		},
		others:  r.NewStyle().Foreground(c("246")),
		outside: r.NewStyle().Foreground(c("240")),
		free:    r.NewStyle().Foreground(c("238")),
		reserve: r.NewStyle().Foreground(c("240")),
	}
	return st
}

func (s styles) ownerStyle(k proto.OwnerKind) lipgloss.Style {
	if st, ok := s.owner[k]; ok {
		return st
	}
	return s.muted
}

// ---------------------------------------------------------------------------
// Humane units, matching the web UI.

const gib = float64(1 << 30)

func fmtGiB(b int64) string {
	v := float64(b) / gib
	if v >= 10 || v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
}

func fmtBytes(b int64) string {
	switch {
	case b >= 1<<40:
		return fmt.Sprintf("%.1f TiB", float64(b)/(1<<40))
	case b >= 1<<30:
		return fmtGiB(b) + " GiB"
	case b >= 1<<20:
		return fmt.Sprintf("%.0f MiB", float64(b)/(1<<20))
	}
	return fmt.Sprintf("%d KiB", b>>10)
}

func fmtCores(c float64) string {
	if c >= 10 || c == math.Trunc(c) {
		return fmt.Sprintf("%.0f", c)
	}
	return fmt.Sprintf("%.1f", c)
}

// fmtDur is the compact terminal form: "3m12s", "41s", "1h04m".
func fmtDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int(d.Round(time.Second) / time.Second)
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh%02dm", s/3600, (s%3600)/60)
}

func command(spec proto.JobSpec) string {
	if spec.Shell != "" {
		return spec.Shell
	}
	return strings.Join(spec.Argv, " ")
}

// validTime treats Go's zero time (serialized despite omitempty) as unset.
func validTime(t time.Time) bool { return !t.IsZero() && t.Year() > 2000 }
