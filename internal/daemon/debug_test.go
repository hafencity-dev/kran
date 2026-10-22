package daemon

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDebugGoroutinesOnlyWhenEnabled(t *testing.T) {
	for _, tc := range []struct {
		debug, unix bool
		want        int
	}{{true, true, 200}, {false, true, 404}, {true, false, 404}} {
		d := &Daemon{deps: Deps{Debug: tc.debug}}
		rec := httptest.NewRecorder()
		d.routes(tc.unix).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/debug/goroutines", nil))
		if rec.Code != tc.want {
			t.Errorf("debug=%v unix=%v: status %d", tc.debug, tc.unix, rec.Code)
		}
		if tc.want == 200 && !strings.HasPrefix(rec.Body.String(), "goroutine profile: total ") {
			t.Errorf("body %q", rec.Body.String()[:min(80, rec.Body.Len())])
		}
		rec = httptest.NewRecorder()
		d.routes(tc.unix).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/debug/slots", nil))
		if rec.Code != tc.want || tc.want == 200 && strings.TrimSpace(rec.Body.String()) != `{"busy":[]}` {
			t.Errorf("slots debug=%v unix=%v: %d %q", tc.debug, tc.unix, rec.Code, rec.Body.String())
		}
	}
}

func TestNamespaceFromEnv(t *testing.T) {
	for _, tc := range []struct{ env, ns, slice string }{
		{"", "", ""}, {"kran-dev", "kran-dev", "kran_dev.slice"}, {"Bad NS", "", ""}, {"kran;rm", "", ""},
	} {
		t.Setenv("KRAN_NAMESPACE", tc.env)
		if ns, slice := namespace(t.Logf); ns != tc.ns || slice != tc.slice {
			t.Errorf("%q: %q %q", tc.env, ns, slice)
		}
	}
}
