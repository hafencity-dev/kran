package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"
)

var errPickCanceled = errors.New("canceled")

// pick lets the user choose one of items: arrow keys (or j/k) and Enter on a
// terminal, a numbered prompt otherwise. Returns the index.
func pick(in *os.File, out io.Writer, title string, items []string) (int, error) {
	if len(items) == 0 {
		return -1, errors.New("nothing to choose from")
	}
	if in != nil && isTerminal(in) {
		if i, err := pickArrows(in, out, title, items); err == nil || errors.Is(err, errPickCanceled) {
			return i, err
		}
	}
	return pickNumbered(bufio.NewReader(in), out, title, items)
}

func pickNumbered(r *bufio.Reader, out io.Writer, title string, items []string) (int, error) {
	fmt.Fprintln(out, title)
	for i, it := range items {
		fmt.Fprintf(out, "  %d) %s\n", i+1, it)
	}
	for tries := 0; tries < 3; tries++ {
		fmt.Fprintf(out, "Number (1-%d, empty to cancel): ", len(items))
		line, err := r.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			return -1, errPickCanceled
		}
		if n, e := strconv.Atoi(line); e == nil && n >= 1 && n <= len(items) {
			return n - 1, nil
		}
		if err != nil {
			break
		}
		fmt.Fprintln(out, "That is not one of the numbers above.")
	}
	return -1, errPickCanceled
}

func pickArrows(in *os.File, out io.Writer, title string, items []string) (int, error) {
	old, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return -1, err
	}
	defer term.Restore(int(in.Fd()), old)
	sel := 0
	draw := func(first bool) {
		if !first {
			fmt.Fprintf(out, "\x1b[%dA", len(items)+1)
		}
		fmt.Fprintf(out, "\r\x1b[2K%s (arrows to move, Enter to choose, q to cancel)\r\n", title)
		for i, it := range items {
			mark := "  "
			if i == sel {
				mark = "> "
			}
			fmt.Fprintf(out, "\r\x1b[2K%s%s\r\n", mark, it)
		}
	}
	draw(true)
	buf := make([]byte, 8)
	for {
		n, err := in.Read(buf)
		if err != nil {
			return -1, err
		}
		k := string(buf[:n])
		switch {
		case k == "\r" || k == "\n":
			return sel, nil
		case k == "q" || k == "\x03" || k == "\x1b" || k == "\x04":
			return -1, errPickCanceled
		case k == "\x1b[A" || k == "k":
			sel = (sel + len(items) - 1) % len(items)
		case k == "\x1b[B" || k == "j":
			sel = (sel + 1) % len(items)
		default:
			continue
		}
		draw(false)
	}
}
