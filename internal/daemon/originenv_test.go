package daemon

import (
	"errors"
	"testing"

	"github.com/hafencity-dev/kran/internal/proto"
)

func TestOriginEnv(t *testing.T) {
	t.Setenv("TZ", "Europe/Lisbon")
	oe := originEnv([]proto.Runtime{
		{Kind: proto.RuntimeNative, Toolchains: map[string]string{"go": "1.25.3", "node": "24.8.0"}},
		{Kind: proto.RuntimeContainer, Toolchains: map[string]string{"mise": "auto"}},
	})
	if oe.TZ != "Europe/Lisbon" || oe.Toolchains["go"] != "1.25.3" || oe.Toolchains["mise"] != "" || oe.User == "" || oe.Home == "" {
		t.Errorf("origin env %+v", oe)
	}
}

func TestLocalTZ(t *testing.T) {
	for env, want := range map[string]string{":Europe/Berlin": "Europe/Berlin", "/usr/share/zoneinfo/Asia/Tokyo": "Asia/Tokyo", "/etc/custom": "", "UTC": "UTC"} {
		t.Setenv("TZ", env)
		if got := localTZ(); got != want {
			t.Errorf("TZ=%q: %q, want %q", env, got, want)
		}
	}
	for link, want := range map[string]string{"/usr/share/zoneinfo/Europe/Berlin": "Europe/Berlin",
		"/var/db/timezone/zoneinfo/America/New_York": "America/New_York", "/etc/zone": ""} {
		if got := zoneFromLink(link, nil); got != want {
			t.Errorf("link %q: %q, want %q", link, got, want)
		}
	}
	if zoneFromLink("/usr/share/zoneinfo/UTC", errors.New("no link")) != "" {
		t.Error("error ignored")
	}
}
