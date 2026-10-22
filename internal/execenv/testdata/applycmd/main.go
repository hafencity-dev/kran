// Command applycmd is a minimal static stand-in for the Linux kran binary in
// container tests: it serves `internal apply`, `internal docker-proxy` and
// `internal cache-trim` like the kran binary does.
package main

import (
	"os"

	"github.com/hafencity-dev/kran/internal/execenv"
)

func main() {
	if len(os.Args) >= 3 && os.Args[1] == "internal" {
		switch os.Args[2] {
		case "apply":
			os.Exit(execenv.Apply(os.Args[3:]))
		case "docker-proxy":
			os.Exit(execenv.DockerProxy(os.Args[3:]))
		case "cache-trim":
			os.Exit(execenv.CacheTrim(os.Args[3:]))
		}
	}
	os.Exit(2)
}
