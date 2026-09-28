package decode

import (
	"container/list"
	"encoding/json"
	"errors"
	"hash/fnv"
	"sync"
	"sync/atomic"
)

// DefaultCacheSize is the number of entries a CachingDecoder keeps when the
// constructor is passed a non-positive maxEntries.
const DefaultCacheSize = 4096

// CachingDecoder memoizes the output of another Decoder keyed by a hash of the
// raw XDR payload. Decoding is the hot path during ingestion and the same
// payloads recur constantly within a page: every event of a given kind shares
// its topic symbols (e.g. {"symbol":"transfer"}) and many share whole values.
// Re-decoding identical XDR is pure waste, so a small LRU collapses it to a
// map lookup.
//
// Keys are the FNV-1a 64-bit hash of the base64 XDR; the original string is
// stored alongside the decoded value and compared on every hit, so the
// astronomically unlikely collision degrades to a cache miss instead of
// returning the wrong JSON.
//
// The zero value is not usable; call NewCachingDecoder. A CachingDecoder is
// safe for concurrent use (the window sweep decodes from several goroutines).
type CachingDecoder struct {
	inner Decoder

	mu         sync.Mutex
	maxEntries int
	entries    map[uint64]*list.Element
	order      *list.List // front = most recently used

	hits   atomic.Uint64
	misses atomic.Uint64
}

type cacheEntry struct {
	key   string // the exact XDR the hash was computed from
	value json.RawMessage
}

// NewCachingDecoder wraps inner in an LRU cache. A maxEntries <= 0 selects
// DefaultCacheSize. A nil inner decoder is tolerated at construction and
// reported as an error on the first decode, so callers wiring optional
// dependencies don't have to pre-check.
func NewCachingDecoder(inner Decoder, maxEntries int) *CachingDecoder {
	if maxEntries <= 0 {
		maxEntries = DefaultCacheSize
	}
	return &CachingDecoder{
		inner:      inner,
		maxEntries: maxEntries,
		entries:    make(map[uint64]*list.Element, maxEntries),
		order:      list.New(),
	}
}

// DecodeScVal returns the cached JSON for base64XDR, decoding and caching it on
// a miss. Decode errors are never cached: a transient failure (or a decoder
// that grows a fix) must be retryable on the next call. Returned bytes are
// copies, so a caller mutating them cannot poison the cache.
func (c *CachingDecoder) DecodeScVal(base64XDR string) (json.RawMessage, error) {
	if c.inner == nil {
		return nil, errors.New("decode: caching decoder has no inner decoder")
	}
	key := cacheKey(base64XDR)
	if v, ok := c.get(key, base64XDR); ok {
		c.hits.Add(1)
		return cloneRaw(v), nil
	}
	c.misses.Add(1)
	v, err := c.inner.DecodeScVal(base64XDR)
	if err != nil {
		return nil, err
	}
	c.put(key, base64XDR, v)
	return cloneRaw(v), nil
}

// Stats reports the cumulative cache hits and misses since construction.
func (c *CachingDecoder) Stats() (hits, misses uint64) {
	return c.hits.Load(), c.misses.Load()
}

// Len reports the number of entries currently cached.
func (c *CachingDecoder) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// get returns the cached value for key, verifying the stored raw key matches so
// a hash collision is treated as a miss. A hit promotes the entry to MRU.
func (c *CachingDecoder) get(key uint64, raw string) (json.RawMessage, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	entry := el.Value.(*cacheEntry)
	if entry.key != raw {
		return nil, false
	}
	c.order.MoveToFront(el)
	return entry.value, true
}

// put inserts or refreshes an entry, evicting the least-recently-used entry
// once the cache is at capacity.
func (c *CachingDecoder) put(key uint64, raw string, value json.RawMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		entry := el.Value.(*cacheEntry)
		// Same key, same raw (get already verified) — refresh both.
		entry.value = value
		c.order.MoveToFront(el)
		return
	}
	el := c.order.PushFront(&cacheEntry{key: raw, value: value})
	c.entries[key] = el
	for c.order.Len() > c.maxEntries {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.order.Remove(oldest)
		delete(c.entries, cacheKey(oldest.Value.(*cacheEntry).key))
	}
}

// cacheKey hashes the raw base64 XDR with FNV-1a. The hash is only a bucket
// selector; correctness relies on the full-key comparison in get.
func cacheKey(raw string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(raw))
	return h.Sum64()
}

func cloneRaw(v json.RawMessage) json.RawMessage {
	if v == nil {
		return nil
	}
	return append(json.RawMessage(nil), v...)
}
