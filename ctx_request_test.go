package qinetic

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erizeta/qilat"
)

// capture registers a GET route that runs fn and returns the app.
func capture(cfg Config, pattern string, fn func(c *Ctx)) *App {
	app := New(cfg)
	app.Get(pattern, func(c *Ctx) error {
		fn(c)
		return nil
	})
	return app
}

func TestRequestAccessors(t *testing.T) {
	var got []string
	app := capture(Config{}, "/a/:name", func(c *Ctx) {
		got = []string{c.Method(), c.Path(), c.OriginalURL(), c.Host(), c.Hostname(), c.Protocol(),
			c.Get("X-Test"), c.Get("X-Missing", "def"), string(c.PeekHeader("X-Test")), c.Route().Path,
			c.Route().Method, strings.Join(c.Route().Params, ","), c.Request().Method()}
	})
	req := httptest.NewRequest("GET", "http://example.com:8080/a/b%2Fc?x=1", nil)
	req.Header.Set("X-Test", "v")
	do(t, app, req)
	want := []string{"GET", "/a/b/c", "/a/b%2Fc?x=1", "example.com:8080", "example.com", "HTTP/1.1", "v", "def",
		"v", "/a/:name", "GET", "name", "GET"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestParams(t *testing.T) {
	tests := []struct {
		pattern, path, key, want string
	}{
		{"/users/:id", "/users/42", "id", "42"},
		{"/users/:id", "/users/J%C3%BCrgen", "id", "Jürgen"},
		{"/files/:name", "/files/a%2Fb", "name", "a/b"},
		{"/files/:name", "/files/a+b", "name", "a+b"},
		{"/u/:name?", "/u", "name", "fallback"},
		{"/static/*", "/static/css/a%20b.css", "*", "css/a b.css"},
		{"/static/*", "/static/css/x.css", "*1", "css/x.css"},
		{"/a/*/b/*", "/a/1/b/2", "*2", "2"},
		{"/api/+", "/api/x/y", "+", "x/y"},
		{"/users/:id", "/users/42", "missing", "fallback"},
	}
	for _, tt := range tests {
		t.Run(tt.pattern+" "+tt.path, func(t *testing.T) {
			var got string
			app := capture(Config{}, tt.pattern, func(c *Ctx) { got = c.Params(tt.key, "fallback") })
			do(t, app, httptest.NewRequest("GET", tt.path, nil))
			if got != tt.want {
				t.Fatalf("Params(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

// TestPeekParamValuesStayValid checks that decoding a second parameter does not overwrite the first.
func TestPeekParamValuesStayValid(t *testing.T) {
	var a, b []byte
	app := capture(Config{}, "/:a/:b", func(c *Ctx) {
		a = c.PeekParam("a")
		b = c.PeekParam("b")
		a = append([]byte(nil), a...)
		b = append([]byte(nil), b...)
	})
	do(t, app, httptest.NewRequest("GET", "/x%20y/"+strings.Repeat("%41", 300), nil))
	if string(a) != "x y" || string(b) != strings.Repeat("A", 300) {
		t.Fatalf("a = %q, b = %d bytes %q...", a, len(b), b[:min(len(b), 8)])
	}
}

func TestParamsInt(t *testing.T) {
	var n int
	var err error
	app := capture(Config{}, "/:id", func(c *Ctx) { n, err = c.ParamsInt("id") })
	if do(t, app, httptest.NewRequest("GET", "/42", nil)); n != 42 || err != nil {
		t.Fatalf("ParamsInt = %d, %v", n, err)
	}
	if do(t, app, httptest.NewRequest("GET", "/x", nil)); err == nil {
		t.Fatal("ParamsInt of a non-number succeeded")
	}
}

func TestQuery(t *testing.T) {
	var got []any
	app := capture(Config{}, "/", func(c *Ctx) {
		got = []any{c.Query("a"), c.Query("missing", "def"), c.Query("empty", "def"), string(c.PeekQuery("a")),
			c.QueryInt("n"), c.QueryInt("bad", 7), c.QueryBool("b"), c.QueryBool("missing", true), c.Queries()}
	})
	do(t, app, httptest.NewRequest("GET", "/?a=x%20y&a=second&empty=&n=12&bad=z&b=true", nil))
	want := []any{"x y", "def", "def", "x y", 12, 7, true, true,
		map[string]string{"a": "x y", "empty": "", "n": "12", "bad": "z", "b": "true"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
}

func TestCookies(t *testing.T) {
	var got []string
	app := capture(Config{}, "/", func(c *Ctx) {
		got = []string{c.Cookies("a"), c.Cookies("b"), c.Cookies("missing", "def")}
	})
	req := httptest.NewRequest("GET", "/", nil)
	// HTTP/2 clients may send one Cookie field per cookie.
	req.Header.Add("Cookie", "a=1; x=y")
	req.Header.Add("Cookie", "b=2")
	do(t, app, req)
	if fmt.Sprint(got) != "[1 2 def]" {
		t.Fatalf("got %q", got)
	}
}

func TestIP(t *testing.T) {
	tests := []struct {
		name, remote, xff string
		cfg               Config
		want              string
	}{
		{"no proxy config", "203.0.113.9:1", "1.1.1.1", Config{}, "203.0.113.9"},
		{"untrusted peer spoofs header", "203.0.113.9:1", "1.1.1.1",
			Config{ProxyHeader: "X-Forwarded-For", TrustedProxies: []string{"10.0.0.0/8"}}, "203.0.113.9"},
		{"trusted proxy", "10.0.0.1:1", "198.51.100.7",
			Config{ProxyHeader: "X-Forwarded-For", TrustedProxies: []string{"10.0.0.0/8"}}, "198.51.100.7"},
		{"client prepends a fake hop", "10.0.0.1:1", "1.1.1.1, 198.51.100.7, 10.0.0.2",
			Config{ProxyHeader: "X-Forwarded-For", TrustedProxies: []string{"10.0.0.0/8"}}, "198.51.100.7"},
		{"malformed hop", "10.0.0.1:1", "198.51.100.7, garbage",
			Config{ProxyHeader: "X-Forwarded-For", TrustedProxies: []string{"10.0.0.0/8"}}, "10.0.0.1"},
		{"all hops trusted", "10.0.0.1:1", "10.0.0.3",
			Config{ProxyHeader: "X-Forwarded-For", TrustedProxies: []string{"10.0.0.0/8"}}, "10.0.0.3"},
		{"single IP entry, IPv6", "[2001:db8::1]:1", "198.51.100.7",
			Config{ProxyHeader: "X-Forwarded-For", TrustedProxies: []string{"2001:db8::1"}}, "198.51.100.7"},
		{"mapped prefix", "10.0.0.1:1", "198.51.100.7",
			Config{ProxyHeader: "X-Forwarded-For", TrustedProxies: []string{"::ffff:10.0.0.0/104"}}, "198.51.100.7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ip string
			var ips []string
			app := capture(tt.cfg, "/", func(c *Ctx) { ip, ips = c.IP(), c.IPs() })
			req := httptest.NewRequest("GET", "/", nil)
			req.RemoteAddr = tt.remote
			req.Header.Set("X-Forwarded-For", tt.xff)
			do(t, app, req)
			if ip != tt.want {
				t.Fatalf("IP() = %q, want %q", ip, tt.want)
			}
			if strings.Join(ips, ", ") != tt.xff {
				t.Fatalf("IPs() = %q, want the header as sent", ips)
			}
		})
	}
}

func TestInvalidTrustedProxyPanics(t *testing.T) {
	defer func() {
		if msg := fmt.Sprint(recover()); !strings.Contains(msg, "not-an-ip") {
			t.Fatalf("panic = %q", msg)
		}
	}()
	New(Config{TrustedProxies: []string{"not-an-ip"}})
}

func TestSecure(t *testing.T) {
	cfg := Config{TrustedProxies: []string{"10.0.0.1"}}
	var secure bool
	app := capture(cfg, "/", func(c *Ctx) { secure = c.Secure() })
	for _, tt := range []struct {
		remote string
		want   bool
	}{{"203.0.113.9:1", false}, {"10.0.0.1:1", true}} {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = tt.remote
		req.Header.Set("X-Forwarded-Proto", "https")
		do(t, app, req)
		if secure != tt.want {
			t.Errorf("remote %s: Secure() = %v, want %v", tt.remote, secure, tt.want)
		}
	}
	req := httptest.NewRequest("GET", "https://example.com/", nil)
	if do(t, app, req); !secure {
		t.Error("Secure() = false over TLS")
	}
}

func bodyApp(cfg Config) *App {
	app := New(cfg)
	app.Post("/", func(c *Ctx) error {
		b, err := c.Body()
		if err != nil {
			return err
		}
		again, _ := c.Body()
		return c.SendString(fmt.Sprintf("%d %v", len(b), bytes.Equal(b, again)))
	})
	app.Post("/stream", func(c *Ctx) error {
		n, err := io.Copy(io.Discard, c.BodyReader())
		if err != nil {
			return err
		}
		return c.SendString(fmt.Sprint(n))
	})
	return app
}

// unsized hides the length, so the request is sent without Content-Length.
type unsized struct{ io.Reader }

func TestBodyLimit(t *testing.T) {
	app := bodyApp(Config{BodyLimit: 10})
	tests := []struct {
		name, path string
		body       io.Reader
		code       int
		want       string
	}{
		{"within", "/", strings.NewReader("0123456789"), 200, "10 true"},
		{"declared too large", "/", strings.NewReader("0123456789x"), 413, "Request Entity Too Large"},
		{"undeclared too large", "/", unsized{strings.NewReader(strings.Repeat("x", 1000))}, 413, "Request Entity Too Large"},
		{"stream within", "/stream", unsized{strings.NewReader("0123456789")}, 200, "10"},
		{"stream too large", "/stream", unsized{strings.NewReader("0123456789x")}, 413, "Request Entity Too Large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", tt.path, tt.body)
			if _, ok := tt.body.(unsized); ok {
				req.ContentLength = -1
			}
			resp, body := do(t, app, req)
			if resp.StatusCode != tt.code || body != tt.want {
				t.Fatalf("got %d %q, want %d %q", resp.StatusCode, body, tt.code, tt.want)
			}
		})
	}
	unlimited := bodyApp(Config{BodyLimit: -1})
	big := strings.Repeat("x", DefaultBodyLimit+1)
	if resp, body := do(t, unlimited, httptest.NewRequest("POST", "/", strings.NewReader(big))); resp.StatusCode != 200 {
		t.Fatalf("unlimited: got %d %q", resp.StatusCode, body)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestBodyReadErrorIs400(t *testing.T) {
	logger, logs := testLogger()
	app := bodyApp(Config{Logger: logger})
	req := httptest.NewRequest("POST", "/", unsized{failingReader{}})
	req.ContentLength = -1
	resp, body := do(t, app, req)
	if resp.StatusCode != 400 || body != "Bad Request" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	if strings.Contains(logs.String(), "connection reset") {
		t.Fatalf("a client error was logged as a server error: %s", logs)
	}
}

func TestBodyTooLargeFromServerIs413(t *testing.T) {
	var err error
	app := New()
	app.Post("/", func(c *Ctx) error {
		_, err = c.Body()
		return err
	})
	base := serveWith(t, &qilat.Server{Handler: app, MaxRequestBodySize: 5})
	resp, perr := http.Post(base, "text/plain", unsized{strings.NewReader("0123456789")})
	if perr != nil {
		t.Fatal(perr)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 413 || !errors.Is(err, ErrRequestEntityTooLarge) {
		t.Fatalf("got %d, Body error %v", resp.StatusCode, err)
	}
}

func TestFormValue(t *testing.T) {
	var got []string
	app := New()
	app.Post("/", func(c *Ctx) error {
		got = []string{c.FormValue("a"), c.FormValue("missing", "def"), c.FormValue("b")}
		return nil
	})
	req := httptest.NewRequest("POST", "/?a=query", strings.NewReader("a=x+y&b=%26"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	do(t, app, req)
	if fmt.Sprint(got) != "[x y def &]" {
		t.Fatalf("urlencoded: got %q", got)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("a", "multi")
	fw, _ := mw.CreateFormFile("file", "hello.txt")
	_, _ = fw.Write([]byte("file body"))
	_ = mw.Close()
	var fileBody, fileName string
	app2 := New()
	app2.Post("/", func(c *Ctx) error {
		got = []string{c.FormValue("a")}
		fh, err := c.FormFile("file")
		if err != nil {
			return err
		}
		f, err := fh.Open()
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		b, _ := io.ReadAll(f)
		fileBody, fileName = string(b), fh.Filename
		if _, err := c.FormFile("nope"); err == nil {
			return errors.New("FormFile of a missing field succeeded")
		}
		return nil
	})
	req = httptest.NewRequest("POST", "/", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if resp, body := do(t, app2, req); resp.StatusCode != 200 {
		t.Fatalf("multipart: got %d %q", resp.StatusCode, body)
	}
	if got[0] != "multi" || fileBody != "file body" || fileName != "hello.txt" {
		t.Fatalf("multipart: value %q, file %q %q", got, fileName, fileBody)
	}
}

func TestMultipartFormErrors(t *testing.T) {
	var err error
	app := New()
	app.Post("/", func(c *Ctx) error {
		_, err = c.MultipartForm()
		return nil
	})
	for ct, want := range map[string]int{
		"text/plain":                        415,
		"":                                  415,
		"multipart/form-data":               400, // no boundary
		"application/x-www-form-urlencoded": 400,
	} {
		req := httptest.NewRequest("POST", "/", strings.NewReader("x"))
		req.Header.Set("Content-Type", ct)
		do(t, app, req)
		var e *Error
		if !errors.As(err, &e) || e.Code != want {
			t.Errorf("Content-Type %q: error %v, want a %d *Error", ct, err, want)
		}
	}
}

func TestLocals(t *testing.T) {
	type key struct{}
	app := New()
	app.Use(func(c *Ctx) error {
		c.Locals(key{}, "user-1")
		c.Locals("n", 2)
		return c.Next()
	})
	var got []any
	app.Get("/", func(c *Ctx) error {
		got = []any{c.Locals(key{}), c.Locals("n"), c.Locals("missing")}
		return nil
	})
	get(t, app, "GET", "/")
	if fmt.Sprint(got) != "[user-1 2 <nil>]" {
		t.Fatalf("got %v", got)
	}
}

func TestRouteBeforeMatch(t *testing.T) {
	var r Route
	app := New()
	app.Use(func(c *Ctx) error {
		r = c.Route()
		return c.Next()
	})
	get(t, app, "GET", "/a//b") // unroutable: root middleware runs as route "/"
	if r.Method != "USE" || r.Path != "/" {
		t.Fatalf("Route() = %+v", r)
	}
}

// TestRouteIsACopy checks a handler changing what c.Route returns cannot change the route for later
// requests.
func TestRouteIsACopy(t *testing.T) {
	var seen []string
	app := New()
	app.Get("/users/:id", func(c *Ctx) error {
		rt := c.Route()
		seen = append(seen, rt.Path)
		rt.Path, rt.Method = "/changed", "POST"
		return nil
	})
	get(t, app, "GET", "/users/1")
	get(t, app, "GET", "/users/2")
	if len(seen) != 2 || seen[1] != "/users/:id" {
		t.Fatalf("Route().Path across requests: %q", seen)
	}
	if r, _ := get(t, app, "GET", "/users/3"); r.StatusCode != 200 {
		t.Fatalf("route stopped matching: %d", r.StatusCode)
	}
}

func TestRouteAllocs(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts differ under the race detector")
	}
	app := New()
	var rt Route
	app.Get("/users/:id", func(c *Ctx) error {
		rt = c.Route()
		return nil
	})
	get(t, app, "GET", "/users/1")
	c := &Ctx{route: app.router.routes[0], app: app}
	if n := testing.AllocsPerRun(100, func() { rt = c.Route() }); n != 0 || rt.Path != "/users/:id" {
		t.Fatalf("Route: %v allocs/op, %+v", n, rt)
	}
}

// TestAccessorAllocs pins the documented costs: Peek* never allocate and the string accessors copy once.
func TestAccessorAllocs(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool drops items at random under the race detector")
	}
	accessors := map[string]struct {
		call func(c *Ctx)
		want float64
	}{
		"none":       {func(c *Ctx) {}, 0},
		"PeekParam":  {func(c *Ctx) { _ = c.PeekParam("id") }, 0},
		"PeekQuery":  {func(c *Ctx) { _ = c.PeekQuery("q") }, 0},
		"PeekHeader": {func(c *Ctx) { _ = c.PeekHeader("X-Test") }, 0},
		"Params":     {func(c *Ctx) { _ = c.Params("id") }, 1},
		"Query":      {func(c *Ctx) { _ = c.Query("q") }, 1},
		"Get":        {func(c *Ctx) { _ = c.Get("X-Test") }, 1},
	}
	measure := func(call func(c *Ctx)) float64 {
		app := New()
		app.Get("/users/:id", func(c *Ctx) error {
			call(c)
			return nil
		})
		std := qilat.ToStd(app)
		req := httptest.NewRequest("GET", "/users/42?q=search", nil)
		req.Header.Set("X-Test", "value")
		std.ServeHTTP(httptest.NewRecorder(), req)
		return testing.AllocsPerRun(200, func() { std.ServeHTTP(httptest.NewRecorder(), req) })
	}
	base := measure(accessors["none"].call)
	for name, a := range accessors {
		if got := measure(a.call) - base; got != a.want {
			t.Errorf("%s: %v allocs/op, want %v", name, got, a.want)
		}
	}
}

func TestIPOverTCP(t *testing.T) {
	app := New()
	app.Get("/", func(c *Ctx) error { return c.SendString(c.IP()) })
	resp, err := http.Get(serve(t, app))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "127.0.0.1" {
		t.Fatalf("IP() = %q", body)
	}
}

func TestBodyReaderAfterBody(t *testing.T) {
	app := New(Config{BodyLimit: 5})
	app.Post("/", func(c *Ctx) error {
		b, err := c.Body()
		if err != nil {
			_, rerr := io.ReadAll(c.BodyReader())
			return errors.Join(err, rerr)
		}
		again, err := io.ReadAll(c.BodyReader())
		if err != nil {
			return err
		}
		return c.SendString(string(b) + "|" + string(again))
	})
	if resp, body := do(t, app, httptest.NewRequest("POST", "/", strings.NewReader("abc"))); body != "abc|abc" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	if resp, _ := do(t, app, httptest.NewRequest("POST", "/", strings.NewReader("abcdef"))); resp.StatusCode != 413 {
		t.Fatalf("too large: got %d", resp.StatusCode)
	}
}

func TestHostFromTrustedProxy(t *testing.T) {
	cfg := Config{TrustedProxies: []string{"10.0.0.0/8"}}
	tests := []struct {
		name, remote   string
		fwd            []string
		host, hostname string
	}{
		{"untrusted peer", "203.0.113.9:1", []string{"evil.example"}, "app.example:8080", "app.example"},
		{"trusted proxy", "10.0.0.1:1", []string{"public.example"}, "public.example", "public.example"},
		{"appended list takes the nearest", "10.0.0.1:1", []string{"evil.example, public.example:443"}, "public.example:443", "public.example"},
		{"repeated fields", "10.0.0.1:1", []string{"evil.example", "public.example"}, "public.example", "public.example"},
		{"empty value falls back", "10.0.0.1:1", []string{" "}, "app.example:8080", "app.example"},
		{"ipv6", "10.0.0.1:1", []string{"[2001:db8::1]"}, "[2001:db8::1]", "2001:db8::1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var host, hostname string
			app := capture(cfg, "/", func(c *Ctx) { host, hostname = c.Host(), c.Hostname() })
			req := httptest.NewRequest("GET", "http://app.example:8080/", nil)
			req.RemoteAddr = tt.remote
			for _, v := range tt.fwd {
				req.Header.Add("X-Forwarded-Host", v)
			}
			do(t, app, req)
			if host != tt.host || hostname != tt.hostname {
				t.Fatalf("Host() = %q, Hostname() = %q; want %q, %q", host, hostname, tt.host, tt.hostname)
			}
		})
	}
}
