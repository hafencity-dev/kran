package link

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"time"

	"github.com/hafencity-dev/kran/internal/config"
	"github.com/hafencity-dev/kran/internal/paths"
	"github.com/hafencity-dev/kran/internal/service"
	"github.com/hafencity-dev/kran/internal/version"
)

// Info describes a machine's kran installation (`kran internal link-info`).
type Info struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	OS        string   `json:"os"`
	Arch      string   `json:"arch"`
	Version   string   `json:"version"`
	Protocol  string   `json:"protocol"`
	Home      string   `json:"home"`
	User      string   `json:"user"`
	KranPath  string   `json:"kran_path"`
	ConfigDir string   `json:"config_dir"`
	StateDir  string   `json:"state_dir"`
	CacheDir  string   `json:"cache_dir"`
	PubKey    string   `json:"pubkey"` // "" until `kran setup` ran
	HostKey   string   `json:"host_key,omitempty"`
	Addresses []string `json:"addresses"`
	SSHPort   int      `json:"ssh_port"`
	HasDaemon bool     `json:"has_daemon"`
	// ServiceMode is "launchd-system", "launchd-agent", "systemd" or "" (no
	// service installed). NoGUI is set on a Mac without a GUI login, where
	// the daemon can only start on demand or as a system daemon.
	ServiceMode string `json:"service_mode,omitempty"`
	NoGUI       bool   `json:"no_gui,omitempty"`
	SHA256      string `json:"binary_sha256"`
}

// PeerInfo is what one side tells the other about itself.
type PeerInfo struct {
	Name      string   `json:"name"`
	ID        string   `json:"id"`
	PubKey    string   `json:"pubkey"`
	Addresses []string `json:"addresses"`
	User      string   `json:"user"` // login name to use when connecting to the sender
	HostKey   string   `json:"host_key,omitempty"`
	KranPath  string   `json:"kran_path"`
	OS        string   `json:"os"`
	Arch      string   `json:"arch"`
	Version   string   `json:"version"`
	// Direction is from the receiver's point of view: both | outbound
	// (receiver dials sender) | inbound (sender dials receiver).
	Direction string `json:"direction"`
}

// AcceptRequest is the stdin of `kran internal link-accept`.
type AcceptRequest struct {
	Peer PeerInfo `json:"peer"`
	Tag  string   `json:"tag,omitempty"`
}

// AcceptResponse is its stdout.
type AcceptResponse struct {
	Info
	Changed bool `json:"changed"` // authorized_keys or peers.toml changed
}

// ForgetRequest is the stdin of `kran internal link-forget`.
type ForgetRequest struct {
	Name   string `json:"name,omitempty"`
	ID     string `json:"id,omitempty"`
	PubKey string `json:"pubkey,omitempty"`
}

// ForgetResponse is its stdout.
type ForgetResponse struct {
	RemovedKey  bool `json:"removed_key"`
	RemovedPeer bool `json:"removed_peer"`
}

// ProbeRequest is the stdin of `kran internal link-probe`.
type ProbeRequest struct {
	Addresses []string `json:"addresses"`
	HostKey   string   `json:"host_key,omitempty"`
	User      string   `json:"user"`
}

// ProbeResponse is its stdout.
type ProbeResponse struct {
	OKAddress string        `json:"ok_address,omitempty"`
	RTTMs     float64       `json:"rtt_ms,omitempty"`
	Results   []ProbeResult `json:"results"`
}

// InternalMain implements `kran internal link-*`. args excludes "internal".
func InternalMain(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: kran internal link-info|link-accept|link-forget|link-probe")
		return 2
	}
	ctx := context.Background()
	var err error
	switch args[0] {
	case "link-info":
		var info Info
		if info, err = gatherInfo(); err == nil {
			err = writeJSON(stdout, info)
		}
	case "link-accept":
		var req AcceptRequest
		if err = readJSON(stdin, &req); err == nil {
			var resp *AcceptResponse
			if resp, err = doAccept(req); err == nil {
				err = writeJSON(stdout, resp)
			}
		}
	case "link-forget":
		var req ForgetRequest
		if err = readJSON(stdin, &req); err == nil {
			var resp *ForgetResponse
			if resp, err = doForget(req); err == nil {
				err = writeJSON(stdout, resp)
			}
		}
	case "link-probe":
		var req ProbeRequest
		if err = readJSON(stdin, &req); err == nil {
			var resp *ProbeResponse
			if resp, err = doProbe(ctx, req); err == nil {
				err = writeJSON(stdout, resp)
			}
		}
	default:
		fmt.Fprintf(stderr, "kran: unknown internal command %q\n", args[0])
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "kran: %v\n", err)
		return 1
	}
	return 0
}

func readJSON(r io.Reader, v any) error {
	if err := json.NewDecoder(io.LimitReader(r, 1<<20)).Decode(v); err != nil {
		return fmt.Errorf("read request: %w", err)
	}
	return nil
}

func writeJSON(w io.Writer, v any) error { return json.NewEncoder(w).Encode(v) }

func selfExe() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func gatherInfo() (Info, error) {
	exe, err := selfExe()
	if err != nil {
		return Info{}, err
	}
	home, _ := os.UserHomeDir()
	cfg, err := config.Load("")
	name := config.DefaultName()
	if err == nil {
		name = cfg.Name
	}
	sum, _ := fileSHA256(exe)
	pub, _ := config.PublicKey()
	_, statErr := os.Stat(paths.Socket())
	mode := service.Mode(service.Options{})
	noGUI := runtime.GOOS == "darwin" && mode != "launchd-system" && !service.HasGUISession(context.Background(), service.Options{})
	return Info{
		ID: config.NodeID(), Name: name, OS: runtime.GOOS, Arch: runtime.GOARCH,
		Version: version.Version, Protocol: version.Info().Protocol,
		Home: home, User: currentUser(), KranPath: exe,
		ConfigDir: paths.ConfigDir(), StateDir: paths.StateDir(), CacheDir: paths.CacheDir(),
		PubKey: pub, HostKey: localHostKey(), Addresses: localAddresses(), SSHPort: sshdPort(),
		HasDaemon: statErr == nil, SHA256: sum, ServiceMode: mode, NoGUI: noGUI,
	}, nil
}

func doAccept(req AcceptRequest) (*AcceptResponse, error) {
	p := req.Peer
	if !config.ValidName(p.Name) {
		return nil, fmt.Errorf("peer name %q is not valid", p.Name)
	}
	if keyBlob(p.PubKey) == "" {
		return nil, errors.New("peer public key is missing")
	}
	exe, err := selfExe()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load("")
	if err != nil {
		return nil, err
	}
	if _, _, err := config.EnsureNodeID(); err != nil {
		return nil, err
	}
	if _, _, err := config.EnsureKey(cfg.Name); err != nil {
		return nil, err
	}
	ak, err := authorizedKeysPath()
	if err != nil {
		return nil, err
	}
	changed, err := EnsureAuthorizedKey(ak, PeerKeyLineEnv(exe, p.Name, p.PubKey, req.Tag, InstallEnv()))
	if err != nil {
		return nil, err
	}
	if p.Direction == "" {
		p.Direction = "both"
	}
	peerChanged, err := upsertPeer(config.Peer{
		Name: p.Name, ID: p.ID, Addresses: p.Addresses, User: p.User, HostKey: p.HostKey,
		KranPath: p.KranPath, Direction: p.Direction, OS: p.OS, Arch: p.Arch, Tag: req.Tag,
		RemoteConfigDir: "",
	}, time.Now())
	if err != nil {
		return nil, err
	}
	info, err := gatherInfo()
	if err != nil {
		return nil, err
	}
	return &AcceptResponse{Info: info, Changed: changed || peerChanged}, nil
}

// upsertPeer writes the peer unless an equivalent entry exists (LinkedAt and
// the remembered fastest address are not part of the comparison).
func upsertPeer(n config.Peer, now time.Time) (changed bool, err error) {
	err = config.UpdatePeers("", func(ps *config.Peers) error {
		if old, ok := ps.Find(n.Name); ok {
			n.LinkedAt = old.LinkedAt
			if n.PreferredAddress == "" {
				n.PreferredAddress = old.PreferredAddress
			}
			if n.RemoteConfigDir == "" {
				n.RemoteConfigDir = old.RemoteConfigDir
			}
			if reflect.DeepEqual(*old, n) {
				return errNoChange
			}
		}
		if n.LinkedAt.IsZero() {
			n.LinkedAt = now.UTC().Truncate(time.Second)
		}
		ps.Upsert(n)
		changed = true
		return nil
	})
	if errors.Is(err, errNoChange) {
		return false, nil
	}
	return changed, err
}

var errNoChange = errors.New("no change")

func doForget(req ForgetRequest) (*ForgetResponse, error) {
	var resp ForgetResponse
	if blob := keyBlob(req.PubKey); blob != "" {
		ak, err := authorizedKeysPath()
		if err != nil {
			return nil, err
		}
		if resp.RemovedKey, err = RemoveAuthorizedKey(ak, blob); err != nil {
			return nil, err
		}
	}
	err := config.UpdatePeers("", func(ps *config.Peers) error {
		for i := range ps.Peer {
			if (req.ID != "" && ps.Peer[i].ID == req.ID) || (req.Name != "" && ps.Peer[i].Name == req.Name) {
				ps.Peer = append(ps.Peer[:i], ps.Peer[i+1:]...)
				resp.RemovedPeer = true
				return nil
			}
		}
		return errNoChange
	})
	if err != nil && !errors.Is(err, errNoChange) {
		return nil, err
	}
	return &resp, nil
}

func doProbe(ctx context.Context, req ProbeRequest) (*ProbeResponse, error) {
	signer, err := config.Signer()
	if err != nil {
		return nil, err
	}
	rs := HandshakeAll(ctx, req.Addresses, req.User, signer, req.HostKey, 5*time.Second)
	resp := &ProbeResponse{Results: rs}
	if best, ok := fastestOK(rs); ok {
		resp.OKAddress, resp.RTTMs = best.Address, best.RTTMs
	}
	return resp, nil
}
