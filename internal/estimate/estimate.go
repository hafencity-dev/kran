// Package estimate computes job fingerprints and learns resource estimates
// (cpu, peak memory, duration) and per-node speed from finished jobs.
// SPEC §6.6.
package estimate

import (
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/store"
)

type Estimator interface {
	Fingerprint(spec *proto.JobSpec) string
	// Estimate returns the estimate for a job, honoring explicit
	// requirements (spec.Req.CPUs/Mem) and the node's CPU count for family
	// defaults.
	Estimate(spec *proto.JobSpec, fingerprint string, nodeCPUs int) proto.Estimate
	// Speed is the learned duration multiplier of node relative to origin
	// for this fingerprint (1 when unknown).
	Speed(fingerprint, node string) float64
	// Observe records a finished job.
	Observe(job *proto.Job)
	// Family returns the command family name ("go-test", "next-build", …, "other").
	Family(argv []string) string
}

func New(st store.Store) Estimator { return newEstimator(st) }
