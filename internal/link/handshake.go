package link

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// ProbeResult is the outcome of one handshake attempt.
type ProbeResult struct {
	Address string  `json:"address"`
	OK      bool    `json:"ok"`
	RTTMs   float64 `json:"rtt_ms,omitempty"`
	Error   string  `json:"error,omitempty"`
	// HostKey is the key the server presented ("type base64"); reported when
	// no key was pinned so the caller can pin it.
	HostKey string `json:"host_key,omitempty"`
}

func parseHostKey(k string) (ssh.PublicKey, error) {
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(k))
	return pk, err
}

func hostKeyAlgs(pk ssh.PublicKey) []string {
	switch pk.Type() {
	case ssh.KeyAlgoRSA:
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	default:
		return []string{pk.Type()}
	}
}

// Handshake dials addr, authenticates with signer as user and checks the host
// key against the pinned key (pinned == "" fails: an unverified server proves
// nothing). A completed handshake proves reachability and that the
// forced-command authorized_keys entry exists; no command is run.
func Handshake(ctx context.Context, addr, user string, signer ssh.Signer, pinned string, timeout time.Duration) ProbeResult {
	res := ProbeResult{Address: addr}
	if pinned == "" {
		res.Error = "no host key to check the server against"
		return res
	}
	pin, err := parseHostKey(pinned)
	if err != nil {
		res.Error = fmt.Sprintf("pinned host key is invalid: %v", err)
		return res
	}
	cfg := &ssh.ClientConfig{
		User:    user,
		Auth:    []ssh.AuthMethod{ssh.PublicKeys(signer)},
		Timeout: timeout,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			res.HostKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
			if !bytes.Equal(pin.Marshal(), key.Marshal()) {
				return fmt.Errorf("host key changed: expected %s, got %s", ssh.FingerprintSHA256(pin), ssh.FingerprintSHA256(key))
			}
			return nil
		},
	}
	cfg.HostKeyAlgorithms = hostKeyAlgs(pin)
	start := time.Now()
	d := net.Dialer{Timeout: timeout}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := d.DialContext(cctx, "tcp", addr)
	if err != nil {
		res.Error = simplifyNetErr(err)
		return res
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		res.Error = simplifyNetErr(err)
		return res
	}
	client := ssh.NewClient(c, chans, reqs)
	res.RTTMs = float64(time.Since(start).Microseconds()) / 1000
	_ = client.Close()
	res.OK = true
	return res
}

func simplifyNetErr(err error) string {
	s := err.Error()
	if strings.Contains(s, "unable to authenticate") {
		return "the kran key was not accepted (no authorized_keys entry for it, or sshd restricts users/keys)"
	}
	return s
}

// HandshakeAll tries all addresses in parallel and returns results in input
// order.
func HandshakeAll(ctx context.Context, addrs []string, user string, signer ssh.Signer, pinned string, timeout time.Duration) []ProbeResult {
	out := make([]ProbeResult, len(addrs))
	var wg sync.WaitGroup
	for i, a := range addrs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = Handshake(ctx, a, user, signer, pinned, timeout)
		}()
	}
	wg.Wait()
	return out
}

// fastestOK returns the OK address with the lowest RTT.
func fastestOK(rs []ProbeResult) (ProbeResult, bool) {
	var best ProbeResult
	found := false
	for _, r := range rs {
		if r.OK && (!found || r.RTTMs < best.RTTMs) {
			best, found = r, true
		}
	}
	return best, found
}
