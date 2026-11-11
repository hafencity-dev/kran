package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

func TestLinuxRuntimeAdvice(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "bin", "0.1.10-6-gc9403ef", "linux-arm64", "kran")
	line := linuxRuntimeAdvice(bin, "0.1.10-6-gc9403ef")
	if !strings.HasPrefix(line, "kran: ") || !strings.Contains(line, "its linux runtime for 0.1.10-6-gc9403ef is missing at "+bin) {
		t.Fatalf("line %q", line)
	}
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if line := linuxRuntimeAdvice(bin, "0.1.10-6-gc9403ef"); line != "" {
		t.Fatalf("in place: want silence, got %q", line)
	}
	if line := linuxRuntimeAdvice("", "0.1.10"); line != "" {
		t.Fatalf("a Linux node needs none: %q", line)
	}
}

func problemState() *proto.State {
	return &proto.State{Nodes: []proto.NodeState{{
		Status: proto.NodeOnline, Accepts: true,
		Info: proto.NodeInfo{Name: "devmac", CPUs: 8, Runtimes: []proto.Runtime{
			{Kind: proto.RuntimeNative, Available: true},
			{Kind: proto.RuntimeContainer, OS: "linux", Reason: "its linux runtime for 1.2.3 is missing at /c/kran; run `kran upgrade`",
				Problem: &proto.RuntimeProblem{Code: proto.ProblemLinuxBinaryMissing, Version: "1.2.3", Path: "/c/kran"}},
		}},
	}}}
}

func TestStatusShowsRuntimeProblem(t *testing.T) {
	var out bytes.Buffer
	a := &App{Out: &out, Err: &out}
	a.printStatus(problemState(), time.Now())
	got := out.String()
	if !strings.Contains(got, "online") || !strings.Contains(got, "no container jobs") {
		t.Errorf("node row must stay online and say it takes no container jobs:\n%s", got)
	}
	if !strings.Contains(got, "devmac cannot run container jobs: its linux runtime for 1.2.3 is missing at /c/kran") {
		t.Errorf("cause line missing:\n%s", got)
	}
}

// A node whose container jobs run in a VM shows its two pools apart
// (DEVMAC-UNDERUSE-20261111: "670.7 MiB of 96.0 GiB mem free" hid 32 GiB in
// the VM).
func TestStatusShowsVMPoolApart(t *testing.T) {
	n := proto.NodeState{
		Status: proto.NodeOnline, Accepts: true, Running: 1,
		Info: proto.NodeInfo{Name: "devmac", OS: "darwin", CPUs: 28, MemTotal: 96 << 30, Runtimes: []proto.Runtime{
			{Kind: proto.RuntimeNative, OS: "darwin", Available: true},
			{Kind: proto.RuntimeContainer, OS: "linux", CPUs: 24, Mem: 41971261440, Available: true},
		}},
		Free:   proto.Resources{CPUs: 22, Mem: 670 << 20},
		FreeVM: &proto.Resources{CPUs: 23.5, Mem: 32 << 30},
	}
	var out bytes.Buffer
	a := &App{Out: &out, Err: &out}
	a.printStatus(&proto.State{Nodes: []proto.NodeState{n}}, time.Now())
	want := "host: 22 of 28 cpus, 670.0 MiB of 96.0 GiB mem free · Linux VM: 24 of 24 cpus, 32.0 GiB of 39.1 GiB mem free · 1 running"
	if !strings.Contains(out.String(), want) {
		t.Errorf("want %q in\n%s", want, out.String())
	}
	// A Linux node keeps the one line.
	n.Info.OS, n.FreeVM = "linux", nil
	out.Reset()
	a.printStatus(&proto.State{Nodes: []proto.NodeState{n}}, time.Now())
	if s := out.String(); strings.Contains(s, "Linux VM") || !strings.Contains(s, "22 of 28 cpus free · 670.0 MiB of 96.0 GiB mem free · 1 running") {
		t.Errorf("linux row:\n%s", s)
	}
}

func TestStatusJSONCarriesRuntimeProblem(t *testing.T) {
	b, err := json.Marshal(problemState())
	if err != nil {
		t.Fatal(err)
	}
	var back proto.State
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	p := back.Nodes[0].Info.Runtimes[1].Problem
	if p == nil || p.Code != "linux_binary_missing" || p.Path != "/c/kran" {
		t.Fatalf("problem lost in JSON: %s", b)
	}
	if !strings.Contains(string(b), `"problem":{"code":"linux_binary_missing"`) {
		t.Errorf("field name is public API: %s", b)
	}
	// A healthy runtime adds nothing.
	b, _ = json.Marshal(proto.Runtime{Kind: proto.RuntimeNative, Available: true})
	if strings.Contains(string(b), "problem") {
		t.Errorf("omitempty: %s", b)
	}
}
