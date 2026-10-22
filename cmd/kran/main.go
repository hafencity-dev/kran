// Command kran moves heavy dev jobs to whichever of your machines has room.
package main

import (
	"os"

	"github.com/hafencity-dev/kran/internal/cli"
)

func main() { os.Exit(cli.Main(os.Args)) }
