package daemon

import (
	"net/http"
)

// placeholderUI stands in for internal/ui.Handler() (package D) until the
// lead wires it: production run() passes ui.Handler() as Deps.UI.
func placeholderUI() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<!doctype html><html><head><meta charset="utf-8"><title>kran</title>
<meta name="viewport" content="width=device-width,initial-scale=1"></head>
<body style="font-family:system-ui,sans-serif;margin:2rem;max-width:40rem">
<h1>kran</h1><p>The web dashboard is not built into this binary.
The API is available at <a href="/api/v1/state">/api/v1/state</a>; use <code>kran status</code> or <code>kran top</code>.</p>
</body></html>`))
	})
}
