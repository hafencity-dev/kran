package execenv

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The job's Docker socket (SPEC §6.8). A container job sees its workspace
// at the origin's path, but the Docker daemon resolves bind mounts in its
// own file system, where that path does not exist (the slot lives in a
// volume, or elsewhere on the host): `docker run -v "$PWD":/src` would get
// an empty directory. kran-init therefore starts this proxy as the job's
// user on the job's /var/run/docker.sock. It forwards every request
// unchanged, except that the sources of bind mounts (and of local volumes
// bound to a directory) under the job's own mounts — the workspace, its
// home, the caches — are rewritten to where those directories live on the
// Docker host. A job reaches nothing it could not already bind: the
// targets are its own mounts, and the socket is Docker's anyway.

// dockerMap maps a path in the job container to the same directory on the
// Docker host.
type dockerMap struct {
	Ctr  string `json:"ctr"`
	Host string `json:"host"`
}

// encodeDockerMaps renders maps for KRAN_INIT_DOCKER_MAP.
func encodeDockerMaps(maps []dockerMap) string {
	b, _ := json.Marshal(maps)
	return string(b)
}

// parseDockerMaps reads KRAN_INIT_DOCKER_MAP; entries that are not clean
// absolute paths (or map "/") are dropped. Longest container path first,
// so the workspace wins over the home directory that holds it.
func parseDockerMaps(s string) []dockerMap {
	var in []dockerMap
	if s == "" || json.Unmarshal([]byte(s), &in) != nil {
		return nil
	}
	var out []dockerMap
	for _, m := range in {
		if !cleanAbs(m.Ctr) || !cleanAbs(m.Host) || m.Ctr == "/" {
			continue
		}
		out = append(out, m)
	}
	return sortDockerMaps(out)
}

// sortDockerMaps puts the longest container path first.
func sortDockerMaps(maps []dockerMap) []dockerMap {
	sort.SliceStable(maps, func(i, j int) bool { return len(maps[i].Ctr) > len(maps[j].Ctr) })
	return maps
}

// containerMountSource asks Docker (at sock) where the volume mounted at
// dst in container name lives on the Docker host ("" when unknown).
func containerMountSource(sock, name, dst string) string {
	cl := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}
	resp, err := cl.Get("http://docker/containers/" + url.PathEscape(name) + "/json")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var c struct {
		Mounts []struct{ Type, Source, Destination string }
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&c) != nil {
		return ""
	}
	for _, m := range c.Mounts {
		if m.Destination == dst && m.Type == "volume" && cleanAbs(m.Source) && m.Source != "/" {
			return m.Source
		}
	}
	return ""
}

func cleanAbs(p string) bool { return p != "" && path.IsAbs(p) && path.Clean(p) == p }

// mapHostPath translates a bind source; ok is false when no map applies.
func mapHostPath(maps []dockerMap, src string) (string, bool) {
	if !path.IsAbs(src) {
		return src, false // a named volume, or a path Docker rejects anyway
	}
	p := path.Clean(src)
	for _, m := range maps {
		if p == m.Ctr {
			return m.Host, true
		}
		if rest, ok := strings.CutPrefix(p, m.Ctr+"/"); ok {
			return m.Host + "/" + rest, true
		}
	}
	return src, false
}

// rewriteBind translates the source of a `-v src:dst[:opts]` bind.
func rewriteBind(maps []dockerMap, bind string) string {
	src, rest, ok := strings.Cut(bind, ":")
	if !ok {
		return bind // an anonymous volume: just a container path
	}
	if h, ok := mapHostPath(maps, src); ok {
		return h + ":" + rest
	}
	return bind
}

var (
	createContainerRe = regexp.MustCompile(`^(/v[0-9.]+)?/containers/create$`)
	createVolumeRe    = regexp.MustCompile(`^(/v[0-9.]+)?/volumes/create$`)
)

// maxRewriteBody bounds the request bodies the proxy parses.
const maxRewriteBody = 16 << 20

// rewriteCreateContainer rewrites HostConfig.Binds and bind Mounts, and
// puts a container the job creates at the job's own place in the kernel's
// kill order (score > 0, applyOOMScore).
//
// With a score, what reaches Docker is always the body kran read and
// encoded again, never the job's bytes, and a body that could mean
// something else to Docker than to kran is refused (an error here is a 400
// from the proxy): one that is not exactly one JSON object (null, an
// array, a string, trailing data), a HostConfig that is not an object, and
// any spelling of HostConfig, OomScoreAdj or OomKillDisable other than
// Docker's own, or one of them given twice. Docker's decoder matches names
// regardless of case (Go's folding: "oomscoreadj", "OomſcoreAdj") and lets
// a later name or a repeated object win; refusing every other spelling,
// rather than renaming it, leaves nothing that depends on two decoders
// folding alike. Values kran does not rewrite stay raw JSON, so large
// integers and null are passed on as written.
func rewriteCreateContainer(maps []dockerMap, score int, body []byte) ([]byte, bool, error) {
	if score > 0 {
		if err := checkCreateKeys(body); err != nil {
			return nil, false, err
		}
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, false, err
	}
	if top == nil {
		if score > 0 {
			return nil, false, errors.New("the request body is not a JSON object")
		}
		return body, false, nil
	}
	var hc map[string]json.RawMessage
	if raw, ok := top["HostConfig"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if err := json.Unmarshal(raw, &hc); err != nil {
			if score > 0 {
				return nil, false, errors.New("HostConfig is not a JSON object")
			}
			return body, false, nil
		}
	}
	if hc == nil {
		if score <= 0 {
			return body, false, nil
		}
		hc = map[string]json.RawMessage{}
	}
	changed := applyOOMScore(hc, score)
	if raw, ok := hc["Binds"]; ok {
		var binds []string
		if json.Unmarshal(raw, &binds) == nil {
			for i, b := range binds {
				if nb := rewriteBind(maps, b); nb != b {
					binds[i], changed = nb, true
				}
			}
			hc["Binds"], _ = json.Marshal(binds)
		}
	}
	if raw, ok := hc["Mounts"]; ok {
		var mounts []map[string]json.RawMessage
		if json.Unmarshal(raw, &mounts) == nil {
			for _, mt := range mounts {
				var typ, src string
				json.Unmarshal(mt["Type"], &typ)
				if typ != "bind" || json.Unmarshal(mt["Source"], &src) != nil {
					continue
				}
				if h, ok := mapHostPath(maps, src); ok {
					mt["Source"], _ = json.Marshal(h)
					changed = true
				}
			}
			hc["Mounts"], _ = json.Marshal(mounts)
		}
	}
	if !changed && score <= 0 {
		return body, false, nil
	}
	top["HostConfig"], _ = json.Marshal(hc)
	out, err := json.Marshal(top)
	return out, err == nil, err
}

// checkCreateKeys refuses a create body whose meaning to Docker could
// differ from kran's reading (see rewriteCreateContainer): exactly one
// JSON object, HostConfig in Docker's spelling at most once, and in it
// OomScoreAdj and OomKillDisable in Docker's spelling at most once.
func checkCreateKeys(body []byte) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return errors.New("the request body is not a JSON object")
	}
	top, err := scanKeys(dec, "HostConfig")
	if err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("the request body has more after its JSON object")
	}
	if hc := bytes.TrimSpace(top["HostConfig"]); len(hc) > 0 && hc[0] == '{' {
		sub := json.NewDecoder(bytes.NewReader(hc))
		sub.Token()
		if _, err := scanKeys(sub, "OomScoreAdj", "OomKillDisable"); err != nil {
			return err
		}
	}
	return nil
}

// scanKeys reads the rest of an object whose '{' dec has read and returns
// the values of names. A key that Docker would read as one of names
// (strings.EqualFold, as Go's decoder folds) must be spelled as that name
// and appear once.
func scanKeys(dec *json.Decoder, names ...string) (map[string]json.RawMessage, error) {
	found := map[string]json.RawMessage{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := t.(string)
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, err
		}
		for _, n := range names {
			if !strings.EqualFold(key, n) {
				continue
			}
			if key != n {
				return nil, fmt.Errorf("%q is not Docker's spelling of %s; write %s", key, n, n)
			}
			if _, dup := found[n]; dup {
				return nil, fmt.Errorf("%s is given more than once", n)
			}
			found[n] = val
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return found, nil
}

// applyOOMScore gives a container a job creates through the proxy (a test
// database, a compose service) at least the job's own oom_score_adj
// (ctrOOMScoreDefault, ctrOOMScoreExplicit): in a full VM or host the
// kernel then takes what kran's jobs started before the machine's own
// containers, which sit at 0. A higher score asked for is kept; a lower or
// negative one is raised to the job's, and OomKillDisable is dropped, so a
// container a job creates through the proxy does not rank below the
// machine's own containers. This is a default against accidents, not a
// fence: the job can reach Docker's own socket, a privileged exec can
// re-score a container, and build steps run at dockerd's score (SPEC
// §6.8). score <= 0 (rootless Docker, or an image of an older kran):
// nothing changes.
func applyOOMScore(hc map[string]json.RawMessage, score int) bool {
	if score <= 0 {
		return false
	}
	changed := false
	asked := 0
	if raw, ok := hc["OomScoreAdj"]; ok {
		json.Unmarshal(raw, &asked)
	}
	if asked < score {
		hc["OomScoreAdj"], _ = json.Marshal(score)
		changed = true
	}
	var disable bool
	if raw, ok := hc["OomKillDisable"]; ok && json.Unmarshal(raw, &disable) == nil && disable {
		hc["OomKillDisable"], _ = json.Marshal(false)
		changed = true
	}
	return changed
}

// proxyOOMScore reads the job's score for applyOOMScore from
// KRAN_INIT_OOM_SCORE (set by kran only next to its own --oom-score-adj);
// anything but 1..1000 means none.
func proxyOOMScore(v string) int {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 1000 {
		return 0
	}
	return n
}

// rewriteCreateVolume rewrites the device of a local volume bound to a
// directory (compose: driver_opts {type: none, o: bind, device: ./data}).
func rewriteCreateVolume(maps []dockerMap, body []byte) ([]byte, bool, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, false, err
	}
	var opts map[string]string
	if raw, ok := top["DriverOpts"]; !ok || json.Unmarshal(raw, &opts) != nil {
		return body, false, nil
	}
	isBind := false
	for _, o := range strings.Split(opts["o"], ",") {
		isBind = isBind || o == "bind" || o == "rbind"
	}
	h, ok := mapHostPath(maps, opts["device"])
	if !isBind || !ok {
		return body, false, nil
	}
	opts["device"] = h
	top["DriverOpts"], _ = json.Marshal(opts)
	out, err := json.Marshal(top)
	return out, err == nil, err
}

// rewriteRequest replaces the body of a create request whose mounts need
// mapping, or (with a score) of every container create. A container
// create it cannot read is refused when the job has a score; anything else
// it cannot parse goes to Docker unchanged (and Docker reports the error,
// as without kran).
func rewriteRequest(maps []dockerMap, score int, req *http.Request) error {
	if req.Method != http.MethodPost || (len(maps) == 0 && score <= 0) {
		return nil
	}
	var fn func([]dockerMap, []byte) ([]byte, bool, error)
	switch p := req.URL.Path; {
	case createContainerRe.MatchString(p):
		fn = func(m []dockerMap, b []byte) ([]byte, bool, error) { return rewriteCreateContainer(m, score, b) }
	case createVolumeRe.MatchString(p):
		fn = rewriteCreateVolume
	default:
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxRewriteBody+1))
	req.Body.Close()
	if err != nil {
		return err
	}
	if len(body) > maxRewriteBody {
		return errors.New("request body too large")
	}
	nb, changed, err := fn(maps, body)
	if err != nil && score > 0 && createContainerRe.MatchString(req.URL.Path) {
		// Passed on unread it could reach Docker with any score it asks.
		return fmt.Errorf("the Docker proxy refused this container create (%v); kran gives the containers a job creates the job's place in the kernel's kill order and passes on only a create it can read", err)
	}
	if err == nil && changed {
		body = nb
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.TransferEncoding = nil
	req.Header.Del("Content-Length")
	return nil
}

// DockerProxy implements `kran internal docker-proxy --listen <sock>
// --upstream <sock>` (maps in KRAN_INIT_DOCKER_MAP). It runs until killed:
// the job's container ends it.
func DockerProxy(args []string) int {
	fs := flag.NewFlagSet("docker-proxy", flag.ContinueOnError)
	listen := fs.String("listen", "", "socket to serve")
	upstream := fs.String("upstream", "", "Docker's socket")
	if err := fs.Parse(args); err != nil || *listen == "" || *upstream == "" {
		fmt.Fprintln(os.Stderr, "usage: kran internal docker-proxy --listen <socket> --upstream <socket>")
		return 2
	}
	maps := parseDockerMaps(os.Getenv("KRAN_INIT_DOCKER_MAP"))
	score := proxyOOMScore(os.Getenv("KRAN_INIT_OOM_SCORE"))
	// The job's /tmp is a volume of its own container, created with it:
	// only Docker knows where it lives.
	if name := os.Getenv("KRAN_INIT_CONTAINER"); name != "" {
		if dir := containerMountSource(*upstream, name, ctrTmp); dir != "" {
			maps = sortDockerMaps(append(maps, dockerMap{Ctr: ctrTmp, Host: dir}))
		}
	}
	os.Remove(*listen)
	ln, err := net.Listen("unix", *listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kran: docker proxy: %v\n", err)
		return 1
	}
	os.Chmod(*listen, 0o660)
	serveDockerProxy(ln, *upstream, maps, score)
	return 1
}

// serveDockerProxy accepts until ln fails.
func serveDockerProxy(ln net.Listener, upstream string, maps []dockerMap, score int) {
	for {
		c, err := ln.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			return
		}
		go proxyDockerConn(c, upstream, maps, score)
	}
}

// proxyDockerConn serves one client connection: the first request is
// parsed (and rewritten when it creates a container or volume), the rest
// of the connection is copied raw. A request that does not upgrade the
// connection is sent with "Connection: close", so a client that has more
// to ask opens a new connection, whose first request is parsed again.
// Upgraded connections (attach, exec) carry no further requests.
func proxyDockerConn(c net.Conn, upstream string, maps []dockerMap, score int) {
	defer c.Close()
	// A request kran cannot handle must not end the job's proxy (and with
	// it the job's Docker): it ends this connection only.
	defer func() { recover() }()
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	up, err := net.Dial("unix", upstream)
	if err != nil {
		proxyError(c, http.StatusBadGateway, "kran: cannot reach Docker: "+err.Error())
		return
	}
	defer up.Close()
	if err := rewriteRequest(maps, score, req); err != nil {
		proxyError(c, http.StatusBadRequest, "kran: "+err.Error())
		return
	}
	// One request per connection, also with an Upgrade header: where
	// Docker upgrades it hijacks the connection and the stream belongs to
	// the upgraded protocol; anywhere else Docker answers and closes, so a
	// second request on the connection never reaches Docker unread.
	req.Close = true
	if err := req.Write(up); err != nil {
		return
	}
	if rewritesResponse(maps, req) {
		// An answer that shows bind sources: map them back.
		resp, err := http.ReadResponse(bufio.NewReader(up), req)
		if err != nil {
			return
		}
		rewriteResponse(maps, resp)
		resp.Close = true
		resp.Write(c)
		resp.Body.Close()
		return
	}
	go func() {
		// Whatever the client sends after the request (stdin of an
		// attached container) goes through as is.
		io.Copy(up, br)
		if cw, ok := up.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
	}()
	io.Copy(c, up)
}

func proxyError(c net.Conn, code int, msg string) {
	body, _ := json.Marshal(map[string]string{"message": msg})
	fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %s\r\nConnection: close\r\n\r\n%s",
		code, http.StatusText(code), strconv.Itoa(len(body)), body)
}
