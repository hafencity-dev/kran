package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/hafencity-dev/kran/internal/paths"
)

// PubKeyFile is the public half of kran's own SSH key.
func PubKeyFile() string { return paths.KeyFile() + ".pub" }

// EnsureKey creates kran's ed25519 key pair (OpenSSH format, private 0600)
// when it does not exist and returns the public key line
// "ssh-ed25519 AAAA… kran@<name>". The key is dedicated to kran: on peers it
// is pinned to a forced command, so it can do nothing but speak kran protocol.
func EnsureKey(nodeName string) (pub string, created bool, err error) {
	keyPath := paths.KeyFile()
	err = WithLock(keyPath, func() error {
		if cur, e := PublicKey(); e == nil {
			if _, e2 := os.Stat(keyPath); e2 == nil {
				pub = cur
				return nil
			}
		}
		pk, sk, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			return e
		}
		comment := "kran@" + nodeName
		blk, e := ssh.MarshalPrivateKey(sk, comment)
		if e != nil {
			return e
		}
		sshPub, e := ssh.NewPublicKey(pk)
		if e != nil {
			return e
		}
		pub = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " " + comment
		if e := WriteFileAtomic(keyPath, pem.EncodeToMemory(blk), 0o600); e != nil {
			return e
		}
		created = true
		return WriteFileAtomic(PubKeyFile(), []byte(pub+"\n"), 0o644)
	})
	if err != nil {
		return "", false, fmt.Errorf("create kran key: %w", err)
	}
	return pub, created, nil
}

// PublicKey returns the stored public key line.
func PublicKey() (string, error) {
	data, err := os.ReadFile(PubKeyFile())
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(data))
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line)); err != nil {
		return "", fmt.Errorf("%s is not a valid public key: %w", PubKeyFile(), err)
	}
	return line, nil
}

// Signer loads kran's private key for SSH client authentication.
func Signer() (ssh.Signer, error) {
	data, err := os.ReadFile(paths.KeyFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no kran key at %s; run `kran setup`", paths.KeyFile())
	}
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(data)
}
