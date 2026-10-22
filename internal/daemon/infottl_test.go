package daemon

import (
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// An unavailable runtime is usually fixed from outside (Docker started, the
// Linux binary pushed by kran link): it is probed again soon, not in 10 min.
func TestInfoTTLShortWhileARuntimeIsUnavailable(t *testing.T) {
	ok := proto.Runtime{Kind: proto.RuntimeNative, Available: true}
	down := proto.Runtime{Kind: proto.RuntimeContainer, Reason: "Linux kran binary is missing"}
	if got := infoTTL([]proto.Runtime{ok}); got != 10*time.Minute {
		t.Errorf("all available: %v", got)
	}
	if got := infoTTL([]proto.Runtime{ok, down}); got != 30*time.Second {
		t.Errorf("one unavailable: %v", got)
	}
}
