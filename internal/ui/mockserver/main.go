// Command mockserver runs a simulated kran daemon API for UI development:
//
//	go run ./internal/ui/mockserver                 # API + built UI on :7271
//	go run ./internal/ui/mockserver -nodes 10 -jobs 200   # load test
//	go run ./internal/ui/mockserver -top            # kran top against the simulation
//
// The Vite dev server (cd web && npm run dev) proxies /api here.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/hafencity-dev/kran/internal/tui"
	"github.com/hafencity-dev/kran/internal/ui"
	"github.com/hafencity-dev/kran/internal/ui/mock"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7271", "listen address")
	token := flag.String("token", "", "require this token (as a non-loopback UI would)")
	nodes := flag.Int("nodes", 3, "number of nodes (3 = gx10, devmac, e2e; more adds build nodes)")
	jobs := flag.Int("jobs", 0, "active jobs to sustain (0 = realistic)")
	seed := flag.Uint64("seed", 7, "simulation seed")
	top := flag.Bool("top", false, "run kran top against the simulation instead of serving HTTP")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sim := mock.New(mock.Options{Seed: *seed, ExtraNodes: max(0, *nodes-3), TargetActive: *jobs})
	go sim.Run(ctx.Done())

	if *top {
		if err := tui.Run(ctx, mock.Client{S: sim}); err != nil {
			fmt.Fprintln(os.Stderr, "kran top:", err)
			os.Exit(1)
		}
		return
	}

	api := sim.Handler(*token)
	assets := ui.Handler()
	srv := &http.Server{
		Addr: *addr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				api.ServeHTTP(w, r)
				return
			}
			assets.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	url := "http://" + *addr + "/"
	if *token != "" {
		url += "#token=" + *token
	}
	log.Printf("mock kran daemon on %s", url)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
