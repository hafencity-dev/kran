package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
)

func isTerminal(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

// style renders color only on a tty with NO_COLOR unset.
type style struct{ on bool }

func (a *App) styleOut() style { return style{on: a.outTTY && colorAllowed(a.Env())} }
func (a *App) styleErr() style { return style{on: a.errTTY && colorAllowed(a.Env())} }

func colorAllowed(env []string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, "NO_COLOR=") && kv != "NO_COLOR=" {
			return false
		}
		if kv == "TERM=dumb" {
			return false
		}
	}
	return true
}

func (s style) wrap(code, t string) string {
	if !s.on {
		return t
	}
	return "\x1b[" + code + "m" + t + "\x1b[0m"
}
func (s style) Bold(t string) string   { return s.wrap("1", t) }
func (s style) Dim(t string) string    { return s.wrap("2", t) }
func (s style) Red(t string) string    { return s.wrap("31", t) }
func (s style) Green(t string) string  { return s.wrap("32", t) }
func (s style) Yellow(t string) string { return s.wrap("33", t) }

// humanBytes renders 1536 as "1.5 KiB".
func humanBytes(n int64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(u), 0
	for v := n / u; v >= u && exp < 4; v /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// lineWriter tracks whether the last byte written ended a line, so kran's own
// "kran:" lines never glue onto a program's unterminated stderr output.
type lineWriter struct {
	w    io.Writer
	last byte
}

func (l *lineWriter) Write(p []byte) (int, error) {
	n, err := l.w.Write(p)
	if n > 0 {
		l.last = p[n-1]
	}
	return n, err
}

// line prints text as its own line, first finishing a pending partial line.
func (l *lineWriter) line(text string) {
	if l.last != 0 && l.last != '\n' {
		_, _ = l.w.Write([]byte("\n"))
	}
	_, _ = l.Write([]byte(text + "\n"))
}
