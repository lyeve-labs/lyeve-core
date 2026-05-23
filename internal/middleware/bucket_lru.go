package middleware

import "container/list"

// bucketLRU holds a limiter's token buckets in recency order, so evicting the
// least recently used one at capacity is constant time. Scanning every bucket
// under the lock to find the oldest would, past capacity, let a caller who
// presents a new key on each request (a rotating address, or any IPv6 host)
// make every request pay for that scan while holding the lock every other
// request waits on. The caller serializes access.
type bucketLRU struct {
	max   int
	order *list.List // front is the most recently used
	items map[string]*list.Element
}

type lruEntry struct {
	key    string
	bucket *tokenBucket
}

func newBucketLRU(max int) *bucketLRU {
	return &bucketLRU{max: max, order: list.New(), items: make(map[string]*list.Element)}
}

// get returns the bucket for key and marks it most recently used.
func (c *bucketLRU) get(key string) (*tokenBucket, bool) {
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*lruEntry).bucket, true
}

// add stores b under key, evicting the least recently used bucket first when
// the cache is full.
func (c *bucketLRU) add(key string, b *tokenBucket) {
	if c.max > 0 && c.order.Len() >= c.max {
		if oldest := c.order.Back(); oldest != nil {
			c.order.Remove(oldest)
			delete(c.items, oldest.Value.(*lruEntry).key)
		}
	}
	c.items[key] = c.order.PushFront(&lruEntry{key: key, bucket: b})
}

// has reports whether key is held, without touching its recency.
func (c *bucketLRU) has(key string) bool {
	_, ok := c.items[key]
	return ok
}

func (c *bucketLRU) len() int { return c.order.Len() }

// removeIf drops every bucket stale reports true for, walking from the least
// recently used end.
func (c *bucketLRU) removeIf(stale func(*tokenBucket) bool) {
	for el := c.order.Back(); el != nil; {
		prev := el.Prev()
		entry := el.Value.(*lruEntry)
		if stale(entry.bucket) {
			c.order.Remove(el)
			delete(c.items, entry.key)
		}
		el = prev
	}
}
