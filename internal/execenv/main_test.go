package execenv

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/selfexe"
)

// TestMain lets the test binary stand in for the kran binary: the manager
// launches `<self> internal supervise <dir>`, the sync helper runs
// `<self> internal apply`, and jobs use `<self> helper …` as a portable
// toolbox (sleepers, escapees, memory hogs).
func TestMain(m *testing.M) {
	selfexe.AllowTest() // dispatched right below
	if len(os.Args) >= 3 && os.Args[1] == "internal" {
		switch os.Args[2] {
		case "supervise":
			os.Exit(Supervise(os.Args[3:]))
		case "apply":
			os.Exit(Apply(os.Args[3:]))
		case "docker-proxy":
			os.Exit(DockerProxy(os.Args[3:]))
		case "cache-trim":
			os.Exit(CacheTrim(os.Args[3:]))
		}
		os.Exit(2)
	}
	if len(os.Args) >= 3 && os.Args[1] == "helper" {
		os.Exit(helper(os.Args[2], os.Args[3:]))
	}
	code := m.Run()
	if os.Getenv("KRAN_TEST_DOCKER") == "1" && os.Getenv("KRAN_TEST_KEEP_IMAGE") != "1" {
		// The runner image is the only Docker resource that outlives a
		// test; drop it so test runs leave nothing behind.
		exec.Command(dockerCLIForTests(), "image", "rm", "--force", "kran-test-runner:dev", runnerTestNS+"-runner:dev").Run()
	}
	if applyBin.path != "" {
		os.RemoveAll(filepath.Dir(applyBin.path))
	}
	os.Exit(code)
}

func helper(mode string, args []string) int {
	switch mode {
	case "sleeper": // write pid, then sleep
		os.WriteFile(args[0], []byte(strconv.Itoa(os.Getpid())), 0o644)
		time.Sleep(time.Hour)
	case "escapee": // start a sleeper in a new session and exit (orphaning it)
		self, _ := os.Executable()
		cmd := exec.Command(self, "helper", "sleeper", args[0])
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		for i := 0; i < 200; i++ { // wait until it wrote its pid
			if _, err := os.Stat(args[0]); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	case "alloc": // grow RSS by 8 MiB steps up to N MiB, then sleep
		n, _ := strconv.Atoi(args[0])
		var keep [][]byte
		for i := 0; i < n/8; i++ {
			b := make([]byte, 8<<20)
			for j := 0; j < len(b); j += 4096 {
				b[j] = 1
			}
			keep = append(keep, b)
			time.Sleep(5 * time.Millisecond)
		}
		fmt.Println("allocated", len(keep)*8, "MiB")
		time.Sleep(time.Hour)
	case "lines": // print n numbered lines, delay ms apart
		n, _ := strconv.Atoi(args[0])
		d, _ := strconv.Atoi(args[1])
		for i := 0; i < n; i++ {
			fmt.Printf("line %04d\n", i)
			time.Sleep(time.Duration(d) * time.Millisecond)
		}
	case "pwd":
		wd, _ := os.Getwd()
		fmt.Println(wd)
	default:
		return 2
	}
	return 0
}

func testBinary(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return self
}

func readPID(t *testing.T, file string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(file); err == nil && len(b) > 0 {
			pid, _ := strconv.Atoi(string(b))
			return pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no pid in %s", filepath.Base(file))
	return 0
}
