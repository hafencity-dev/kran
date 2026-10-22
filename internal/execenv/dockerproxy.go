package execenv

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
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
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].Ctr) > len(out[j].Ctr) })
	return out
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

// rewriteCreateContainer rewrites HostConfig.Binds and bind Mounts.
func rewriteCreateContainer(maps []dockerMap, body []byte) ([]byte, bool, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, false, err
	}
	var hc map[string]json.RawMessage
	if raw, ok := top["HostConfig"]; !ok || json.Unmarshal(raw, &hc) != nil || hc == nil {
		return body, false, nil
	}
	changed := false
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
	if !changed {
		return body, false, nil
	}
	top["HostConfig"], _ = json.Marshal(hc)
	out, err := json.Marshal(top)
	return out, err == nil, err
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
// mapping. Anything it cannot parse goes to Docker unchanged (and Docker
// reports the error, as without kran).
func rewriteRequest(maps []dockerMap, req *http.Request) error {
	if req.Method != http.MethodPost || len(maps) == 0 {
		return nil
	}
	var fn func([]dockerMap, []byte) ([]byte, bool, error)
	switch p := req.URL.Path; {
	case createContainerRe.MatchString(p):
		fn = rewriteCreateContainer
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
	if nb, changed, err := fn(maps, body); err == nil && changed {
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
	os.Remove(*listen)
	ln, err := net.Listen("unix", *listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kran: docker proxy: %v\n", err)
		return 1
	}
	os.Chmod(*listen, 0o660)
	serveDockerProxy(ln, *upstream, maps)
	return 1
}

// serveDockerProxy accepts until ln fails.
func serveDockerProxy(ln net.Listener, upstream string, maps []dockerMap) {
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
		go proxyDockerConn(c, upstream, maps)
	}
}

// proxyDockerConn serves one client connection: the first request is
// parsed (and rewritten when it creates a container or volume), the rest
// of the connection is copied raw. A request that does not upgrade the
// connection is sent with "Connection: close", so a client that has more
// to ask opens a new connection, whose first request is parsed again.
// Upgraded connections (attach, exec) carry no further requests.
func proxyDockerConn(c net.Conn, upstream string, maps []dockerMap) {
	defer c.Close()
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
	if err := rewriteRequest(maps, req); err != nil {
		proxyError(c, http.StatusBadRequest, "kran: "+err.Error())
		return
	}
	if req.Header.Get("Upgrade") == "" {
		req.Close = true
	}
	if err := req.Write(up); err != nil {
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
