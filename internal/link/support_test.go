package link

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/hafencity-dev/kran/internal/config"
)

// TestMain lets the test binary double as a fake `kran`: when it is invoked
// under the name "kran" it behaves like the real subcommands link needs on the
// remote side (internal link-*, setup).
func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == "kran" {
		os.Exit(fakeKran(os.Args[1:]))
	}
	// Unless a test sets its own bases, the default GitHub hosts answer 404.
	dead := httptest.NewServer(http.NotFoundHandler())
	defaultDownloadBase, defaultAPIBase = dead.URL, dead.URL
	code := m.Run()
	dead.Close()
	os.Exit(code)
}

func fakeKran(args []string) int {
	if len(args) == 0 {
		return 2
	}
	switch args[0] {
	case "internal":
		return InternalMain(args[1:], os.Stdin, os.Stdout, os.Stderr)
	case "service":
		sd := os.Getenv("KRAN_STATE_DIR")
		if _, err := os.Stat(filepath.Join(sd, "no-service")); err == nil {
			os.Stderr.WriteString("kran: the service is not installed\n")
			return 1
		}
		os.MkdirAll(sd, 0o700)
		f, _ := os.OpenFile(filepath.Join(sd, "service.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		f.WriteString(strings.Join(args[1:], " ") + "\n")
		f.Close()
		return 0
	case "setup":
		fs := flag.NewFlagSet("setup", flag.ContinueOnError)
		name := fs.String("name", "", "")
		fs.Bool("yes", false, "")
		fs.Bool("quiet", false, "")
		noAgents := fs.Bool("no-agents", false, "")
		fs.Bool("no-link", false, "")
		if fs.Parse(args[1:]) != nil {
			return 2
		}
		if !*noAgents { // link must never touch the remote's coding agents
			os.Stderr.WriteString("setup without --no-agents\n")
			return 2
		}
		if *name != "" {
			c := config.Defaults()
			c.Name = *name
			if err := config.Save("", &c); err != nil {
				os.Stderr.WriteString(err.Error() + "\n")
				return 1
			}
		}
		cfg, _ := config.Load("")
		if _, _, err := config.EnsureNodeID(); err != nil {
			return 1
		}
		if _, _, err := config.EnsureKey(cfg.Name); err != nil {
			return 1
		}
		return 0
	}
	return 2
}

// testSSHD is an in-process ssh server that accepts any user whose public key
// appears as a kran forced-command entry in authFile.
type testSSHD struct {
	Addr    string
	HostPub string // path of the .pub file
	HostKey string // "type base64"
	ln      net.Listener
	wg      sync.WaitGroup
}

func newTestSSHD(t *testing.T, authFile string) *testSSHD {
	t.Helper()
	pk, sk, _ := ed25519.GenerateKey(rand.Reader)
	hsigner, _ := ssh.NewSignerFromKey(sk)
	_ = pk
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			b, err := os.ReadFile(authFile)
			if err != nil {
				return nil, err
			}
			blob := strings.Fields(string(ssh.MarshalAuthorizedKey(key)))[1]
			for _, ln := range strings.Split(string(b), "\n") {
				if al, ok := parseAuthLine(ln); ok && al.blob == blob && al.isKran() && strings.Contains(al.opts, "restrict") {
					return &ssh.Permissions{}, nil
				}
			}
			return nil, errors.New("unknown key")
		},
	}
	cfg.AddHostKey(hsigner)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &testSSHD{Addr: ln.Addr().String(), ln: ln}
	s.HostKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hsigner.PublicKey())))
	s.HostPub = filepath.Join(t.TempDir(), "host.pub")
	os.WriteFile(s.HostPub, []byte(s.HostKey+"\n"), 0o644)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer c.Close()
				conn, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				go func() {
					for ch := range chans {
						ch.Reject(ssh.Prohibited, "no")
					}
				}()
				conn.Wait()
			}()
		}
	}()
	t.Cleanup(func() { ln.Close(); s.wg.Wait() })
	return s
}

// fakeSSH implements Runner: "ssh" runs the remote command locally with a
// separate HOME and scrubbed KRAN_* env (a fresh login shell on another
// machine); other tools are scripted.
type fakeSSH struct {
	t          *testing.T
	remoteHome string
	g          string // output of ssh -G
	mu         sync.Mutex
	calls      []Cmd
	other      func(Cmd) (Output, error)
}

func (f *fakeSSH) Run(ctx context.Context, c Cmd) (Output, error) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	if c.Name != "ssh" {
		if f.other != nil {
			return f.other(c)
		}
		return Output{}, &ExitError{Code: 127, Stderr: c.Name + ": not found"}
	}
	for _, a := range c.Args {
		if a == "-G" {
			return Output{Stdout: []byte(f.g)}, nil
		}
		if a == "-O" {
			return Output{}, nil
		}
	}
	remote := c.Args[len(c.Args)-1]
	if remote == "true" {
		return Output{}, nil
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", remote)
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "KRAN_") && !strings.HasPrefix(e, "HOME=") {
			env = append(env, e)
		}
	}
	cmd.Env = append(env, "HOME="+f.remoteHome)
	cmd.Stdin = c.Stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	o := Output{Stdout: out.Bytes(), Stderr: errb.Bytes()}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return o, &ExitError{Code: ee.ExitCode(), Stderr: errb.String()}
	}
	return o, err
}

func (f *fakeSSH) sawInstall() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.Contains(c.Args[len(c.Args)-1], `cat > "$t"`) {
			n++
		}
	}
	return n
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	os.MkdirAll(filepath.Dir(dst), 0o755)
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	out.Close()
}
