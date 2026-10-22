//go:build e2e || chaos || smoke

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// The real kran binary every black-box test drives, built once per run.
var (
	kranBin string
	// raceDir collects data race reports of every kran process
	// (GORACE=log_path=...) when the binary is built with -race.
	raceDir string
	binDir  string
)

// beforeAll builds the kran binary into a temp dir. KRAN_E2E_RACE=1
// builds it with the race detector (make e2e/chaos do), KRAN_E2E_BIN uses
// a prebuilt binary instead.
func beforeAll() func() {
	if p := os.Getenv("KRAN_E2E_BIN"); p != "" {
		abs, err := filepath.Abs(p)
		if err != nil {
			fmt.Fprintln(os.Stderr, "e2e:", err)
			os.Exit(2)
		}
		kranBin = abs
	}
	dir, err := os.MkdirTemp("", "kran-e2e-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		os.Exit(2)
	}
	binDir = dir
	raceDir = filepath.Join(dir, "race")
	os.MkdirAll(raceDir, 0o755)
	if kranBin == "" {
		kranBin = filepath.Join(dir, "kran")
		args := []string{"build", "-trimpath", "-o", kranBin}
		if os.Getenv("KRAN_E2E_RACE") == "1" {
			args = append(args, "-race")
		} else {
			args = append(args, "-ldflags", "-s -w")
		}
		args = append(args, "github.com/hafencity-dev/kran/cmd/kran")
		cmd := exec.Command("go", args...)
		cmd.Env = os.Environ()
		if os.Getenv("KRAN_E2E_RACE") != "1" {
			cmd.Env = append(cmd.Env, "CGO_ENABLED=0")
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "e2e: build kran: %v\n%s", err, out)
			os.RemoveAll(dir)
			os.Exit(2)
		}
	}
	return func() {
		if reports := raceReports(); reports != "" {
			fmt.Fprintf(os.Stderr, "e2e: DATA RACES in kran processes:\n%s\n", reports)
		}
		if os.Getenv("KRAN_E2E_KEEP") == "" {
			os.RemoveAll(dir)
		}
	}
}

// raceReports returns the race detector output written so far.
func raceReports() string {
	ents, _ := os.ReadDir(raceDir)
	var b strings.Builder
	for _, e := range ents {
		data, _ := os.ReadFile(filepath.Join(raceDir, e.Name()))
		if strings.Contains(string(data), "DATA RACE") {
			fmt.Fprintf(&b, "--- %s\n%s\n", e.Name(), data)
		}
	}
	return b.String()
}

// hostOS is a tiny helper for skips.
func hostOS() string { return runtime.GOOS }
