package link

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/hafencity-dev/kran/internal/config"
)

// UnlinkResult reports what Unlink did.
type UnlinkResult struct {
	RemovedPeer   bool
	RemovedKey    bool     // their kran key removed from our authorized_keys
	RemoteCleaned bool     // our key and entry removed on the peer
	Leftover      []string // sentences: what remains to be cleaned by hand
}

// Unlink reverses exactly what Link added for the named peer: the peer's key
// in our authorized_keys, our key and peer entry on the peer (when it is
// reachable), and the peers.toml entry here.
func Unlink(ctx context.Context, name string, o Options) error {
	_, err := UnlinkDetailed(ctx, name, o)
	return err
}

// UnlinkDetailed is Unlink with a report.
func UnlinkDetailed(ctx context.Context, name string, o Options) (*UnlinkResult, error) {
	ps, err := config.LoadPeers("")
	if err != nil {
		return nil, err
	}
	peer, ok := ps.Find(name)
	if !ok {
		return nil, fmt.Errorf("no linked machine named %q; `kran status` lists the linked machines", name)
	}
	p := *peer
	st, haveState := readLinkState(name)
	if o.SelfName == "" {
		cfg, err := config.Load("")
		if err != nil {
			return nil, err
		}
		o.SelfName = cfg.Name
	}
	if o.SelfID == "" {
		if o.SelfID, _, err = config.EnsureNodeID(); err != nil {
			return nil, err
		}
	}
	res := &UnlinkResult{}

	// Remote side first, while we still know how to reach it.
	if o.Host == "" && haveState {
		o.Host = st.SSHDest
		if o.SSHArgs == nil {
			o.SSHArgs = st.SSHArgs
		}
		if o.RemoteEnv == nil {
			o.RemoteEnv = st.RemoteEnv
		}
	}
	if o.Host == "" {
		o.Host = destFromPeer(p, &o)
	}
	if o.RemoteEnv == nil && p.RemoteConfigDir != "" {
		o.RemoteEnv = map[string]string{"KRAN_CONFIG_DIR": p.RemoteConfigDir}
	}
	if o.Host != "" && p.KranPath != "" {
		o.progress("remote", "Removing this machine from "+name)
		c := newSSH(&o)
		defer c.exitMaster()
		if err := c.connect(ctx); err == nil {
			selfPub, _ := config.PublicKey()
			var fr ForgetResponse
			ferr := c.kranJSON(ctx, p.KranPath, ForgetRequest{Name: o.SelfName, ID: o.SelfID, PubKey: selfPub}, &fr, "link-forget")
			if ferr == nil {
				res.RemoteCleaned = true
			} else {
				res.Leftover = append(res.Leftover, fmt.Sprintf("On %s, run `kran internal link-forget` or remove the kran line for %s from ~/.ssh/authorized_keys and the %s entry from peers.toml (%v).", name, o.SelfName, o.SelfName, ferr))
			}
		} else {
			res.Leftover = append(res.Leftover, fmt.Sprintf("%s is not reachable, so its side was not touched. There, remove the authorized_keys line ending in \"kran@%s\" and the %s entry in peers.toml (or run `kran unlink %s` on it).", name, o.SelfName, o.SelfName, o.SelfName))
		}
	} else {
		res.Leftover = append(res.Leftover, fmt.Sprintf("Could not tell how to reach %s; run `kran unlink %s` there to finish.", name, o.SelfName))
	}

	// Our side.
	if haveState && st.PubKey != "" {
		ak, err := authorizedKeysPath()
		if err != nil {
			return res, err
		}
		if res.RemovedKey, err = RemoveAuthorizedKey(ak, keyBlob(st.PubKey)); err != nil {
			return res, err
		}
	} else {
		res.Leftover = append(res.Leftover, fmt.Sprintf("Could not tell which key %s used; remove its kran line (command=\"… serve --stdio\", comment kran@%s) from ~/.ssh/authorized_keys.", name, name))
	}
	err = config.UpdatePeers("", func(ps *config.Peers) error {
		res.RemovedPeer = ps.Remove(name)
		return nil
	})
	if err != nil {
		return res, err
	}
	if err := os.Remove(linkStatePath(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return res, err
	}
	return res, nil
}

// destFromPeer builds an ssh destination from the peer's first address when
// no original destination was recorded.
func destFromPeer(p config.Peer, o *Options) string {
	if len(p.Addresses) == 0 {
		return ""
	}
	addr := p.Addresses[0]
	host, port := strings.Trim(addr, "[]"), ""
	if h, pt, err := net.SplitHostPort(addr); err == nil {
		host, port = h, pt
	}
	if port != "" && port != "22" {
		o.SSHArgs = append(append([]string{}, o.SSHArgs...), "-p", port)
	}
	if p.User != "" {
		return p.User + "@" + host
	}
	return host
}
