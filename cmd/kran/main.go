// Command kran moves heavy dev jobs to whichever of your machines has room.
package main

import (
	"os"

	"github.com/hafencity-dev/kran/internal/cli"
	"github.com/hafencity-dev/kran/internal/tui"
)

func main() {
	cli.DefaultHooks.Top = tui.Run
	os.Exit(cli.Main(os.Args))
}
