package cli

import (
	"testing"

	"github.com/hafencity-dev/kran/internal/link"
)

func TestSetupOffersLinkingByDefaultOnlyWithRealCandidates(t *testing.T) {
	cases := []struct {
		name  string
		cands []link.Candidate
		want  int
	}{
		{"none", nil, 0},
		{"known_hosts only", []link.Candidate{{Source: "known-hosts", Host: "old"}}, 0},
		{"already linked", []link.Candidate{{Source: "ssh-config", Host: "devmac", Linked: true}}, 0},
		{"phones do not count", []link.Candidate{{Source: "tailscale", Host: "phone", Detail: "iOS, online"}}, 0},
		{"ssh and tailscale", []link.Candidate{
			{Source: "ssh-config", Host: "devmac"},
			{Source: "tailscale", Host: "gx10.tail.ts.net", Detail: "Linux, online"},
			{Source: "known-hosts", Host: "github.com"},
		}, 2},
	}
	for _, c := range cases {
		if got := linkableCandidates(c.cands); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}
