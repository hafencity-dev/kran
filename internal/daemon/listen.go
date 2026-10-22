package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/hafencity-dev/kran/internal/transport"
)

// listen opens the unix API socket, the peer socket and the UI listener.
// It returns stop functions run at shutdown.
func (d *Daemon) listen() ([]func(), error) {
	var stops []func()
	fail := func(err error) ([]func(), error) {
		for _, s := range stops {
			s()
		}
		return nil, err
	}

	ul, err := transport.ListenUnix(d.deps.Dirs.socket())
	if err != nil {
		if errors.Is(err, transport.ErrAlreadyRunning) {
			return nil, fmt.Errorf("another kran daemon is already running (socket %s); stop it first: kran service stop", d.deps.Dirs.socket())
		}
		return nil, err
	}
	api := &http.Server{Handler: d.unixHandler, ReadHeaderTimeout: 10 * time.Second, ErrorLog: nil}
	go api.Serve(ul)
	stops = append(stops, func() { shutdownServer(api) })

	pl, err := transport.ListenUnix(d.deps.Dirs.peerSocket())
	if err != nil {
		return fail(err)
	}
	go func() {
		for {
			c, err := pl.Accept()
			if err != nil {
				return
			}
			go d.ServePeerConn(c)
		}
	}()
	stops = append(stops, func() { pl.Close() })

	addr, loopback, err := resolveUIListen(d.cfg.UIListen)
	if err != nil {
		d.logf("ui: %v; the web UI is off", err)
	} else if addr != "" {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			// Never fatal: the UI is an extra, kran run must keep working.
			d.logf("ui: cannot listen on %s: %v; the web UI is off (set ui.listen in config.toml)", addr, err)
		} else {
			d.uiAddr = ln.Addr().String()
			bindHost, _, _ := net.SplitHostPort(addr)
			ui := &http.Server{Handler: d.tcpAuth(d.tcpHandler, loopback, bindHost), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute}
			go ui.Serve(ln)
			stops = append(stops, func() { shutdownServer(ui) })
			d.logf("ui: listening on http://%s", d.uiAddr)
		}
	}
	return stops, nil
}

// UIAddr is the UI listener's address ("" when off).
func (d *Daemon) UIAddr() string { return d.uiAddr }

func shutdownServer(s *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if s.Shutdown(ctx) != nil {
		s.Close()
	}
}

// resolveUIListen maps ui.listen to an address. "" disables the UI;
// "tailscale" binds this node's tailscale IPv4 on port 7270.
func resolveUIListen(v string) (addr string, loopback bool, err error) {
	v = strings.TrimSpace(v)
	switch v {
	case "", "-", "off", "false":
		return "", false, nil
	case "tailscale":
		ip, err := tailscaleIP()
		if err != nil {
			return "", false, err
		}
		return net.JoinHostPort(ip.String(), "7270"), false, nil
	}
	host, _, err := net.SplitHostPort(v)
	if err != nil {
		return "", false, fmt.Errorf("ui.listen %q is not host:port", v)
	}
	if host == "localhost" {
		return v, true, nil
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return v, ip != nil && ip.IsLoopback(), nil
}

var tailnet = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func tailscaleIP() (net.IP, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip4 := n.IP.To4(); ip4 != nil && tailnet.Contains(ip4) {
				return ip4, nil
			}
		}
	}
	return nil, errors.New("no tailscale address found (is tailscale up?)")
}
