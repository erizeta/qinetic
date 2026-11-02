package csrf

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erizeta/qinetic"
	"github.com/erizeta/qinetic/internal/memory"
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

// plainStorage stands in for an external Storage, such as Redis, without memory's extras.
type plainStorage struct{ m *memory.Storage }

func (p plainStorage) Get(ctx context.Context, k string) ([]byte, error) { return p.m.Get(ctx, k) }
func (p plainStorage) Set(ctx context.Context, k string, v []byte, ttl time.Duration) error {
	return p.m.Set(ctx, k, v, ttl)
}
func (p plainStorage) Delete(ctx context.Context, k string) error { return p.m.Delete(ctx, k) }
func (p plainStorage) Close() error                               { return p.m.Close() }

type response struct {
	code    int
	body    string
	cookie  *http.Cookie // the csrf_ Set-Cookie, if any
	cookies []*http.Cookie
}

type request struct {
	method, target string
	cookie, token  string
	cookieName     string // the token cookie's name; by default as the middleware names it for the scheme
	hdr            map[string]string
	form           string
}

func send(t *testing.T, app *qinetic.App, r request) response {
	t.Helper()
	return sendFrom(t, app, r, "")
}

// sendFrom is send with the connection's remote address, such as a trusted proxy's.
func sendFrom(t *testing.T, app *qinetic.App, r request, remote string) response {
	t.Helper()
	var body io.Reader
	if r.form != "" {
		body = strings.NewReader(r.form)
	}
	target := r.target
	if target == "" {
		target = "/"
	}
	req := httptest.NewRequest(r.method, target, body)
	if r.form != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if r.cookie != "" {
		name := r.cookieName
		if name == "" {
			name = "csrf_"
			if strings.HasPrefix(target, "https://") || remote == "10.0.0.1:1" && r.hdr["X-Forwarded-Proto"] == "https" {
				name = "__Host-csrf_"
			}
		}
		req.AddCookie(&http.Cookie{Name: name, Value: r.cookie})
	}
	if r.token != "" {
		req.Header.Set("X-Csrf-Token", r.token)
	}
	for k, v := range r.hdr {
		req.Header.Set(k, v)
	}
	if remote != "" {
		req.RemoteAddr = remote
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	out := response{code: resp.StatusCode, body: string(b), cookies: resp.Cookies()}
	for _, ck := range resp.Cookies() {
		if ck.Name == "csrf_" || ck.Name == "__Host-csrf_" {
			out.cookie = ck
		}
	}
	return out
}

func newApp(cfg Config) *qinetic.App {
	app := qinetic.New(qinetic.Config{Logger: slog.New(slog.DiscardHandler), TrustedProxies: []string{"10.0.0.1"}})
	app.Use(New(cfg))
	app.Get("/", func(c *qinetic.Ctx) error { return c.SendString(TokenFromContext(c)) })
	app.Post("/", func(c *qinetic.Ctx) error { return c.SendString("changed") })
	app.Post("/login", func(c *qinetic.Ctx) error {
		if err := DeleteToken(c); err != nil {
			return err
		}
		return c.SendString("logged in")
	})
	return app
}

// storages returns the default in-memory storage and an external one without its extras.
func storages(clk *clock) map[string]qinetic.Storage {
	return map[string]qinetic.Storage{
		"default": nil,
		"plain":   plainStorage{memory.New(memory.Config{Clock: clk.now})},
	}
}

func TestIssueToken(t *testing.T) {
	clk := useClock(t)
	app := newApp(Config{})
	r := send(t, app, request{method: "GET"})
	ck := r.cookie
	if r.code != 200 || ck == nil || ck.Value != r.body || len(ck.Value) != 43 {
		t.Fatalf("got %d %q, cookie %+v", r.code, r.body, ck)
	}
	if !ck.HttpOnly || ck.Secure || ck.SameSite != http.SameSiteLaxMode || ck.Path != "/" || ck.MaxAge != 3600 {
		t.Fatalf("cookie attributes %+v", ck)
	}
	// A stored token is reused, without a new cookie until half its life has passed.
	clk.advance(10 * time.Minute)
	if r2 := send(t, app, request{method: "GET", cookie: ck.Value}); r2.body != ck.Value || r2.cookie != nil {
		t.Fatalf("reuse: %q, cookie %+v", r2.body, r2.cookie)
	}
	clk.advance(25 * time.Minute)
	if r3 := send(t, app, request{method: "HEAD", cookie: ck.Value}); r3.cookie == nil || r3.cookie.Value != ck.Value {
		t.Fatalf("refresh: cookie %+v", r3.cookie)
	}
	// An unknown cookie value is replaced by a token the server issued.
	if r4 := send(t, app, request{method: "GET", cookie: "forged"}); r4.body == "forged" || r4.cookie == nil {
		t.Fatalf("forged cookie kept: %q", r4.body)
	}
}

func TestCookieOptions(t *testing.T) {
	useClock(t)
	app := newApp(Config{CookieScriptAccess: true, CookieSameSite: qinetic.CookieSameSiteStrict, CookiePath: "/app",
		Expiration: 2 * time.Minute})
	ck := send(t, app, request{method: "GET", target: "https://example.com/"}).cookie
	if ck == nil || ck.HttpOnly || !ck.Secure || ck.SameSite != http.SameSiteStrictMode || ck.Path != "/app" || ck.MaxAge != 120 {
		t.Fatalf("cookie %+v", ck)
	}
	// Behind a trusted TLS proxy the cookie is Secure too.
	ck = send(t, app, request{method: "GET", hdr: map[string]string{"X-Forwarded-Proto": "https"}}).cookie
	if ck == nil || ck.Secure {
		t.Fatalf("untrusted proxy made the cookie Secure: %+v", ck)
	}
}

func TestTokenCheck(t *testing.T) {
	clk := useClock(t)
	for name, st := range storages(clk) {
		t.Run(name, func(t *testing.T) {
			app := newApp(Config{Storage: st})
			token := send(t, app, request{method: "GET"}).body
			tests := []struct {
				name string
				req  request
				code int
				body string
			}{
				{"valid header", request{method: "POST", cookie: token, token: token}, 200, "changed"},
				{"valid form field", request{method: "POST", cookie: token, form: "_csrf=" + token}, 200, "changed"},
				{"PUT", request{method: "PUT", cookie: token, token: token}, 405, ""},
				{"no token", request{method: "POST", cookie: token}, 403, "missing CSRF token"},
				{"no cookie", request{method: "POST", token: token}, 403, "missing CSRF token"},
				{"wrong token", request{method: "POST", cookie: token, token: token + "x"}, 403, "invalid CSRF token"},
				{"matching pair never issued", request{method: "POST", cookie: "made-up", token: "made-up"}, 403, "invalid CSRF token"},
				{"query is not read by default", request{method: "POST", target: "/?_csrf=" + token, cookie: token}, 403, "missing CSRF token"},
			}
			for _, tt := range tests {
				r := send(t, app, tt.req)
				if r.code != tt.code || tt.body != "" && r.body != tt.body {
					t.Errorf("%s: got %d %q, want %d %q", tt.name, r.code, r.body, tt.code, tt.body)
				}
			}
			clk.advance(time.Hour)
			if r := send(t, app, request{method: "POST", cookie: token, token: token}); r.code != 403 {
				t.Errorf("expired token: %d", r.code)
			}
		})
	}
}

func TestOriginCheck(t *testing.T) {
	useClock(t)
	app := newApp(Config{TrustedOrigins: []string{"https://admin.example.com"}})
	token := send(t, app, request{method: "GET"}).body
	tests := []struct {
		name string
		hdr  map[string]string
		code int
	}{
		{"same-origin", map[string]string{"Sec-Fetch-Site": "same-origin"}, 200},
		{"user typed URL", map[string]string{"Sec-Fetch-Site": "none"}, 200},
		{"cross-site", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, 403},
		{"same-site sibling", map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://blog.example.com"}, 403},
		{"cross-site trusted", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://admin.example.com"}, 200},
		// Without Fetch Metadata, as over plain HTTP in some browsers, Origin decides.
		{"foreign Origin over HTTP", map[string]string{"Origin": "http://evil.example"}, 403},
		{"Origin matching Host", map[string]string{"Origin": "http://example.com"}, 200},
		{"null Origin", map[string]string{"Origin": "null"}, 403},
		{"trusted Origin", map[string]string{"Origin": "https://ADMIN.example.com"}, 200},
		{"neither header", nil, 200},
	}
	for _, tt := range tests {
		r := send(t, app, request{method: "POST", cookie: token, token: token, hdr: tt.hdr})
		if r.code != tt.code {
			t.Errorf("%s: got %d %q, want %d", tt.name, r.code, r.body, tt.code)
		}
		if tt.code == 403 && r.body != "cross-origin request refused" {
			t.Errorf("%s: body %q", tt.name, r.body)
		}
	}
	// A same-origin browser request still needs the token.
	if r := send(t, app, request{method: "POST", hdr: map[string]string{"Sec-Fetch-Site": "same-origin"}}); r.code != 403 {
		t.Errorf("same-origin without token: %d", r.code)
	}
}

func TestDeleteToken(t *testing.T) {
	clk := useClock(t)
	for name, st := range storages(clk) {
		t.Run(name, func(t *testing.T) {
			app := newApp(Config{Storage: st})
			token := send(t, app, request{method: "GET"}).body
			r := send(t, app, request{method: "POST", target: "/login", cookie: token, token: token})
			if r.code != 200 || r.cookie == nil || r.cookie.MaxAge >= 0 || r.cookie.Value != "" {
				t.Fatalf("login: %d, cookie %+v", r.code, r.cookie)
			}
			if r = send(t, app, request{method: "POST", cookie: token, token: token}); r.code != 403 {
				t.Fatalf("deleted token accepted: %d", r.code)
			}
			if next := send(t, app, request{method: "GET", cookie: token}).body; next == token || next == "" {
				t.Fatalf("no new token after DeleteToken: %q", next)
			}
		})
	}
}

func TestDeleteTokenWithoutMiddleware(t *testing.T) {
	var err error
	app := qinetic.New()
	app.Get("/", func(c *qinetic.Ctx) error {
		err = DeleteToken(c)
		return nil
	})
	resp, _ := app.Test(httptest.NewRequest("GET", "/", nil))
	_ = resp.Body.Close()
	if !errors.Is(err, ErrNoMiddleware) {
		t.Fatalf("got %v", err)
	}
}

func TestExtractorsAndNext(t *testing.T) {
	useClock(t)
	app := newApp(Config{Extractor: Chain(FromHeader("X-Other"), FromQuery("t")), Next: func(c *qinetic.Ctx) bool {
		return c.Get("X-Skip") != ""
	}})
	token := send(t, app, request{method: "GET"}).body
	if r := send(t, app, request{method: "POST", target: "/?t=" + token, cookie: token}); r.code != 200 {
		t.Fatalf("FromQuery: %d %q", r.code, r.body)
	}
	if r := send(t, app, request{method: "POST", cookie: token, hdr: map[string]string{"X-Other": token}}); r.code != 200 {
		t.Fatalf("FromHeader: %d %q", r.code, r.body)
	}
	if r := send(t, app, request{method: "POST", hdr: map[string]string{"X-Skip": "1"}}); r.code != 200 {
		t.Fatalf("Next: %d", r.code)
	}
}

type brokenStorage struct{ plainStorage }

func (brokenStorage) Get(context.Context, string) ([]byte, error) { return nil, errors.New("down") }

func TestStorageErrorsAndConfig(t *testing.T) {
	clk := useClock(t)
	broken := brokenStorage{plainStorage{memory.New(memory.Config{Clock: clk.now})}}
	app := newApp(Config{Storage: broken})
	if r := send(t, app, request{method: "GET", cookie: "x"}); r.code != 500 {
		t.Fatalf("storage error on issue: %d", r.code)
	}
	if r := send(t, app, request{method: "POST", cookie: "x", token: "x"}); r.code != 500 {
		t.Fatalf("storage error on check: %d", r.code)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("no panic for a malformed trusted origin")
		}
	}()
	New(Config{TrustedOrigins: []string{"admin.example.com"}})
}

// TestOriginSchemeAndReferer covers the checks shared with Fiber: Origin must match the request's scheme
// as well as its host, and an HTTPS request without Origin needs a matching Referer.
func TestOriginSchemeAndReferer(t *testing.T) {
	useClock(t)
	app := newApp(Config{TrustedOrigins: []string{"https://admin.example.com"}})
	token := send(t, app, request{method: "GET"}).body
	tests := []struct {
		name, target string
		hdr          map[string]string
		code         int
		body         string
	}{
		{"http Origin to https", "https://example.com/", map[string]string{"Origin": "http://example.com"}, 403, "cross-origin request refused"},
		{"https Origin to http", "http://example.com/", map[string]string{"Origin": "https://example.com"}, 403, "cross-origin request refused"},
		{"https Origin to https", "https://example.com/", map[string]string{"Origin": "https://example.com"}, 200, "changed"},
		{"default port in Host", "https://example.com:443/", map[string]string{"Origin": "https://example.com"}, 200, "changed"},
		{"other port", "https://example.com:8443/", map[string]string{"Origin": "https://example.com"}, 403, "cross-origin request refused"},
		{"https without Origin or Referer", "https://example.com/", nil, 403, "missing Referer on a request without Origin"},
		{"https with own Referer", "https://example.com/", map[string]string{"Referer": "https://example.com/form?x=1"}, 200, "changed"},
		{"https with foreign Referer", "https://example.com/", map[string]string{"Referer": "https://evil.example/"}, 403, "cross-origin request refused"},
		{"https with http Referer", "https://example.com/", map[string]string{"Referer": "http://example.com/form"}, 403, "cross-origin request refused"},
		{"https with trusted Referer", "https://example.com/", map[string]string{"Referer": "https://admin.example.com/panel"}, 200, "changed"},
		{"https with relative Referer", "https://example.com/", map[string]string{"Referer": "/form"}, 403, "cross-origin request refused"},
		{"http without either", "http://example.com/", nil, 200, "changed"},
		// Behind a trusted TLS proxy the request is HTTPS too, so the same rules apply.
		{"proxied https, http Origin", "http://example.com/", map[string]string{"X-Forwarded-Proto": "https", "Origin": "http://example.com"}, 403, "cross-origin request refused"},
		{"proxied https, https Origin", "http://example.com/", map[string]string{"X-Forwarded-Proto": "https", "Origin": "https://example.com"}, 200, "changed"},
		{"proxied https, no Referer", "http://example.com/", map[string]string{"X-Forwarded-Proto": "https"}, 403, "missing Referer on a request without Origin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hdr := map[string]string{}
			for k, v := range tt.hdr {
				hdr[k] = v
			}
			req := request{method: "POST", target: tt.target, cookie: token, token: token, hdr: hdr}
			if hdr["X-Forwarded-Proto"] != "" {
				r := sendFrom(t, app, req, "10.0.0.1:1")
				if r.code != tt.code || r.body != tt.body {
					t.Fatalf("got %d %q, want %d %q", r.code, r.body, tt.code, tt.body)
				}
				return
			}
			if r := send(t, app, req); r.code != tt.code || r.body != tt.body {
				t.Fatalf("got %d %q, want %d %q", r.code, r.body, tt.code, tt.body)
			}
		})
	}
}

// TestHostPrefix checks the token cookie gets the __Host- prefix over HTTPS, so a cookie tossed in from a
// subdomain, which cannot use that prefix, is not taken for it.
func TestHostPrefix(t *testing.T) {
	useClock(t)
	app := newApp(Config{})
	ck := send(t, app, request{method: "GET", target: "https://example.com/"}).cookie
	if ck == nil || ck.Name != "__Host-csrf_" || !ck.Secure || ck.Path != "/" || ck.Domain != "" {
		t.Fatalf("HTTPS cookie %+v", ck)
	}
	if plain := send(t, app, request{method: "GET"}).cookie; plain == nil || plain.Name != "csrf_" {
		t.Fatalf("HTTP cookie %+v", plain)
	}
	post := request{method: "POST", target: "https://example.com/", cookie: ck.Value, token: ck.Value,
		hdr: map[string]string{"Origin": "https://example.com"}}
	if r := send(t, app, post); r.code != 200 {
		t.Fatalf("POST with the __Host- cookie: %d %q", r.code, r.body)
	}
	tossed := post
	tossed.cookieName = "csrf_"
	if r := send(t, app, tossed); r.code != 403 || r.body != "missing CSRF token" {
		t.Fatalf("tossed csrf_ cookie over HTTPS: %d %q", r.code, r.body)
	}
	for name, cfg := range map[string]Config{
		"custom name":   {CookieName: "xsrf"},
		"domain":        {CookieDomain: "example.com"},
		"narrower path": {CookiePath: "/app"},
	} {
		cookies := send(t, newApp(cfg), request{method: "GET", target: "https://example.com/"}).cookies
		if len(cookies) != 1 || strings.HasPrefix(cookies[0].Name, "__Host-") {
			t.Errorf("%s: cookies %v, want one without the __Host- prefix", name, cookies)
		}
	}
}
