package limiter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erizeta/qinetic"
	"github.com/erizeta/qinetic/internal/memory"
)

// clock is the fake time behind now; windows start on whole minutes from t0.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

var t0 = time.Unix(6000, 0)

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(d time.Duration) {
	c.mu.Lock()
	c.t = t0.Add(d)
	c.mu.Unlock()
}

func useClock(t *testing.T) *clock {
	clk := &clock{t: t0}
	now = clk.now
	t.Cleanup(func() { now = time.Now })
	return clk
}

// plainStorage hides memory's Incr, standing in for an external Storage without atomic counters.
type plainStorage struct{ m *memory.Storage }

func (p plainStorage) Get(ctx context.Context, k string) ([]byte, error) { return p.m.Get(ctx, k) }
func (p plainStorage) Set(ctx context.Context, k string, v []byte, ttl time.Duration) error {
	return p.m.Set(ctx, k, v, ttl)
}
func (p plainStorage) Delete(ctx context.Context, k string) error { return p.m.Delete(ctx, k) }
func (p plainStorage) Close() error                               { return p.m.Close() }

// storages returns both counting paths: atomic Incr, and Get/Set under the striped lock.
func storages(clk *clock) map[string]qinetic.Storage {
	return map[string]qinetic.Storage{
		"incrementer": memory.New(memory.Config{Clock: clk.now}),
		"plain":       plainStorage{memory.New(memory.Config{Clock: clk.now})},
	}
}

type result struct {
	code                    int
	limit, remaining, reset string
	retryAfter              string
}

func hit(t *testing.T, app *qinetic.App, target string) result {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest("GET", target, nil))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	h := resp.Header
	return result{resp.StatusCode, h.Get("X-RateLimit-Limit"), h.Get("X-RateLimit-Remaining"),
		h.Get("X-RateLimit-Reset"), h.Get("Retry-After")}
}

func newApp(cfg Config) *qinetic.App {
	app := qinetic.New(qinetic.Config{Logger: slog.New(slog.DiscardHandler)})
	app.Use(New(cfg))
	app.Get("/", func(c *qinetic.Ctx) error { return c.SendString("ok") })
	app.Get("/fail", func(c *qinetic.Ctx) error { return qinetic.ErrBadRequest })
	app.Get("/boom", func(c *qinetic.Ctx) error { return errors.New("internal") })
	return app
}

func TestFixedWindow(t *testing.T) {
	clk := useClock(t)
	for name, st := range storages(clk) {
		t.Run(name, func(t *testing.T) {
			clk.set(0)
			app := newApp(Config{Max: 2, Expiration: time.Minute, Storage: st})
			if r := hit(t, app, "/"); r != (result{200, "2", "1", "60", ""}) {
				t.Fatalf("first: %+v", r)
			}
			clk.set(10 * time.Second)
			if r := hit(t, app, "/"); r != (result{200, "2", "0", "50", ""}) {
				t.Fatalf("second: %+v", r)
			}
			for range 5 {
				if r := hit(t, app, "/"); r != (result{429, "2", "0", "50", "50"}) {
					t.Fatalf("over the limit: %+v", r)
				}
			}
			// The window started with the first request, and the rejected ones used no quota.
			clk.set(60 * time.Second)
			if r := hit(t, app, "/"); r != (result{200, "2", "1", "60", ""}) {
				t.Fatalf("new window: %+v", r)
			}
		})
	}
}

// TestSlidingWindowHoldsBoundaryBurst sends Max requests just before a window ends: a fixed window would
// allow Max more right after, the sliding window does not.
func TestSlidingWindowHoldsBoundaryBurst(t *testing.T) {
	clk := useClock(t)
	for name, st := range storages(clk) {
		t.Run(name, func(t *testing.T) {
			app := newApp(Config{Max: 10, Expiration: time.Minute, Algorithm: SlidingWindow, Storage: st})
			clk.set(59 * time.Second)
			for i := range 10 {
				if r := hit(t, app, "/"); r.code != 200 || r.remaining != fmt.Sprint(9-i) || r.reset != "1" {
					t.Fatalf("burst request %d: %+v", i, r)
				}
			}
			// The current window is full: wait 1 s for the next one, then 6 s for this one's weight to
			// fall to 9/10, the same moment the request below at 66 s gets through.
			if r := hit(t, app, "/"); r.code != 429 || r.retryAfter != "7" {
				t.Fatalf("11th in the same window: %+v", r)
			}
			// One second into the next window, the previous one still weighs 59/60 of 10.
			clk.set(61 * time.Second)
			r := hit(t, app, "/")
			// 1 + 10×(59/60) > 10; the weight falls to 9/10, allowing 1 more, at 6 s into the window.
			if r.code != 429 || r.retryAfter != "5" || r.reset != "59" {
				t.Fatalf("after the boundary: %+v", r)
			}
			clk.set(65 * time.Second)
			if r := hit(t, app, "/"); r.code != 429 {
				t.Fatalf("before Retry-After: %+v", r)
			}
			clk.set(66 * time.Second)
			if r := hit(t, app, "/"); r.code != 200 || r.remaining != "0" {
				t.Fatalf("at Retry-After: %+v", r)
			}
			// Two windows later the old burst is gone entirely.
			clk.set(180 * time.Second)
			if r := hit(t, app, "/"); r.code != 200 || r.remaining != "9" {
				t.Fatalf("two windows later: %+v", r)
			}
		})
	}
}

func TestSlidingWindowRetryAfterWhenCurrentWindowIsFull(t *testing.T) {
	clk := useClock(t)
	app := newApp(Config{Max: 4, Expiration: time.Minute, Algorithm: SlidingWindow})
	clk.set(0)
	for range 4 {
		hit(t, app, "/")
	}
	// The current window holds 4 = Max; in the next one it weighs 4×(1−t/60), allowing one more
	// request once that is 3, 15 s in: 60 s + 15 s from now.
	if r := hit(t, app, "/"); r.code != 429 || r.retryAfter != "75" {
		t.Fatalf("got %+v", r)
	}
	clk.set(75 * time.Second)
	if r := hit(t, app, "/"); r.code != 200 {
		t.Fatalf("at Retry-After: %+v", r)
	}
}

func TestSkipRequests(t *testing.T) {
	clk := useClock(t)
	for _, alg := range []Algorithm{FixedWindow, SlidingWindow} {
		for name, st := range storages(clk) {
			t.Run(fmt.Sprint(alg, name), func(t *testing.T) {
				clk.set(0)
				failed := newApp(Config{Max: 2, Algorithm: alg, Storage: st, SkipFailedRequests: true})
				for _, target := range []string{"/fail", "/boom", "/fail", "/", "/"} {
					if r := hit(t, failed, target); r.code == 429 {
						t.Fatalf("%s counted although it failed: %+v", target, r)
					}
				}
				if r := hit(t, failed, "/"); r.code != 429 {
					t.Fatalf("third success: %+v", r)
				}
				clk.set(10 * time.Minute)
				ok := newApp(Config{Max: 1, Algorithm: alg, Storage: st, SkipSuccessfulRequests: true,
					KeyGenerator: func(*qinetic.Ctx) string { return "skip-ok" }})
				for range 3 {
					if r := hit(t, ok, "/"); r.code != 200 {
						t.Fatalf("success counted: %+v", r)
					}
				}
				hit(t, ok, "/fail")
				if r := hit(t, ok, "/fail"); r.code != 429 {
					t.Fatalf("second failure: %+v", r)
				}
			})
		}
	}
}

func TestConfigHooks(t *testing.T) {
	useClock(t)
	app := qinetic.New()
	app.Use(New(Config{
		Max:          1,
		KeyGenerator: func(c *qinetic.Ctx) string { return c.Get("X-User") },
		LimitReached: func(c *qinetic.Ctx) error { return c.Status(503).SendString("slow down") },
		Next:         func(c *qinetic.Ctx) bool { return c.Get("X-Admin") != "" },
	}))
	app.Get("/", func(c *qinetic.Ctx) error { return nil })
	send := func(hdr ...string) (int, string) {
		req := httptest.NewRequest("GET", "/", nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, _ := app.Test(req)
		defer func() { _ = resp.Body.Close() }()
		b := make([]byte, 64)
		n, _ := resp.Body.Read(b)
		return resp.StatusCode, string(b[:n])
	}
	send("X-User", "a")
	if code, body := send("X-User", "a"); code != 503 || body != "slow down" {
		t.Fatalf("LimitReached: %d %q", code, body)
	}
	if code, _ := send("X-User", "b"); code != 200 {
		t.Fatalf("other key: %d", code)
	}
	if code, _ := send("X-User", "a", "X-Admin", "1"); code != 200 {
		t.Fatalf("Next: %d", code)
	}
}

func TestDefaults(t *testing.T) {
	useClock(t)
	app := newApp(Config{})
	for range 5 {
		hit(t, app, "/")
	}
	if r := hit(t, app, "/"); r.code != 429 || r.limit != "5" || r.retryAfter != "60" {
		t.Fatalf("got %+v", r)
	}
}

// TestConcurrentExactCount checks no request slips past the limit when many arrive at once.
func TestConcurrentExactCount(t *testing.T) {
	clk := useClock(t)
	for _, alg := range []Algorithm{FixedWindow, SlidingWindow} {
		for name, st := range storages(clk) {
			t.Run(fmt.Sprint(alg, name), func(t *testing.T) {
				clk.set(time.Duration(alg) * time.Hour)
				app := newApp(Config{Max: 100, Algorithm: alg, Storage: st})
				var allowed atomic.Int64
				var wg sync.WaitGroup
				for range 50 {
					wg.Go(func() {
						for range 10 {
							if r := hit(t, app, "/"); r.code == 200 {
								allowed.Add(1)
							}
						}
					})
				}
				wg.Wait()
				if allowed.Load() != 100 {
					t.Fatalf("%d requests allowed, want 100", allowed.Load())
				}
			})
		}
	}
}

type failingStorage struct{ plainStorage }

func (failingStorage) Get(context.Context, string) ([]byte, error) {
	return nil, errors.New("redis down")
}

func TestStorageErrors(t *testing.T) {
	clk := useClock(t)
	broken := failingStorage{plainStorage{memory.New(memory.Config{Clock: clk.now})}}
	if r := hit(t, newApp(Config{Storage: broken}), "/"); r.code != 500 {
		t.Fatalf("storage error: %+v", r)
	}
	m := memory.New(memory.Config{Clock: clk.now})
	_ = m.Set(context.Background(), keyPrefix+"192.0.2.1", []byte("garbage"), time.Minute)
	if r := hit(t, newApp(Config{Storage: plainStorage{m}}), "/"); r.code != 500 {
		t.Fatalf("malformed counter: %+v", r)
	}
}
