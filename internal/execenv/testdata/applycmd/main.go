// Command applycmd is a minimal static stand-in for the Linux kran binary in
// container tests: it serves `internal apply` like `kran internal apply`.
package main

import (
	"os"

	"github.com/hafencity-dev/kran/internal/execenv"
)

func main() {
	if len(os.Args) >= 3 && os.Args[1] == "internal" && os.Args[2] == "apply" {
		os.Exit(execenv.Apply(os.Args[3:]))
	}
	os.Exit(2)
}
