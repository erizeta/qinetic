// Package memory is the in-process qinetic.Storage used by middleware when none is configured.
package memory

import (
	"container/heap"
	"context"
	"errors"
	"strconv"
	"sync"
	"time"
)

// DefaultMaxEntries bounds the entries kept when Config.MaxEntries is zero.
const DefaultMaxEntries = 100_000

// sweepPerSet is how many expired entries a Set removes, so cleanup needs no goroutine.
const sweepPerSet = 8

// ErrClosed is returned after Close.
var ErrClosed = errors.New("qinetic/memory: storage closed")

// ErrTooLarge is returned by Set for an entry larger than Config.MaxBytes on its own.
var ErrTooLarge = errors.New("qinetic/memory: entry larger than MaxBytes")

// Config configures a Storage.
type Config struct {
	// MaxEntries bounds the number of entries, so a client cannot exhaust memory with new keys. When it is
	// reached, the entry closest to expiry is evicted; entries without expiry go last. Zero means
	// DefaultMaxEntries.
	MaxEntries int
	// MaxBytes bounds the total size of keys and values, evicting like MaxEntries. Zero means no bound
	// beyond MaxEntries; a cache of response bodies should set one.
	MaxBytes int64
	// Clock returns the current time. If nil, time.Now is used.
	Clock func() time.Time
}

type entry struct {
	val []byte
	exp time.Time // zero: no expiry
	idx int       // position in the heap
}

// Storage is a map guarded by a mutex, with a min-heap on expiry for sweeping and eviction.
type Storage struct {
	mu       sync.Mutex
	entries  map[string]*entry
	byExp    expHeap
	max      int
	maxBytes int64
	size     int64 // bytes of keys and values held
	now      func() time.Time
	closed   bool
}

// New returns an empty Storage. It starts no goroutines.
func New(cfg ...Config) *Storage {
	var c Config
	if len(cfg) > 0 {
		c = cfg[0]
	}
	if c.MaxEntries <= 0 {
		c.MaxEntries = DefaultMaxEntries
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
	return &Storage{entries: make(map[string]*entry), max: c.MaxEntries, maxBytes: c.MaxBytes, now: c.Clock}
}

func (e *entry) expired(now time.Time) bool { return !e.exp.IsZero() && !now.Before(e.exp) }

// Get returns a copy of the value of key, or nil when it is missing or expired.
func (s *Storage) Get(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	e, ok := s.entries[key]
	if !ok {
		return nil, nil
	}
	if e.expired(s.now()) {
		s.remove(key, e)
		return nil, nil
	}
	return append([]byte{}, e.val...), nil
}

// Set stores a copy of val. A ttl of zero or less means no expiry.
func (s *Storage) Set(_ context.Context, key string, val []byte, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	now := s.now()
	var exp time.Time
	if ttl > 0 {
		exp = now.Add(ttl)
	}
	if s.maxBytes > 0 && int64(len(key)+len(val)) > s.maxBytes {
		return ErrTooLarge
	}
	s.put(key, append([]byte{}, val...), exp, now)
	return nil
}

// Delete removes key.
func (s *Storage) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if e, ok := s.entries[key]; ok {
		s.remove(key, e)
	}
	return nil
}

// Incr adds delta to the decimal integer at key; see qinetic.Incrementer.
func (s *Storage) Incr(_ context.Context, key string, delta int64, ttl time.Duration) (int64, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, time.Time{}, ErrClosed
	}
	now := s.now()
	var n int64
	var exp time.Time
	if e, ok := s.entries[key]; ok && !e.expired(now) {
		v, err := strconv.ParseInt(string(e.val), 10, 64)
		if err != nil {
			return 0, time.Time{}, errors.New("qinetic/memory: Incr on a value that is not an integer")
		}
		n, exp = v, e.exp
	} else if ttl > 0 {
		exp = now.Add(ttl)
	}
	n += delta
	s.put(key, strconv.AppendInt(nil, n, 10), exp, now)
	return n, exp, nil
}

// Close marks the storage closed; later calls fail with ErrClosed. There is nothing to stop.
func (s *Storage) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.entries, s.byExp, s.size = nil, nil, 0
	return nil
}

// Len returns the number of entries, expired ones not yet removed included.
func (s *Storage) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

func (s *Storage) put(key string, val []byte, exp, now time.Time) {
	if e, ok := s.entries[key]; ok {
		s.size += int64(len(val) - len(e.val))
		e.val, e.exp = val, exp
		heap.Fix(&s.byExp, e.idx)
		s.evictBytes(e)
		return
	}
	// Expired entries sit at the top of the heap; drop a few on every new key.
	for range sweepPerSet {
		if len(s.byExp) == 0 || !s.byExp[0].e.expired(now) {
			break
		}
		top := s.byExp[0]
		s.remove(top.key, top.e)
	}
	if len(s.entries) >= s.max {
		top := s.byExp[0]
		s.remove(top.key, top.e)
	}
	e := &entry{val: val, exp: exp}
	s.entries[key] = e
	s.size += int64(len(key) + len(val))
	heap.Push(&s.byExp, heapItem{key: key, e: e})
	s.evictBytes(e)
}

// evictBytes evicts entries closest to expiry, never keep itself, until the total fits MaxBytes.
func (s *Storage) evictBytes(keep *entry) {
	for s.maxBytes > 0 && s.size > s.maxBytes {
		top := s.byExp[0]
		if top.e == keep {
			// keep is the next to go; evict the one after it instead.
			if len(s.byExp) == 1 {
				return
			}
			top = s.byExp[s.secondIdx()]
		}
		s.remove(top.key, top.e)
	}
}

// secondIdx is the heap index of the entry after the top: one of its two children.
func (s *Storage) secondIdx() int {
	if len(s.byExp) > 2 && s.byExp.Less(2, 1) {
		return 2
	}
	return 1
}

func (s *Storage) remove(key string, e *entry) {
	delete(s.entries, key)
	s.size -= int64(len(key) + len(e.val))
	heap.Remove(&s.byExp, e.idx)
}

type heapItem struct {
	key string
	e   *entry
}

// expHeap orders entries by expiry, with entries that never expire last.
type expHeap []heapItem

func (h expHeap) Len() int { return len(h) }

func (h expHeap) Less(i, j int) bool {
	a, b := h[i].e.exp, h[j].e.exp
	switch {
	case a.IsZero():
		return false
	case b.IsZero():
		return true
	}
	return a.Before(b)
}

func (h expHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].e.idx, h[j].e.idx = i, j
}

func (h *expHeap) Push(x any) {
	it := x.(heapItem)
	it.e.idx = len(*h)
	*h = append(*h, it)
}

func (h *expHeap) Pop() any {
	old := *h
	it := old[len(old)-1]
	old[len(old)-1] = heapItem{}
	*h = old[:len(old)-1]
	return it
}
