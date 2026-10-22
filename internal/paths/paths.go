// Package paths resolves every filesystem location kran uses (SPEC §4).
// Environment overrides (used by tests and multi-daemon setups):
// KRAN_CONFIG_DIR, KRAN_STATE_DIR, KRAN_RUNTIME_DIR, KRAN_CACHE_DIR.
package paths

import (
	"os"
	"path/filepath"
	"runtime"
)

func home() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return "/"
}

// ConfigDir holds config.toml, peers.toml and the kran SSH key.
func ConfigDir() string {
	if d := os.Getenv("KRAN_CONFIG_DIR"); d != "" {
		return d
	}
	if runtime.GOOS != "darwin" {
		if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
			return filepath.Join(d, "kran")
		}
	}
	return filepath.Join(home(), ".config", "kran")
}

// StateDir holds the database, logs, job dirs, slots and blobs.
func StateDir() string {
	if d := os.Getenv("KRAN_STATE_DIR"); d != "" {
		return d
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home(), "Library", "Application Support", "kran")
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "kran")
	}
	return filepath.Join(home(), ".local", "state", "kran")
}

// RuntimeDir holds sockets and locks.
func RuntimeDir() string {
	if d := os.Getenv("KRAN_RUNTIME_DIR"); d != "" {
		return d
	}
	if runtime.GOOS != "darwin" {
		if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
			return filepath.Join(d, "kran")
		}
	}
	return StateDir()
}

// CacheDir holds downloaded kran binaries for other platforms.
func CacheDir() string {
	if d := os.Getenv("KRAN_CACHE_DIR"); d != "" {
		return d
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home(), "Library", "Caches", "kran")
	}
	if d := os.Getenv("XDG_CACHE_HOME"); d != "" {
		return filepath.Join(d, "kran")
	}
	return filepath.Join(home(), ".cache", "kran")
}

func Socket() string     { return filepath.Join(RuntimeDir(), "kran.sock") }
func PeerSocket() string { return filepath.Join(RuntimeDir(), "kran-peer.sock") }
func ConfigFile() string { return filepath.Join(ConfigDir(), "config.toml") }
func PeersFile() string  { return filepath.Join(ConfigDir(), "peers.toml") }
func KeyFile() string    { return filepath.Join(ConfigDir(), "id_ed25519") }
func DBFile() string     { return filepath.Join(StateDir(), "kran.db") }
func LogFile() string    { return filepath.Join(StateDir(), "daemon.log") }
func TokenFile() string  { return filepath.Join(StateDir(), "ui.token") }
func JobsDir() string    { return filepath.Join(StateDir(), "jobs") }
func SlotsDir() string   { return filepath.Join(StateDir(), "slots") }
func BlobsDir() string   { return filepath.Join(StateDir(), "blobs") }

// BinaryCache is where kran binaries for other platforms are kept:
// <cache>/bin/<version>/<os>-<arch>/kran.
func BinaryCache(version, goos, goarch string) string {
	return filepath.Join(CacheDir(), "bin", version, goos+"-"+goarch, "kran")
}

// EnsureDirs creates config, state, runtime and cache dirs with 0700.
func EnsureDirs() error {
	for _, d := range []string{ConfigDir(), StateDir(), RuntimeDir(), CacheDir(), JobsDir(), SlotsDir(), BlobsDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}
