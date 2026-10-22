package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// sentInput is the stdin payload sent so far, and the resume answers.
func sentInput(s *fakeSession) (string, []int64, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b bytes.Buffer
	var resumes []int64
	eofs := 0
	for _, f := range s.sent {
		switch f.t {
		case proto.FrameStdin:
			b.Write(f.payload)
		case proto.FrameStdinResume:
			var r proto.StdinResume
			json.Unmarshal(f.payload, &r)
			resumes = append(resumes, r.Offset)
		case proto.FrameStdinEOF:
			eofs++
		}
	}
	return b.String(), resumes, eofs
}

func ptr(v int64) *int64 { return &v }

// kran run reads its stdin only within the daemon's credit, keeps what the
// job has not consumed and sends it again from a Resume.
func TestStdinCreditAndResume(t *testing.T) {
	pr, pw, _ := os.Pipe()
	s := newFakeSession()
	h := newHarness(t, &fakeClient{sessions: []*fakeSession{s}}, "cat")
	h.r.flags.Stdin = true
	h.r.stdin = pr
	input := "0123456789abcdefghijklmnopqrstuvwxyz"
	go func() {
		s.push(proto.FrameDecision, proto.Decision{JobID: "j", Mode: "remote", Node: "n", StdinLimit: 10})
		s.push(proto.FramePhase, proto.Phase{State: proto.JobRunning})
		pw.Write([]byte(input))
		waitFor(t, func() bool { got, _, _ := sentInput(s); return got == input[:10] })
		time.Sleep(100 * time.Millisecond)
		if got, _, _ := sentInput(s); got != input[:10] {
			t.Errorf("sent beyond the credit: %q", got)
		}
		s.push(proto.FrameStdinCredit, proto.StdinCredit{Consumed: 4, Limit: 20})
		waitFor(t, func() bool { got, _, _ := sentInput(s); return got == input[:20] })
		// A broken stream: the target holds 12 bytes; send again from there.
		s.push(proto.FrameStdinCredit, proto.StdinCredit{Consumed: 6, Limit: 26, Resume: ptr(12)})
		waitFor(t, func() bool { got, _, _ := sentInput(s); return got == input[:20]+input[12:26] })
		pw.Close()
		s.push(proto.FrameStdinCredit, proto.StdinCredit{Consumed: 26, Limit: 100})
		waitFor(t, func() bool { _, _, eofs := sentInput(s); return eofs == 1 })
		s.push(proto.FrameExit, proto.Exit{JobID: "j", Code: 0, State: proto.JobSucceeded})
	}()
	if code := h.run(t); code != 0 {
		t.Fatal(code, h.stderr.String())
	}
	got, resumes, eofs := sentInput(s)
	if got != input[:20]+input[12:] || len(resumes) != 1 || resumes[0] != 12 || eofs != 1 {
		t.Fatalf("sent %q resumes %v eofs %d", got, resumes, eofs)
	}
}

// Input is read only once the remote command runs: when the job falls back
// to running here, the local child still finds all of it.
func TestStdinUntouchedUntilRemoteStart(t *testing.T) {
	pr, pw, _ := os.Pipe()
	s := newFakeSession()
	h := newHarness(t, &fakeClient{sessions: []*fakeSession{s}}, "cat")
	h.r.flags.Stdin = true
	h.r.stdin = pr
	h.r.stdio.In = pr
	pw.Write([]byte("all of it\n"))
	pw.Close()
	go func() {
		s.push(proto.FrameDecision, proto.Decision{JobID: "j", Mode: "remote", Node: "n", StdinLimit: 1 << 20})
		time.Sleep(200 * time.Millisecond) // syncing
		s.push(proto.FrameNotice, proto.Notice{Kind: "fallback", Message: "n failed before the job started"})
		s.push(proto.FrameDecision, proto.Decision{JobID: "j", Mode: "local", Envelope: &proto.LocalEnvelope{}})
	}()
	if code := h.run(t); code != 0 {
		t.Fatal(code, h.stderr.String())
	}
	if out := localOut(t, h); !strings.Contains(out, "all of it") {
		t.Fatalf("the local child got %q", out)
	}
}

// Input the job consumed is forgotten; a Resume before it is answered as
// lost (the daemon then closes the job's input and says so).
func TestStdinResumeOfForgottenInputIsLost(t *testing.T) {
	pr, pw, _ := os.Pipe()
	defer pw.Close()
	s := newFakeSession()
	h := newHarness(t, &fakeClient{sessions: []*fakeSession{s}}, "cat")
	h.r.flags.Stdin = true
	h.r.stdin = pr
	go func() {
		s.push(proto.FrameDecision, proto.Decision{JobID: "j", Mode: "remote", Node: "n", StdinLimit: 100})
		s.push(proto.FramePhase, proto.Phase{State: proto.JobRunning})
		pw.Write([]byte("0123456789"))
		waitFor(t, func() bool { got, _, _ := sentInput(s); return got == "0123456789" })
		s.push(proto.FrameStdinCredit, proto.StdinCredit{Consumed: 8, Limit: 108, Resume: ptr(5)})
		waitFor(t, func() bool { return s.count(proto.FrameStdinResume) == 1 })
		s.push(proto.FrameExit, proto.Exit{JobID: "j", Code: 0, State: proto.JobSucceeded})
	}()
	h.run(t)
	f, _ := s.find(proto.FrameStdinResume)
	var r proto.StdinResume
	json.Unmarshal(f.payload, &r)
	if !r.Lost {
		t.Fatalf("%+v", r)
	}
}
