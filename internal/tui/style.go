package tui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/hafencity-dev/kran/internal/proto"
)

// styles are pure values (Lip Gloss v2 does no terminal I/O). With NO_COLOR
// every style is the zero style, so the view carries no escape sequences
// at all; otherwise Bubble Tea downsamples colors to what the terminal has.
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

func newStyles(noColor bool) styles {
	if noColor {
		plain := lipgloss.NewStyle()
		return styles{
			noColor: true, title: plain, active: plain, muted: plain, bold: plain, sel: plain,
			good: plain, warn: plain, bad: plain, crane: plain, reason: plain,
			owner: map[proto.OwnerKind]lipgloss.Style{}, others: plain, outside: plain, free: plain, reserve: plain,
		}
	}
	c := lipgloss.Color
	st := styles{
		noColor: noColor,
		title:   lipgloss.NewStyle().Bold(true),
		active:  lipgloss.NewStyle().Bold(true).Foreground(c("220")),
		muted:   lipgloss.NewStyle().Foreground(c("245")),
		bold:    lipgloss.NewStyle().Bold(true),
		sel:     lipgloss.NewStyle().Reverse(true),
		good:    lipgloss.NewStyle().Foreground(c("35")),
		warn:    lipgloss.NewStyle().Foreground(c("214")),
		bad:     lipgloss.NewStyle().Foreground(c("203")),
		crane:   lipgloss.NewStyle().Foreground(c("220")).Bold(true),
		reason:  lipgloss.NewStyle().Foreground(c("223")),
		owner: map[proto.OwnerKind]lipgloss.Style{
			proto.OwnerHuman:  lipgloss.NewStyle().Foreground(c("33")),
			proto.OwnerClaude: lipgloss.NewStyle().Foreground(c("208")),
			proto.OwnerCodex:  lipgloss.NewStyle().Foreground(c("36")),
			proto.OwnerAgent:  lipgloss.NewStyle().Foreground(c("99")),
			proto.OwnerCI:     lipgloss.NewStyle().Foreground(c("205")),
		},
		others:  lipgloss.NewStyle().Foreground(c("246")),
		outside: lipgloss.NewStyle().Foreground(c("240")),
		free:    lipgloss.NewStyle().Foreground(c("238")),
		reserve: lipgloss.NewStyle().Foreground(c("240")),
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
