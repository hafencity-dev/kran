package workspace

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
)

// The hash cache is one compact binary file per workspace root, replaced
// atomically (temp + rename). Readers never see a partial file, concurrent
// scanners from several processes simply race to publish a complete
// snapshot (last writer wins, both are valid), and loading 30k entries is a
// single sequential read — much cheaper than row-wise sqlite access.
//
// Layout: magic | root | count | entries… | crc32(all previous bytes).
// Entry: path (uvarint len + bytes), dev, ino, size, mtime, ctime (uvarint /
// varint), sha256 (32 bytes).

const cacheMagic = "KRANHC1\n"

type statKey struct {
	dev, ino     uint64
	size         int64
	mtime, ctime int64 // unix ns
}

type cacheEntry struct {
	key  statKey
	hash [32]byte
}

func cacheFile(dir, root string) string {
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(dir, "hashcache", hex.EncodeToString(sum[:12])+".bin")
}

// loadCache returns the cached entries for root; a missing or corrupt file
// yields an empty cache (it is only an optimization).
func loadCache(dir, root string) map[string]cacheEntry {
	b, err := os.ReadFile(cacheFile(dir, root))
	if err != nil {
		return map[string]cacheEntry{}
	}
	m, err := decodeCache(b, root)
	if err != nil {
		return map[string]cacheEntry{}
	}
	return m
}

func decodeCache(b []byte, root string) (map[string]cacheEntry, error) {
	if len(b) < len(cacheMagic)+4 || string(b[:len(cacheMagic)]) != cacheMagic {
		return nil, errors.New("bad magic")
	}
	body, sum := b[:len(b)-4], binary.BigEndian.Uint32(b[len(b)-4:])
	if crc32.ChecksumIEEE(body) != sum {
		return nil, errors.New("bad checksum")
	}
	r := bytes.NewReader(body[len(cacheMagic):])
	gotRoot, err := readString(r)
	if err != nil || gotRoot != root {
		return nil, errors.New("root mismatch")
	}
	n, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, err
	}
	m := make(map[string]cacheEntry, n)
	for i := uint64(0); i < n; i++ {
		p, err := readString(r)
		if err != nil {
			return nil, err
		}
		var e cacheEntry
		var v [3]uint64
		for j := range v {
			if v[j], err = binary.ReadUvarint(r); err != nil {
				return nil, err
			}
		}
		e.key.dev, e.key.ino, e.key.size = v[0], v[1], int64(v[2])
		if e.key.mtime, err = binary.ReadVarint(r); err != nil {
			return nil, err
		}
		if e.key.ctime, err = binary.ReadVarint(r); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(r, e.hash[:]); err != nil {
			return nil, err
		}
		m[p] = e
	}
	return m, nil
}

func readString(r *bytes.Reader) (string, error) {
	n, err := binary.ReadUvarint(r)
	if err != nil {
		return "", err
	}
	if n > uint64(r.Len()) {
		return "", io.ErrUnexpectedEOF
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return string(b), nil
}

// saveCache publishes a complete snapshot for root.
func saveCache(dir, root string, m map[string]cacheEntry) error {
	file := cacheFile(dir, root)
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return fmt.Errorf("hash cache dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".tmp-*")
	if err != nil {
		return fmt.Errorf("hash cache: %w", err)
	}
	defer os.Remove(tmp.Name())
	crc := crc32.NewIEEE()
	w := bufio.NewWriterSize(io.MultiWriter(tmp, crc), 256<<10)
	var buf [binary.MaxVarintLen64]byte
	putU := func(v uint64) { w.Write(buf[:binary.PutUvarint(buf[:], v)]) }
	putI := func(v int64) { w.Write(buf[:binary.PutVarint(buf[:], v)]) }
	putS := func(s string) { putU(uint64(len(s))); w.WriteString(s) }

	w.WriteString(cacheMagic)
	putS(root)
	putU(uint64(len(m)))
	for p, e := range m {
		putS(p)
		putU(e.key.dev)
		putU(e.key.ino)
		putU(uint64(e.key.size))
		putI(e.key.mtime)
		putI(e.key.ctime)
		w.Write(e.hash[:])
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		return fmt.Errorf("hash cache write: %w", err)
	}
	var tail [4]byte
	binary.BigEndian.PutUint32(tail[:], crc.Sum32())
	if _, err := tmp.Write(tail[:]); err != nil {
		tmp.Close()
		return fmt.Errorf("hash cache write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("hash cache write: %w", err)
	}
	if err := os.Rename(tmp.Name(), file); err != nil {
		return fmt.Errorf("hash cache publish: %w", err)
	}
	return nil
}
