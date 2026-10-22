package execenv

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"path"
	"regexp"
	"strings"
)

// The way back (SPEC §6.8): what Docker reports about mounts carries the
// Docker host's paths the proxy wrote into the request. A job that
// inspects its own containers ("is the store mounted?") must see the paths
// it used, so the proxy maps them back in the answers that show bind
// sources. Paths outside the job's maps pass unchanged.

var (
	inspectContainerRe = regexp.MustCompile(`^(/v[0-9.]+)?/containers/[^/]+/json$`)
	listContainersRe   = regexp.MustCompile(`^(/v[0-9.]+)?/containers/json$`)
	inspectVolumeRe    = regexp.MustCompile(`^(/v[0-9.]+)?/volumes/[^/]+$`)
	listVolumesRe      = regexp.MustCompile(`^(/v[0-9.]+)?/volumes$`)
)

// maxRewriteResponse bounds the answers the proxy parses; a bigger one
// (thousands of containers) streams through unchanged.
const maxRewriteResponse = 32 << 20

// rewritesResponse reports whether the answer to req may carry bind
// sources to map back.
func rewritesResponse(maps []dockerMap, req *http.Request) bool {
	if req.Method != http.MethodGet || len(maps) == 0 {
		return false
	}
	p := req.URL.Path
	return inspectContainerRe.MatchString(p) || listContainersRe.MatchString(p) ||
		inspectVolumeRe.MatchString(p) || listVolumesRe.MatchString(p)
}

// unmapHostPath is mapHostPath backwards: a Docker host path under one of
// the maps becomes the job's path.
func unmapHostPath(maps []dockerMap, src string) (string, bool) {
	if !path.IsAbs(src) {
		return src, false
	}
	p := path.Clean(src)
	best := -1
	for i, m := range maps {
		if (p == m.Host || strings.HasPrefix(p, m.Host+"/")) && (best < 0 || len(m.Host) > len(maps[best].Host)) {
			best = i
		}
	}
	if best < 0 {
		return src, false
	}
	m := maps[best]
	if p == m.Host {
		return m.Ctr, true
	}
	return m.Ctr + "/" + strings.TrimPrefix(p, m.Host+"/"), true
}

// unmapBind maps the source of "src:dst[:opts]" back.
func unmapBind(maps []dockerMap, bind string) string {
	src, rest, ok := strings.Cut(bind, ":")
	if !ok {
		return bind
	}
	if c, ok := unmapHostPath(maps, src); ok {
		return c + ":" + rest
	}
	return bind
}

// unmapJSON walks a decoded answer and maps back, in place: "Source" of
// objects whose "Type" is "bind", every entry of "Binds", and "device" of
// "Options"/"DriverOpts" of local volumes bound to a directory. It
// reports whether anything changed.
func unmapJSON(maps []dockerMap, v any) bool {
	changed := false
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			changed = unmapJSON(maps, e) || changed
		}
	case map[string]any:
		if typ, _ := t["Type"].(string); typ == "bind" {
			if s, ok := t["Source"].(string); ok {
				if c, ok := unmapHostPath(maps, s); ok {
					t["Source"], changed = c, true
				}
			}
		}
		if binds, ok := t["Binds"].([]any); ok {
			for i, b := range binds {
				if s, ok := b.(string); ok {
					if nb := unmapBind(maps, s); nb != s {
						binds[i], changed = nb, true
					}
				}
			}
		}
		for _, k := range []string{"Options", "DriverOpts"} {
			opts, ok := t[k].(map[string]any)
			if !ok {
				continue
			}
			o, _ := opts["o"].(string)
			dev, _ := opts["device"].(string)
			if dev == "" || !strings.Contains(","+o+",", ",bind,") && !strings.Contains(","+o+",", ",rbind,") {
				continue
			}
			if c, ok := unmapHostPath(maps, dev); ok {
				opts["device"], changed = c, true
			}
		}
		for k, e := range t {
			if k == "Binds" {
				continue
			}
			if _, ok := e.(map[string]any); ok {
				changed = unmapJSON(maps, e) || changed
			} else if _, ok := e.([]any); ok {
				changed = unmapJSON(maps, e) || changed
			}
		}
	}
	return changed
}

// rewriteResponse maps bind sources in a JSON answer back. The body is
// read up to maxRewriteResponse; a bigger or unparsable one goes to the
// client unchanged (what was read first, then the rest, streamed).
func rewriteResponse(maps []dockerMap, resp *http.Response) {
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "json") || resp.Body == nil {
		return
	}
	head, err := io.ReadAll(io.LimitReader(resp.Body, maxRewriteResponse+1))
	if err != nil || len(head) > maxRewriteResponse {
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(head), resp.Body), resp.Body}
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
		return
	}
	resp.Body.Close()
	body := head
	d := json.NewDecoder(bytes.NewReader(head))
	d.UseNumber()
	var v any
	if d.Decode(&v) == nil && unmapJSON(maps, v) {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if enc.Encode(v) == nil {
			body = buf.Bytes()
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.TransferEncoding = nil
	resp.Header.Del("Content-Length")
}
