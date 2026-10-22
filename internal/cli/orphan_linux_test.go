package cli

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Even without the watcher, the job's leader gets a parent-death signal
// (forked from a thread that lives as long as kran run).
func TestDeathSignalKillsLeader(t *testing.T) {
	pgid, kill := startStandaloneHelper(t, "KRAN_TEST_NO_WATCHER=1")
	kill()
	deadline := time.Now().Add(3 * time.Second)
	for {
		// The leader is not our child: a zombie counts as dead.
		if syscall.Kill(pgid, 0) != nil || isZombie(pgid) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the job's leader outlived its SIGKILLed kran run")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func isZombie(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i > 0 && i+2 < len(s) && s[i+2] == 'Z'
}
