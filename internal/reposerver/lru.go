package reposerver

import (
	"container/list"
	"sync"
	"time"
)

// lru is a least-recently-used cache bounded by entry count and total size, with a
// per-entry expiry. Values must be treated as immutable once added. It is safe for
// concurrent use; onEvict runs with the cache lock held and must not call back into
// the cache.
type lru[V any] struct {
	mu         sync.Mutex
	maxEntries int
	maxBytes   int64
	now        func() time.Time
	onEvict    func(key string, v V)

	ll    *list.List
	items map[string]*list.Element
	bytes int64
}

type lruItem[V any] struct {
	key     string
	val     V
	size    int64
	expires time.Time
}

func newLRU[V any](maxEntries int, maxBytes int64, now func() time.Time) *lru[V] {
	if now == nil {
		now = time.Now
	}
	return &lru[V]{maxEntries: maxEntries, maxBytes: maxBytes, now: now, ll: list.New(), items: map[string]*list.Element{}}
}

// Get returns the live value of key and marks it recently used.
func (c *lru[V]) Get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero V
	el, ok := c.items[key]
	if !ok {
		return zero, false
	}
	it := el.Value.(*lruItem[V])
	if !it.expires.IsZero() && !c.now().Before(it.expires) {
		c.removeElement(el)
		return zero, false
	}
	c.ll.MoveToFront(el)
	return it.val, true
}

// Add stores key (replacing any previous value) for ttl (zero = no expiry) and evicts
// least recently used entries over the bounds. It returns false (and stores nothing)
// when the value alone exceeds the size bound.
func (c *lru[V]) Add(key string, v V, size int64, ttl time.Duration) bool {
	if size < 0 {
		size = 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.removeElement(el)
	}
	if c.maxBytes > 0 && size > c.maxBytes {
		return false
	}
	it := &lruItem[V]{key: key, val: v, size: size}
	if ttl > 0 {
		it.expires = c.now().Add(ttl)
	}
	c.items[key] = c.ll.PushFront(it)
	c.bytes += size
	for (c.maxEntries > 0 && c.ll.Len() > c.maxEntries) || (c.maxBytes > 0 && c.bytes > c.maxBytes) {
		back := c.ll.Back()
		if back == nil {
			break
		}
		c.removeElement(back)
	}
	return true
}

// Remove deletes key.
func (c *lru[V]) Remove(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.removeElement(el)
	}
}

// Len and Bytes report the current usage (expired entries included until touched).
func (c *lru[V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

func (c *lru[V]) Bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}

func (c *lru[V]) removeElement(el *list.Element) {
	it := el.Value.(*lruItem[V])
	c.ll.Remove(el)
	delete(c.items, it.key)
	c.bytes -= it.size
	if c.onEvict != nil {
		c.onEvict(it.key, it.val)
	}
}
