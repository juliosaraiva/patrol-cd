package github

import (
	"container/list"
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// fetchTimeout bounds a shared (singleflight) fetch, which runs detached from the first
// caller's cancellation so that other waiters are not failed by it.
const fetchTimeout = 30 * time.Second

// lruCache is a small bounded LRU with per-key singleflight loading. Validity (expiry) is
// decided by the caller at read time so callers can apply their own clock.
type lruCache[V any] struct {
	mu    sync.Mutex
	cap   int
	ll    *list.List
	items map[string]*list.Element
	sf    singleflight.Group
}

type lruEntry[V any] struct {
	key string
	val V
}

func newLRUCache[V any](capacity int) *lruCache[V] {
	return &lruCache[V]{cap: capacity, ll: list.New(), items: map[string]*list.Element{}}
}

func (c *lruCache[V]) get(key string, valid func(V) bool) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero V
	el, ok := c.items[key]
	if !ok {
		return zero, false
	}
	e := el.Value.(*lruEntry[V])
	if valid != nil && !valid(e.val) {
		return zero, false
	}
	c.ll.MoveToFront(el)
	return e.val, true
}

func (c *lruCache[V]) put(key string, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		el.Value.(*lruEntry[V]).val = v
		c.ll.MoveToFront(el)
		return
	}
	c.items[key] = c.ll.PushFront(&lruEntry[V]{key: key, val: v})
	for c.ll.Len() > c.cap {
		back := c.ll.Back()
		c.ll.Remove(back)
		delete(c.items, back.Value.(*lruEntry[V]).key)
	}
}

// removeIf deletes key when match reports true for the stored value.
func (c *lruCache[V]) removeIf(key string, match func(V) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok && match(el.Value.(*lruEntry[V]).val) {
		c.ll.Remove(el)
		delete(c.items, key)
	}
}

// load returns a valid cached value or fetches one, sharing concurrent fetches for the
// same key. Waiters honor their own ctx.
func (c *lruCache[V]) load(ctx context.Context, key string, valid func(V) bool, fetch func(context.Context) (V, error)) (V, error) {
	var zero V
	if v, ok := c.get(key, valid); ok {
		return v, nil
	}
	ch := c.sf.DoChan(key, func() (any, error) {
		if v, ok := c.get(key, valid); ok {
			return v, nil
		}
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
		defer cancel()
		v, err := fetch(fctx)
		if err != nil {
			return nil, err
		}
		c.put(key, v)
		return v, nil
	})
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case r := <-ch:
		if r.Err != nil {
			return zero, r.Err
		}
		return r.Val.(V), nil
	}
}
