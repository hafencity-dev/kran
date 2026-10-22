package execenv

import (
	"fmt"
	"strconv"
)

const gib = int64(1) << 30

// fmtGiB renders a memory size the way kran talks to users: "6 GiB",
// "2.5 GiB", "512 MiB".
func fmtGiB(b int64) string {
	if b <= 0 {
		return "0 GiB"
	}
	if b < gib {
		return fmt.Sprintf("%d MiB", (b+(1<<20)-1)>>20)
	}
	v := float64(b) / float64(gib)
	s := strconv.FormatFloat(v, 'f', 1, 64)
	if s[len(s)-2:] == ".0" {
		s = s[:len(s)-2]
	}
	return s + " GiB"
}

// suggestMem proposes a --mem value after an OOM: twice the cap, rounded
// up to whole GiB ("12G").
func suggestMem(capBytes int64) string {
	g := (2*capBytes + gib - 1) / gib
	return fmt.Sprintf("%dG", max(g, 2))
}
