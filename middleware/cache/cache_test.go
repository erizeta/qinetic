package cache

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erizeta/qinetic"
	"github.com/erizeta/qinetic/internal/memory"
	"github.com/erizeta/qinetic/middleware/compress"
	"github.com/erizeta/qinetic/middleware/cors"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func useClock(t *testing.T) *clock {
	clk := &clock{t: time.Unix(1_000_000, 0)}
	now = clk.now
	t.Cleanup(func() { now = time.Now })
	return clk
}

// plainStorage stands in for an external Storage, such as Redis.
type plainStorage struct{ m *memory.Storage }

func (p plainStorage) Get(ctx context.Context, k string) ([]byte, error) { return p.m.Get(ctx, k) }
func (p plainStorage) Set(ctx context.Context, k string, v []byte, ttl time.Duration) error {
	return p.m.Set(ctx, k, v, ttl)
}
func (p plainStorage) Delete(ctx context.Context, k string) error { return p.m.Delete(ctx, k) }
func (p plainStorage) Close() error                               { return p.m.Close() }

func storages(clk *clock) map[string]qinetic.Storage {
	return map[string]qinetic.Storage{
		"memory": nil,
		"plain":  plainStorage{memory.New(memory.Config{Clock: clk.now})},
	}
}

type result struct {
	code   int
	cache  string
	body   string
	header http.Header
}

func get(t *testing.T, app *qinetic.App, method, target string, hdr ...string) result {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header[http.CanonicalHeaderKey(hdr[i])] = append(req.Header[http.CanonicalHeaderKey(hdr[i])], hdr[i+1])
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return result{resp.StatusCode, resp.Header.Get("X-Cache"), string(b), resp.Header}
}

// counted returns a handler that numbers its calls, so a hit is visible as an unchanged number.
func counted(fn func(c *qinetic.Ctx, n int64) error) (qinetic.Handler, *atomic.Int64) {
	var calls atomic.Int64
	return func(c *qinetic.Ctx) error { return fn(c, calls.Add(1)) }, &calls
}

func TestHitMissExpiry(t *testing.T) {
	clk := useClock(t)
	for name, st := range storages(clk) {
		t.Run(name, func(t *testing.T) {
			app := qinetic.New()
			app.Use(New(Config{Storage: st, Expiration: time.Minute}))
			h, calls := counted(func(c *qinetic.Ctx, n int64) error {
				c.Set("ETag", `"v1"`)
				return c.Type("json").SendString(fmt.Sprintf(`{"n":%d}`, n))
			})
			app.Get("/data", h)
			if r := get(t, app, "GET", "/data?x=1"); r.code != 200 || r.cache != "miss" || r.body != `{"n":1}` {
				t.Fatalf("first: %+v", r)
			}
			r := get(t, app, "GET", "/data?x=1")
			if r.code != 200 || r.cache != "hit" || r.body != `{"n":1}` || r.header.Get("Content-Type") != "application/json" ||
				r.header.Get("ETag") != `"v1"` || r.header.Get("Content-Length") != "7" || calls.Load() != 1 {
				t.Fatalf("hit: %+v, calls %d", r, calls.Load())
			}
			if r := get(t, app, "HEAD", "/data?x=1"); r.cache != "hit" || r.body != "" || r.header.Get("Content-Length") != "7" {
				t.Fatalf("HEAD hit: %+v", r)
			}
			if r := get(t, app, "GET", "/data?x=2"); r.cache != "miss" || r.body != `{"n":2}` {
				t.Fatalf("other query: %+v", r)
			}
			clk.advance(time.Minute)
			if r := get(t, app, "GET", "/data?x=1"); r.cache != "miss" || r.body != `{"n":3}` {
				t.Fatalf("expired: %+v", r)
			}
		})
	}
}

func TestNotStored(t *testing.T) {
	useClock(t)
	app := qinetic.New(qinetic.Config{Logger: slog.New(slog.DiscardHandler)})
	app.Use(New(Config{MaxBytes: 10}))
	routes := map[string]func(c *qinetic.Ctx) error{
		"/cookie":    func(c *qinetic.Ctx) error { c.Cookie(&qinetic.Cookie{Name: "s", Value: "1"}); return nil },
		"/private":   func(c *qinetic.Ctx) error { c.Set("Cache-Control", "private, max-age=60"); return nil },
		"/nostore":   func(c *qinetic.Ctx) error { c.Set("Cache-Control", "no-store"); return nil },
		"/nocache":   func(c *qinetic.Ctx) error { c.Set("Cache-Control", "No-Cache"); return nil },
		"/varystar":  func(c *qinetic.Ctx) error { c.Set("Vary", "*"); return nil },
		"/notfound":  func(c *qinetic.Ctx) error { return c.SendStatus(404) },
		"/error":     func(c *qinetic.Ctx) error { return errors.New("boom") },
		"/big":       func(c *qinetic.Ctx) error { return c.SendString("more than ten bytes") },
		"/streamed":  func(c *qinetic.Ctx) error { _, err := c.Writer().Write([]byte("s")); return err },
		"/manyvary":  func(c *qinetic.Ctx) error { c.Set("Vary", "a, b, c, d, e, f, g, h, i"); return nil },
		"/plain":     func(c *qinetic.Ctx) error { return nil },
		"/head-only": func(c *qinetic.Ctx) error { return nil },
	}
	calls := map[string]int{}
	var mu sync.Mutex
	for path, fn := range routes {
		app.Get(path, func(c *qinetic.Ctx) error {
			mu.Lock()
			calls[path]++
			mu.Unlock()
			return fn(c)
		})
	}
	for path := range routes {
		if path == "/plain" || path == "/head-only" {
			continue
		}
		get(t, app, "GET", path)
		get(t, app, "GET", path)
		if calls[path] != 2 {
			t.Errorf("%s: handler ran %d times, want the response not stored", path, calls[path])
		}
	}
	// Request side: Authorization and no-store bypass the cache both ways.
	get(t, app, "GET", "/plain")
	for _, hdr := range [][]string{{"Authorization", "Bearer x"}, {"Cache-Control", "no-store"}} {
		if r := get(t, app, "GET", "/plain", hdr...); r.cache != "" {
			t.Errorf("%s: X-Cache %q, want the cache skipped", hdr[0], r.cache)
		}
	}
	if r := get(t, app, "GET", "/plain"); r.cache != "hit" || calls["/plain"] != 3 {
		t.Errorf("plain: %+v, %d calls", r, calls["/plain"])
	}
	// A HEAD miss is not stored: its handler may send no body.
	get(t, app, "HEAD", "/head-only")
	if r := get(t, app, "GET", "/head-only"); r.cache != "miss" {
		t.Errorf("GET after HEAD: %+v", r)
	}
	if r := get(t, app, "POST", "/plain"); r.cache != "" {
		t.Errorf("POST: %+v", r)
	}
}

func TestStatusCodesAndKey(t *testing.T) {
	useClock(t)
	app := qinetic.New()
	app.Use(New(Config{StatusCodes: []int{200, 404}, KeyGenerator: func(c *qinetic.Ctx) string { return c.Path() },
		Next: func(c *qinetic.Ctx) bool { return c.Get("X-Skip") != "" }}))
	h, calls := counted(func(c *qinetic.Ctx, n int64) error { return c.Status(404).SendString(fmt.Sprint(n)) })
	app.Get("/gone", h)
	get(t, app, "GET", "/gone?a=1")
	if r := get(t, app, "GET", "/gone?b=2"); r.code != 404 || r.cache != "hit" || calls.Load() != 1 {
		t.Fatalf("404 under a path-only key: %+v", r)
	}
	if r := get(t, app, "GET", "/gone", "X-Skip", "1"); r.cache != "" || calls.Load() != 2 {
		t.Fatalf("Next: %+v", r)
	}
}

func TestVaryOrigin(t *testing.T) {
	clk := useClock(t)
	for name, st := range storages(clk) {
		t.Run(name, func(t *testing.T) {
			app := qinetic.New()
			// cors runs before the cache and sets Vary: Origin and its own headers on every request.
			app.Use(cors.New(cors.Config{AllowOrigins: []string{"https://a.example", "https://b.example"}}))
			app.Use(New(Config{Storage: st}))
			h, calls := counted(func(c *qinetic.Ctx, n int64) error {
				return c.SendString(fmt.Sprintf("for %s #%d", c.Get("Origin"), n))
			})
			app.Get("/", h)
			a := get(t, app, "GET", "/", "Origin", "https://a.example")
			if a.cache != "miss" || a.body != "for https://a.example #1" {
				t.Fatalf("A miss: %+v", a)
			}
			b := get(t, app, "GET", "/", "Origin", "https://b.example")
			if b.cache != "miss" || b.body != "for https://b.example #2" {
				t.Fatalf("B must not get A's copy: %+v", b)
			}
			a = get(t, app, "GET", "/", "Origin", "https://a.example")
			b = get(t, app, "GET", "/", "Origin", "https://b.example")
			if a.cache != "hit" || a.body != "for https://a.example #1" || b.cache != "hit" || b.body != "for https://b.example #2" {
				t.Fatalf("hits: A %+v, B %+v", a, b)
			}
			// cors headers are its own, set once per request, not replayed from the copy.
			if got := a.header.Values("Access-Control-Allow-Origin"); len(got) != 1 || got[0] != "https://a.example" {
				t.Fatalf("A's CORS header: %q", got)
			}
			if r := get(t, app, "GET", "/"); r.cache != "miss" || calls.Load() != 3 {
				t.Fatalf("no Origin: %+v", r)
			}
		})
	}
}

func TestVaryAbsentVersusEmpty(t *testing.T) {
	useClock(t)
	app := qinetic.New()
	app.Use(New())
	h, _ := counted(func(c *qinetic.Ctx, n int64) error {
		c.Set("Vary", "X-Lang")
		return c.SendString(fmt.Sprint(n))
	})
	app.Get("/", h)
	get(t, app, "GET", "/")
	if r := get(t, app, "GET", "/", "X-Lang", ""); r.cache != "miss" || r.body != "2" {
		t.Fatalf("empty header served the absent one's copy: %+v", r)
	}
	if r := get(t, app, "GET", "/"); r.cache != "hit" || r.body != "1" {
		t.Fatalf("absent: %+v", r)
	}
	if r := get(t, app, "GET", "/", "X-Lang", "  "); r.cache != "hit" || r.body != "2" {
		t.Fatalf("whitespace-only is trimmed to empty: %+v", r)
	}
	// Repeated lines are one value: "a, b" from one line equals "a" and "b" from two.
	get(t, app, "GET", "/", "X-Lang", "a", "X-Lang", "b")
	if r := get(t, app, "GET", "/", "X-Lang", "a,b"); r.cache != "hit" {
		t.Fatalf("combined lines: %+v", r)
	}
}

func TestManifestChangesWithVary(t *testing.T) {
	useClock(t)
	vary := "Origin"
	var mu sync.Mutex
	app := qinetic.New()
	app.Use(New())
	h, _ := counted(func(c *qinetic.Ctx, n int64) error {
		mu.Lock()
		c.Set("Vary", vary)
		mu.Unlock()
		return c.SendString(fmt.Sprint(n))
	})
	app.Get("/", h)
	get(t, app, "GET", "/", "Origin", "https://a.example", "Accept-Language", "en")
	if r := get(t, app, "GET", "/", "Origin", "https://a.example", "Accept-Language", "fr"); r.cache != "hit" {
		t.Fatalf("Vary: Origin ignores the language: %+v", r)
	}
	mu.Lock()
	vary = "Accept-Language, Origin"
	mu.Unlock()
	// A new response varying on more headers replaces the manifest...
	get(t, app, "GET", "/", "Origin", "https://b.example", "Accept-Language", "fr")
	// ...so the old copy, keyed on Origin alone, is no longer found.
	if r := get(t, app, "GET", "/", "Origin", "https://a.example", "Accept-Language", "en"); r.cache != "miss" || r.body != "3" {
		t.Fatalf("after Vary changed: %+v", r)
	}
	if r := get(t, app, "GET", "/", "Origin", "https://b.example", "Accept-Language", "fr"); r.cache != "hit" || r.body != "2" {
		t.Fatalf("new manifest: %+v", r)
	}
}

func TestMaxVariants(t *testing.T) {
	useClock(t)
	app := qinetic.New()
	app.Use(New(Config{MaxVariants: 3}))
	h, _ := counted(func(c *qinetic.Ctx, n int64) error {
		c.Set("Vary", "X-Tenant")
		return c.SendString(fmt.Sprint(n))
	})
	app.Get("/", h)
	for i := range 4 {
		get(t, app, "GET", "/", "X-Tenant", fmt.Sprint(i))
	}
	for i := range 3 {
		if r := get(t, app, "GET", "/", "X-Tenant", fmt.Sprint(i)); r.cache != "hit" || r.body != fmt.Sprint(i+1) {
			t.Fatalf("variant %d: %+v", i, r)
		}
	}
	if r := get(t, app, "GET", "/", "X-Tenant", "3"); r.cache != "miss" {
		t.Fatalf("variant over MaxVariants stored: %+v", r)
	}
	// Storing an existing variant again does not count as a new one.
	if r := get(t, app, "GET", "/", "X-Tenant", "0"); r.cache != "hit" {
		t.Fatalf("existing variant: %+v", r)
	}
}

func TestForeignEntriesAreMisses(t *testing.T) {
	clk := useClock(t)
	st := memory.New(memory.Config{Clock: clk.now})
	app := qinetic.New()
	app.Use(New(Config{Storage: st, KeyGenerator: func(*qinetic.Ctx) string { return "k" }}))
	h, calls := counted(func(c *qinetic.Ctx, n int64) error { return c.SendString(fmt.Sprint(n)) })
	app.Get("/", h)
	for _, stored := range [][]byte{
		{2, kindEntry, 200},                              // a future version
		{formatVersion, 'X'},                             // unknown kind
		{formatVersion, kindEntry, 0xff},                 // truncated
		{formatVersion, kindManifest, 1, 0},              // manifest without names
		append([]byte{formatVersion, kindEntry}, "x"...), // garbage
		{formatVersion},
		// Out-of-range numbers, as a hostile external store could hold, are not converted.
		binary.AppendUvarint([]byte{formatVersion, kindEntry}, 5),
		binary.AppendUvarint([]byte{formatVersion, kindEntry}, 1<<63),
		append(binary.AppendUvarint([]byte{formatVersion, kindManifest}, 1<<40), 1, 1, 'a'),
	} {
		_ = st.Set(context.Background(), hashKey("k"), stored, time.Minute)
		before := calls.Load()
		if r := get(t, app, "GET", "/"); r.cache != "miss" || calls.Load() != before+1 {
			t.Fatalf("stored %v: %+v", stored, r)
		}
	}
}

func TestHeadersFromEarlierMiddlewareAreNotReplayed(t *testing.T) {
	useClock(t)
	var id atomic.Int64
	app := qinetic.New()
	app.Use(func(c *qinetic.Ctx) error {
		c.Set("X-Request-ID", fmt.Sprint(id.Add(1)))
		return c.Next()
	})
	app.Use(New())
	app.Get("/", func(c *qinetic.Ctx) error {
		c.Set("X-Handler", "set by handler")
		return c.SendString("ok")
	})
	get(t, app, "GET", "/")
	r := get(t, app, "GET", "/")
	if r.cache != "hit" || r.header.Get("X-Handler") != "set by handler" {
		t.Fatalf("hit: %+v", r)
	}
	if got := r.header.Values("X-Request-ID"); len(got) != 1 || got[0] != "2" {
		t.Fatalf("X-Request-ID on a hit = %q, want only this request's", got)
	}
}

type brokenStorage struct{ plainStorage }

func (brokenStorage) Get(context.Context, string) ([]byte, error) {
	return nil, errors.New("redis down")
}
func (brokenStorage) Set(context.Context, string, []byte, time.Duration) error {
	return errors.New("redis down")
}

func TestStorageOutageServesUncached(t *testing.T) {
	clk := useClock(t)
	var logs bytes.Buffer
	app := qinetic.New()
	app.Use(New(Config{Storage: brokenStorage{plainStorage{memory.New(memory.Config{Clock: clk.now})}},
		Logger: slog.New(slog.NewTextHandler(&logs, nil))}))
	app.Get("/", func(c *qinetic.Ctx) error { return c.SendString("fresh") })
	if r := get(t, app, "GET", "/"); r.code != 200 || r.body != "fresh" || r.cache != "miss" {
		t.Fatalf("got %+v", r)
	}
	if !strings.Contains(logs.String(), "storage read failed") || !strings.Contains(logs.String(), "storage write failed") {
		t.Fatalf("outage not logged: %s", logs.String())
	}
}

// TestOrderWithCompress shows why the cache goes after compress: in front of it there is no body to store.
func TestOrderWithCompress(t *testing.T) {
	useClock(t)
	big := strings.Repeat("cache me ", 300)
	newApp := func(cacheFirst bool) (*qinetic.App, *atomic.Int64) {
		app := qinetic.New()
		if cacheFirst {
			app.Use(New(), compress.New())
		} else {
			app.Use(compress.New(), New())
		}
		h, calls := counted(func(c *qinetic.Ctx, _ int64) error { return c.SendString(big) })
		app.Get("/", h)
		return app, calls
	}
	fetch := func(app *qinetic.App) (string, string) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			t.Fatalf("response not compressed: %v", err)
		}
		b, _ := io.ReadAll(zr)
		return resp.Header.Get("X-Cache"), string(b)
	}
	inside, calls := newApp(false)
	fetch(inside)
	if xc, body := fetch(inside); xc != "hit" || body != big || calls.Load() != 1 {
		t.Fatalf("cache inside compress: X-Cache %q, %d calls", xc, calls.Load())
	}
	outside, calls := newApp(true)
	fetch(outside)
	if _, body := fetch(outside); body != big || calls.Load() != 2 {
		t.Fatalf("cache in front of compress stored a committed response: %d calls", calls.Load())
	}
}

// TestConcurrentSameKey reads and writes one key, with variants, from many goroutines; run it with -race.
func TestConcurrentSameKey(t *testing.T) {
	useClock(t)
	app := qinetic.New()
	app.Use(New(Config{MaxVariants: 4}))
	app.Get("/", func(c *qinetic.Ctx) error {
		c.Set("Vary", "X-Tenant")
		return c.SendString("tenant " + c.Get("X-Tenant"))
	})
	var wg sync.WaitGroup
	for g := range 32 {
		wg.Go(func() {
			for i := range 50 {
				tenant := fmt.Sprint((g + i) % 6)
				if r := get(t, app, "GET", "/", "X-Tenant", tenant); r.body != "tenant "+tenant {
					t.Errorf("tenant %s got %q (%s)", tenant, r.body, r.cache)
					return
				}
			}
		})
	}
	wg.Wait()
}
