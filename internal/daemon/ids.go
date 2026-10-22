package daemon

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"sync"
	"time"
)

// Job IDs (SPEC §7): 10 chars of lowercase Crockford base32, sortable by
// time. 35 bits of 100 ms ticks since 2025-01-01 (≈108 years) followed by 15
// random bits. Within one process IDs are strictly increasing.
const idAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

var idEpoch = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

type idGen struct {
	mu   sync.Mutex
	last uint64
}

func (g *idGen) next(now time.Time) string {
	ticks := uint64(now.Sub(idEpoch) / (100 * time.Millisecond))
	var rb [2]byte
	rand.Read(rb[:])
	v := ticks<<15 | uint64(binary.BigEndian.Uint16(rb[:])&0x7fff)
	g.mu.Lock()
	if v <= g.last {
		v = g.last + 1
	}
	g.last = v
	g.mu.Unlock()
	v &= 1<<50 - 1
	var out [10]byte
	for i := 9; i >= 0; i-- {
		out[i] = idAlphabet[v&31]
		v >>= 5
	}
	return string(out[:])
}

// validID reports whether s looks like a job ID we generate or accept
// (protects paths built from IDs).
func validID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
