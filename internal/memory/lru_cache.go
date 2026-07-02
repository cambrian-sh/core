package memory

import "container/list"

// lruCache is a generic bounded least-recently-used cache.
// Not safe for concurrent use — callers must hold their own lock.
type lruCache[K comparable, V any] struct {
	cap   int
	items map[K]*list.Element
	order *list.List // front = most recently used
}

type lruEntry[K comparable, V any] struct {
	key K
	val V
}

func newLRUCache[K comparable, V any](capacity int) *lruCache[K, V] {
	if capacity <= 0 {
		capacity = 100
	}
	return &lruCache[K, V]{
		cap:   capacity,
		items: make(map[K]*list.Element, capacity),
		order: list.New(),
	}
}

// Get returns (value, true) if key exists, promoting it to MRU position.
func (c *lruCache[K, V]) Get(key K) (V, bool) {
	el, ok := c.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*lruEntry[K, V]).val, true
}

// Put inserts or updates key→val, evicting the LRU entry if over capacity.
func (c *lruCache[K, V]) Put(key K, val V) {
	if el, ok := c.items[key]; ok {
		c.order.MoveToFront(el)
		el.Value.(*lruEntry[K, V]).val = val
		return
	}
	if c.order.Len() >= c.cap {
		// Evict least recently used (back of list).
		lru := c.order.Back()
		if lru != nil {
			c.order.Remove(lru)
			delete(c.items, lru.Value.(*lruEntry[K, V]).key)
		}
	}
	el := c.order.PushFront(&lruEntry[K, V]{key: key, val: val})
	c.items[key] = el
}

// Clear removes all entries.
func (c *lruCache[K, V]) Clear() {
	c.items = make(map[K]*list.Element, c.cap)
	c.order.Init()
}

// Len returns the number of cached entries.
func (c *lruCache[K, V]) Len() int { return c.order.Len() }
