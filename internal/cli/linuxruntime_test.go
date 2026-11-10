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
