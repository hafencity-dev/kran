//go:build unix

package transport

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEnsureDaemonSpawnsOnce(t *testing.T) {
	old := serviceGrace
	serviceGrace = 50 * time.Millisecond
	defer func() { serviceGrace = old }()
	dir := shortDir(t)
	sock := filepath.Join(dir, "p.sock")
	lock := filepath.Join(dir, "start.lock")
	var spawns atomic.Int32
	var mu sync.Mutex
	var ln net.Listener
	spawn := func() (func() error, error) {
		spawns.Add(1)
		stop := make(chan struct{})
		go func() {
			time.Sleep(200 * time.Millisecond) // daemon start-up time
			l, err := ListenUnix(sock)
			if err != nil {
				return
			}
			mu.Lock()
			ln = l
			mu.Unlock()
			go func() {
				for {
					c, err := l.Accept()
					if err != nil {
						return
					}
					c.Close()
				}
			}()
		}()
		return func() error { <-stop; return nil }, nil
	}
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- ensureDaemon(context.Background(), sock, lock, spawn)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := spawns.Load(); n != 1 {
		t.Fatalf("spawned %d daemons, want 1", n)
	}
	// Already running: no spawn.
	if err := ensureDaemon(context.Background(), sock, lock, spawn); err != nil || spawns.Load() != 1 {
		t.Fatalf("err %v spawns %d", err, spawns.Load())
	}
	mu.Lock()
	ln.Close()
	mu.Unlock()
}

func TestEnsureDaemonReportsEarlyExit(t *testing.T) {
	old := serviceGrace
	serviceGrace = 10 * time.Millisecond
	defer func() { serviceGrace = old }()
	dir := shortDir(t)
	err := ensureDaemon(context.Background(), filepath.Join(dir, "p.sock"), filepath.Join(dir, "l"), func() (func() error, error) {
		return func() error { return nil }, nil
	})
	if err == nil || !strings.Contains(err.Error(), "stopped right after starting") {
		t.Fatalf("err = %v", err)
	}
}
