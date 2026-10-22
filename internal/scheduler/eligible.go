package scheduler

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// whyKind classifies why a node cannot take a job, so the queue reason can
// name the binding constraint instead of listing every node.
type whyKind int

const (
	whyPolicy    whyKind = iota // the job's own flags exclude the node; never shown
	whyStatus                   // offline, draining, not accepting, degraded
	whyFailed                   // this job failed to start there before
	whyBreaker                  // repeated start failures
	whyGPU                      // no suitable GPU
	whyRuntime                  // no runtime for the job's OS
	whyToolchain                // missing toolchain
	whyDisk                     // disk floor
	whyUnknown                  // node state not known yet
)

type ineligible struct {
	node   string
	kind   whyKind
	reason string // "devmac: draining"
}

// originOnly reports whether the job may run only on the origin.
func (j *sjob) originOnly() bool {
	return j.req.Prefer == proto.PreferLocal || j.req.OriginOnly
}

// policyAllows applies the job's own placement flags to a node.
func (s *sched) policyAllows(j *sjob, node string) bool {
	self := node == s.opts.Origin
	switch {
	case j.originOnly() && !self:
		return false
	case j.req.Prefer == proto.PreferRemote && self:
		return false
	case j.req.Node != "" && j.req.Node != node:
		return false
	case !self && j.rejects >= s.opts.MaxRemoteAttempts:
		return false // remote attempts used up: fall back to origin
	case self && j.rejects > 0 && j.noFallback && !j.originOnly():
		return false
	}
	return true
}

func (s *sched) originOS() string {
	if e := s.nodes[s.opts.Origin]; e != nil && e.st.Info.OS != "" {
		return e.st.Info.OS
	}
	return ""
}

func (s *sched) gpuWord() string {
	if s.originOS() == "darwin" {
		return "a GPU (NVIDIA, or Apple for macOS jobs)"
	}
	return "an NVIDIA GPU"
}

// gpuMatch: NVIDIA anywhere; an Apple GPU only serves jobs from a Mac
// (they run natively on macOS).
func (s *sched) gpuMatch(e *nodeEntry) bool {
	for _, g := range e.st.Info.GPUs {
		if g.Vendor == "nvidia" || (g.Vendor == "apple" && s.originOS() == "darwin") {
			return true
		}
	}
	return false
}

func (s *sched) breakerReason(e *nodeEntry, now time.Time) string {
	b := e.brk
	if b.fails < breakerThreshold || !now.Before(b.openUntil) {
		return ""
	}
	return fmt.Sprintf("%s: %d failed starts in a row (last: %s), retrying in %s",
		e.st.Info.Name, b.fails, b.lastErr, fmtDur(b.openUntil.Sub(now).Seconds()))
}

// eligible returns the runtimes of v that may run j, or why none may.
func (s *sched) eligible(j *sjob, v *nodeView, now time.Time) ([]rtView, *ineligible) {
	name := v.name
	st := v.e.st
	no := func(k whyKind, format string, args ...any) ([]rtView, *ineligible) {
		return nil, &ineligible{node: name, kind: k, reason: name + ": " + fmt.Sprintf(format, args...)}
	}
	if !s.policyAllows(j, name) {
		return no(whyPolicy, "excluded by the job's flags")
	}
	fallback := v.self && j.rejects > 0
	if v.self {
		if st.Info.CPUs == 0 && st.Info.MemTotal == 0 {
			return no(whyUnknown, "this machine's resources are not known yet")
		}
		if st.Status == proto.NodeDraining && !j.originOnly() && !fallback && j.req.Node != name {
			return no(whyStatus, "draining")
		}
	} else {
		switch st.Status {
		case proto.NodeOffline:
			if st.Reason != "" {
				return no(whyStatus, "offline (%s)", st.Reason)
			}
			return no(whyStatus, "offline")
		case proto.NodeDraining:
			return no(whyStatus, "draining")
		case proto.NodeDegraded:
			if st.Reason != "" {
				return no(whyStatus, "%s", strings.TrimPrefix(st.Reason, name+": "))
			}
			return no(whyStatus, "degraded")
		}
		if !st.Accepts {
			return no(whyStatus, "not accepting jobs (accept.enabled is off there)")
		}
		if st.Info.CPUs == 0 && st.Info.MemTotal == 0 {
			return no(whyUnknown, "resources not known yet")
		}
	}
	if msg, bad := j.excluded[name]; bad {
		return no(whyFailed, "failed to start this job (%s)", msg)
	}
	if !v.self {
		if r := s.breakerReason(v.e, now); r != "" {
			return nil, &ineligible{node: name, kind: whyBreaker, reason: r}
		}
	}
	if j.req.GPU && !s.gpuMatch(v.e) {
		return no(whyGPU, "no %s", strings.TrimPrefix(strings.TrimPrefix(s.gpuWord(), "a "), "an "))
	}

	// Runtimes: the job's OS policy, then toolchains.
	want := s.originOS()
	osAny := j.req.OS == proto.OSAny
	var byOS []rtView
	var unavailable []rtView
	for _, rv := range v.rts {
		if !osAny && want != "" && rv.rt.OS != want {
			continue
		}
		if j.req.GPU && rv.rt.Kind == proto.RuntimeContainer && !s.hasNvidia(v.e) {
			continue // an Apple GPU is not reachable from a Linux container
		}
		if !rv.rt.Available {
			unavailable = append(unavailable, rv)
			continue
		}
		byOS = append(byOS, rv)
	}
	if len(byOS) == 0 {
		osName := want
		if osName == "" || osAny {
			osName = "usable"
		}
		if len(unavailable) > 0 && unavailable[0].rt.Reason != "" {
			return no(whyRuntime, "no %s runtime (%s)", osName, unavailable[0].rt.Reason)
		}
		if len(unavailable) > 0 {
			return no(whyRuntime, "no %s runtime (%s runtime unavailable)", osName, unavailable[0].rt.Kind)
		}
		return no(whyRuntime, "no %s runtime", osName)
	}
	var ok []rtView
	missing := ""
	for _, rv := range byOS {
		if v.self {
			ok = append(ok, rv) // the user's own machine is what they would use anyway
			continue
		}
		if m := missingToolchain(rv.rt, j.req.Toolchains); m != "" {
			if missing == "" {
				missing = m
			}
			continue
		}
		ok = append(ok, rv)
	}
	if len(ok) == 0 {
		return no(whyToolchain, "missing toolchain %s", missing)
	}
	if !v.self {
		m := st.Metrics
		floor := max(10*gib, st.Info.DiskTotal/20)
		if !m.Time.IsZero() && m.DiskFree > 0 && m.DiskFree < floor {
			return no(whyDisk, "disk almost full (%s free)", fmtBytes(m.DiskFree))
		}
	}
	// Prefer native over container when both may run the job.
	sort.SliceStable(ok, func(a, b int) bool {
		return ok[a].rt.Kind == proto.RuntimeNative && ok[b].rt.Kind != proto.RuntimeNative
	})
	return ok, nil
}

func (s *sched) hasNvidia(e *nodeEntry) bool {
	for _, g := range e.st.Info.GPUs {
		if g.Vendor == "nvidia" {
			return true
		}
	}
	return false
}

// missingToolchain returns "tool@version" for the first pinned toolchain the
// runtime cannot provide, or "". The container runtime installs any
// toolchain through mise; Go fetches its own toolchain (GOTOOLCHAIN=auto);
// a native runtime with mise can install the rest. A runtime that reports
// no toolchains at all is assumed capable (not probed yet).
func missingToolchain(rt proto.Runtime, need map[string]string) string {
	if len(need) == 0 || rt.Kind == proto.RuntimeContainer || rt.Toolchains == nil {
		return ""
	}
	if rt.Toolchains["mise"] != "" {
		return ""
	}
	tools := make([]string, 0, len(need))
	for t := range need {
		tools = append(tools, t)
	}
	sort.Strings(tools)
	for _, tool := range tools {
		want := need[tool]
		have := rt.Toolchains[tool]
		if tool == "go" && have != "" {
			continue
		}
		if have == "" || !versionSatisfies(have, want) {
			if want == "" {
				return tool
			}
			return tool + "@" + want
		}
	}
	return ""
}

// versionSatisfies checks an installed version against a pin: "24" or
// "24.8" match by prefix components; ">=20", "^20.1", "~20.1" and "20.x" are
// understood loosely; anything else is accepted.
func versionSatisfies(have, want string) bool {
	want = strings.TrimSpace(want)
	if want == "" || want == "*" || want == "latest" || want == "lts" {
		return true
	}
	hv := versionParts(have)
	if len(hv) == 0 {
		return false
	}
	op := ""
	for _, p := range []string{">=", "^", "~", ">", "="} {
		if strings.HasPrefix(want, p) {
			op, want = p, strings.TrimSpace(strings.TrimPrefix(want, p))
			break
		}
	}
	wv := versionParts(strings.TrimSuffix(strings.TrimSuffix(want, ".x"), ".*"))
	if len(wv) == 0 {
		return true
	}
	cmp := func(n int) int {
		for i := 0; i < n && i < len(wv); i++ {
			h := 0
			if i < len(hv) {
				h = hv[i]
			}
			if h != wv[i] {
				if h < wv[i] {
					return -1
				}
				return 1
			}
		}
		return 0
	}
	switch op {
	case ">=":
		return cmp(len(wv)) >= 0
	case ">":
		return cmp(len(wv)) > 0
	case "^":
		return hv[0] == wv[0] && cmp(len(wv)) >= 0
	case "~":
		return cmp(min(2, len(wv))) == 0 && cmp(len(wv)) >= 0
	}
	return cmp(len(wv)) == 0
}

func versionParts(v string) []int {
	v = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(v), "v"), "go")
	var out []int
	for _, p := range strings.Split(v, ".") {
		end := 0
		for end < len(p) && p[end] >= '0' && p[end] <= '9' {
			end++
		}
		if end == 0 {
			break
		}
		n, _ := strconv.Atoi(p[:end])
		out = append(out, n)
		if end < len(p) {
			break // "1.25rc1"
		}
	}
	return out
}
