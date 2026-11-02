package memory

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/erizeta/qinetic"
	"go.uber.org/goleak"
)

var (
	_ qinetic.Storage     = (*Storage)(nil)
	_ qinetic.Incrementer = (*Storage)(nil)
)

// TestMain also proves the storage starts no goroutine of its own.
func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newStorage(limit int) (*Storage, *fakeClock) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	return New(Config{MaxEntries: limit, Clock: clk.now}), clk
}

var ctx = context.Background()

func TestGetSetDelete(t *testing.T) {
	s, clk := newStorage(0)
	val := []byte("v")
	if err := s.Set(ctx, "k", val, time.Second); err != nil {
		t.Fatal(err)
	}
	val[0] = 'x' // the storage keeps its own copy
	got, _ := s.Get(ctx, "k")
	if string(got) != "v" {
		t.Fatalf("Get = %q", got)
	}
	got[0] = 'y'
	if again, _ := s.Get(ctx, "k"); string(again) != "v" {
		t.Fatalf("Get returned the stored slice: %q", again)
	}
	if missing, err := s.Get(ctx, "nope"); missing != nil || err != nil {
		t.Fatalf("missing key: %q, %v", missing, err)
	}
	clk.advance(time.Second)
	if got, _ := s.Get(ctx, "k"); got != nil || s.Len() != 0 {
		t.Fatalf("expired key: %q, %d entries", got, s.Len())
	}
	_ = s.Set(ctx, "forever", nil, 0)
	clk.advance(1000 * time.Hour)
	if got, _ := s.Get(ctx, "forever"); got == nil {
		t.Fatal("entry without ttl expired")
	}
	_ = s.Delete(ctx, "forever")
	_ = s.Delete(ctx, "never-there")
	if s.Len() != 0 {
		t.Fatalf("%d entries after Delete", s.Len())
	}
}

func TestSetSweepsExpiredEntries(t *testing.T) {
	s, clk := newStorage(0)
	for i := range 20 {
		_ = s.Set(ctx, fmt.Sprint("old", i), []byte("x"), time.Second)
	}
	clk.advance(2 * time.Second)
	_ = s.Set(ctx, "new", []byte("x"), time.Minute)
	if s.Len() != 20-sweepPerSet+1 {
		t.Fatalf("%d entries, want %d after one sweep", s.Len(), 20-sweepPerSet+1)
	}
	for range 2 {
		_ = s.Set(ctx, fmt.Sprint("new", s.Len()), []byte("x"), time.Minute)
	}
	if s.Len() != 3 {
		t.Fatalf("%d entries, want the expired ones swept", s.Len())
	}
}

func TestMaxEntriesEvictsClosestToExpiry(t *testing.T) {
	s, _ := newStorage(3)
	_ = s.Set(ctx, "forever", []byte("x"), 0)
	_ = s.Set(ctx, "soon", []byte("x"), time.Second)
	_ = s.Set(ctx, "later", []byte("x"), time.Hour)
	_ = s.Set(ctx, "new", []byte("x"), time.Minute)
	if got, _ := s.Get(ctx, "soon"); got != nil || s.Len() != 3 {
		t.Fatalf("soon still there or %d entries", s.Len())
	}
	_ = s.Set(ctx, "newer", []byte("x"), time.Minute)
	_ = s.Set(ctx, "newest", []byte("x"), time.Minute)
	// Only entries without expiry are left to evict now.
	_ = s.Set(ctx, "last", []byte("x"), 0)
	if s.Len() != 3 {
		t.Fatalf("%d entries", s.Len())
	}
	// Updating an existing key never evicts.
	_ = s.Set(ctx, "last", []byte("y"), 0)
	if got, _ := s.Get(ctx, "last"); string(got) != "y" || s.Len() != 3 {
		t.Fatalf("update: %q, %d entries", got, s.Len())
	}
}

func TestIncr(t *testing.T) {
	s, clk := newStorage(0)
	n, exp, err := s.Incr(ctx, "c", 1, time.Minute)
	start := clk.now()
	if n != 1 || !exp.Equal(start.Add(time.Minute)) || err != nil {
		t.Fatalf("first Incr = %d %v %v", n, exp, err)
	}
	clk.advance(30 * time.Second)
	// The ttl only applies when the key is created.
	if n, exp, _ = s.Incr(ctx, "c", 5, time.Hour); n != 6 || !exp.Equal(start.Add(time.Minute)) {
		t.Fatalf("second Incr = %d %v", n, exp)
	}
	if n, _, _ = s.Incr(ctx, "c", -2, time.Hour); n != 4 {
		t.Fatalf("negative delta: %d", n)
	}
	if got, _ := s.Get(ctx, "c"); string(got) != "4" {
		t.Fatalf("Get on a counter = %q", got)
	}
	clk.advance(30 * time.Second)
	if n, exp, _ = s.Incr(ctx, "c", 1, time.Minute); n != 1 || !exp.Equal(clk.now().Add(time.Minute)) {
		t.Fatalf("Incr after expiry = %d %v", n, exp)
	}
	_ = s.Set(ctx, "text", []byte("abc"), 0)
	if _, _, err := s.Incr(ctx, "text", 1, 0); err == nil {
		t.Fatal("Incr on text succeeded")
	}
}

func TestClose(t *testing.T) {
	s, _ := newStorage(0)
	_ = s.Set(ctx, "k", []byte("v"), 0)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	_, gerr := s.Get(ctx, "k")
	_, _, ierr := s.Incr(ctx, "k", 1, 0)
	for _, err := range []error{gerr, s.Set(ctx, "k", nil, 0), s.Delete(ctx, "k"), ierr} {
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("after Close: %v", err)
		}
	}
}

func TestConcurrent(t *testing.T) {
	s, clk := newStorage(500)
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Go(func() {
			for i := range 2000 {
				key := fmt.Sprint(g, ":", i%700)
				switch i % 4 {
				case 0:
					_ = s.Set(ctx, key, []byte("v"), time.Duration(i%5)*time.Second)
				case 1:
					_, _ = s.Get(ctx, key)
				case 2:
					_, _, _ = s.Incr(ctx, fmt.Sprint("counter", i%10), 1, time.Minute)
				case 3:
					_ = s.Delete(ctx, key)
					clk.advance(time.Millisecond)
				}
			}
		})
	}
	wg.Wait()
	if s.Len() > 500 {
		t.Fatalf("%d entries, over MaxEntries", s.Len())
	}
}

func TestIncrConcurrentIsExact(t *testing.T) {
	s, _ := newStorage(0)
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			for range 500 {
				_, _, _ = s.Incr(ctx, "n", 1, time.Minute)
			}
		})
	}
	wg.Wait()
	if got, _ := s.Get(ctx, "n"); string(got) != "16000" {
		t.Fatalf("count %s, want 16000", got)
	}
}

func TestMaxBytes(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	s := New(Config{MaxBytes: 100, Clock: clk.now})
	val := make([]byte, 29) // 30 bytes with a one-letter key
	_ = s.Set(ctx, "a", val, time.Minute)
	_ = s.Set(ctx, "b", val, 2*time.Minute)
	_ = s.Set(ctx, "c", val, 3*time.Minute)
	_ = s.Set(ctx, "d", val, 4*time.Minute) // 120 bytes: "a", closest to expiry, goes
	if got, _ := s.Get(ctx, "a"); got != nil || s.Len() != 3 || s.size != 90 {
		t.Fatalf("after overflow: a=%v, %d entries, %d bytes", got, s.Len(), s.size)
	}
	// An entry written now is never evicted for itself, even when it expires first.
	_ = s.Set(ctx, "e", val, time.Second)
	if got, _ := s.Get(ctx, "e"); got == nil || s.size > 100 {
		t.Fatalf("new entry evicted, or %d bytes", s.size)
	}
	// Growing an entry in place evicts others too.
	_ = s.Set(ctx, "e", make([]byte, 69), time.Second)
	if got, _ := s.Get(ctx, "e"); len(got) != 69 || s.size > 100 {
		t.Fatalf("grown entry: %d bytes stored, %d total", len(got), s.size)
	}
	if err := s.Set(ctx, "huge", make([]byte, 100), 0); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized Set: %v", err)
	}
	_ = s.Delete(ctx, "e")
	if s.size != int64(30*(s.Len())) {
		t.Fatalf("size %d with %d entries after Delete", s.size, s.Len())
	}
}
