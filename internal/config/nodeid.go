package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hafencity-dev/kran/internal/paths"
)

// NodeIDFile is where the node ID lives; it never changes after setup.
func NodeIDFile() string { return filepath.Join(paths.ConfigDir(), "node_id") }

// NewNodeID returns a random 16-byte hex ID.
func NewNodeID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Errorf("kran: no randomness available: %w", err))
	}
	return hex.EncodeToString(b[:])
}

// NodeID returns the stored node ID, or "" when setup has not run.
func NodeID() string {
	data, err := os.ReadFile(NodeIDFile())
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(data))
	if len(id) != 32 {
		return ""
	}
	return id
}

// EnsureNodeID returns the node ID, generating and storing it on first use.
// created reports whether a new ID was made.
func EnsureNodeID() (id string, created bool, err error) {
	path := NodeIDFile()
	err = WithLock(path, func() error {
		if cur := NodeID(); cur != "" {
			id = cur
			return nil
		}
		id = NewNodeID()
		created = true
		return WriteFileAtomic(path, []byte(id+"\n"), 0o600)
	})
	if err != nil && !errors.Is(err, os.ErrPermission) {
		err = fmt.Errorf("node id: %w", err)
	}
	return id, created, err
}
