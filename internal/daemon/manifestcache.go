package daemon

import (
	"container/list"
	"sync"

	"github.com/hafencity-dev/kran/internal/proto"
)

// manifestCache keeps recently exchanged manifests by key, so a sync can
// refer to one by hash instead of sending it again (peer protocol 1.1).
// The origin keys by manifest hash, the target by origin and hash: an
// origin can only refer to manifests it sent itself. Bounded by the total
// number of entries held (~200 bytes each in memory); the most recent
// manifest is kept even when it alone exceeds the bound. Manifests in the
// cache are never modified.
type manifestCache struct {
	max int

	mu      sync.Mutex
	entries int
	ll      *list.List // front = most recently used
	items   map[string]*list.Element
}

type cachedManifest struct {
	key string
	m   *proto.Manifest
}

// manifestCacheEntries: about 250k entries, ~50 MB at most.
const manifestCacheEntries = 250_000

func newManifestCache(max int) *manifestCache {
	return &manifestCache{max: max, ll: list.New(), items: map[string]*list.Element{}}
}

func (c *manifestCache) get(key string) *proto.Manifest {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil
	}
	c.ll.MoveToFront(el)
	return el.Value.(*cachedManifest).m
}

func (c *manifestCache) put(key string, m *proto.Manifest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		return
	}
	c.items[key] = c.ll.PushFront(&cachedManifest{key: key, m: m})
	c.entries += len(m.Entries)
	for c.entries > c.max && c.ll.Len() > 1 {
		el := c.ll.Back()
		cm := el.Value.(*cachedManifest)
		c.ll.Remove(el)
		delete(c.items, cm.key)
		c.entries -= len(cm.m.Entries)
	}
}

// lastSent remembers, per target node and worktree, the hash of the last
// manifest the target accepted: the base the next sync's delta refers to.
type lastSent struct {
	mu sync.Mutex
	m  map[string]string
}

func (l *lastSent) get(node, root string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m[node+"\x00"+root]
}

func (l *lastSent) set(node, root, hash string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[string]string{}
	}
	if len(l.m) > 4096 { // nodes × worktrees; never reached in practice
		clear(l.m)
	}
	l.m[node+"\x00"+root] = hash
}
