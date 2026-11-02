package cors

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/erizeta/qinetic"
)

// response is what the tests check of an *http.Response, whose body send has already closed.
type response struct {
	StatusCode int
	Header     http.Header
}

func send(t *testing.T, app *qinetic.App, method, origin string, hdr ...string) response {
	t.Helper()
	req := httptest.NewRequest(method, "/", nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return response{resp.StatusCode, resp.Header}
}

func newApp(cfg Config) *qinetic.App {
	app := qinetic.New()
	app.Use(New(cfg))
	app.All("/", func(c *qinetic.Ctx) error { return c.SendString("route") })
	return app
}

func TestPreflight(t *testing.T) {
	app := newApp(Config{AllowOrigins: []string{"https://app.example"}, AllowCredentials: true, MaxAge: 600})
	resp := send(t, app, "OPTIONS", "https://app.example", "Access-Control-Request-Method", "PUT",
		"Access-Control-Request-Headers", "X-Token")
	h := resp.Header
	if resp.StatusCode != 204 || h.Get("Access-Control-Allow-Origin") != "https://app.example" ||
		h.Get("Access-Control-Allow-Credentials") != "true" || h.Get("Access-Control-Allow-Methods") != "GET, POST, HEAD, PUT, DELETE, PATCH" ||
		h.Get("Access-Control-Allow-Headers") != "X-Token" || h.Get("Access-Control-Max-Age") != "600" ||
		h.Get("Vary") != "Origin, Access-Control-Request-Method, Access-Control-Request-Headers" {
		t.Fatalf("got %d %v", resp.StatusCode, h)
	}
}

func TestRefusedOrigin(t *testing.T) {
	app := newApp(Config{AllowOrigins: []string{"https://app.example"}})
	resp := send(t, app, "OPTIONS", "https://evil.example", "Access-Control-Request-Method", "DELETE")
	if resp.StatusCode != 204 || resp.Header.Get("Access-Control-Allow-Origin") != "" || resp.Header.Get("Access-Control-Allow-Methods") != "" {
		t.Fatalf("refused preflight: %d %v", resp.StatusCode, resp.Header)
	}
	resp = send(t, app, "GET", "https://evil.example")
	if resp.Header.Get("Access-Control-Allow-Origin") != "" || resp.Header.Get("Vary") != "Origin" {
		t.Fatalf("refused simple request: %v", resp.Header)
	}
}

func TestSimpleRequest(t *testing.T) {
	app := newApp(Config{AllowOrigins: []string{"https://App.Example"}, ExposeHeaders: []string{"X-Total"}})
	resp := send(t, app, "GET", "https://app.example")
	if resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != "https://app.example" ||
		resp.Header.Get("Access-Control-Expose-Headers") != "X-Total" {
		t.Fatalf("got %d %v", resp.StatusCode, resp.Header)
	}
	// No Origin: not a CORS request, but caches must still key on Origin.
	resp = send(t, app, "GET", "")
	if resp.Header.Get("Access-Control-Allow-Origin") != "" || resp.Header.Get("Vary") != "Origin" {
		t.Fatalf("no origin: %v", resp.Header)
	}
	// A plain OPTIONS request without Access-Control-Request-Method reaches the route.
	if resp = send(t, app, "OPTIONS", "https://app.example"); resp.StatusCode != 200 {
		t.Fatalf("plain OPTIONS: %d", resp.StatusCode)
	}
}

func TestDefaultAllowsAny(t *testing.T) {
	app := newApp(Config{})
	resp := send(t, app, "GET", "https://anyone.example")
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" || resp.Header.Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("%v", resp.Header)
	}
}

func TestWildcardSubdomains(t *testing.T) {
	app := newApp(Config{AllowOrigins: []string{"https://*.example.com"}, AllowCredentials: true})
	tests := map[string]bool{
		"https://a.example.com":          true,
		"https://a.b.example.com":        true,
		"https://example.com":            false,
		"http://a.example.com":           false,
		"https://evil.com/.example.com":  false,
		"https://evil.com?.example.com":  false,
		"https://a.example.com.evil.com": false,
		"https://.example.com":           false,
		"https://user@a.example.com":     false,
		"https://a.example.com:8443":     false,
		"https://xexample.com":           false,
		"https://a.example.com/":         false,
		"https://A.Example.com":          true,
		"null":                           false,
		"https://a_b.example.com":        false,
		"https://a-b.example.com":        true,
		"https://a..example.com":         false, // empty DNS label
	}
	for origin, want := range tests {
		got := send(t, app, "GET", origin).Header.Get("Access-Control-Allow-Origin") == origin
		if got != want {
			t.Errorf("%s: allowed %v, want %v", origin, got, want)
		}
	}
}

func TestAllowOriginsFunc(t *testing.T) {
	app := newApp(Config{AllowOriginsFunc: func(o string) bool { return o == "https://dyn.example" }})
	if send(t, app, "GET", "https://dyn.example").Header.Get("Access-Control-Allow-Origin") != "https://dyn.example" {
		t.Fatal("func origin refused")
	}
	if send(t, app, "GET", "https://other.example").Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("func did not refuse")
	}
}

func TestConfiguredHeadersAndNegativeMaxAge(t *testing.T) {
	app := newApp(Config{AllowHeaders: []string{"Content-Type"}, AllowMethods: []string{"GET"}, MaxAge: -1})
	resp := send(t, app, "OPTIONS", "https://x.example", "Access-Control-Request-Method", "GET",
		"Access-Control-Request-Headers", "X-Anything")
	if resp.Header.Get("Access-Control-Allow-Headers") != "Content-Type" || resp.Header.Get("Access-Control-Allow-Methods") != "GET" ||
		resp.Header.Get("Access-Control-Max-Age") != "0" {
		t.Fatalf("%v", resp.Header)
	}
}

func TestInvalidConfigPanics(t *testing.T) {
	for name, cfg := range map[string]Config{
		"credentials with *":  {AllowOrigins: []string{"*"}, AllowCredentials: true},
		"credentials default": {AllowCredentials: true},
		"path":                {AllowOrigins: []string{"https://a.example/path"}},
		"no scheme":           {AllowOrigins: []string{"a.example"}},
		"bad wildcard":        {AllowOrigins: []string{"https://a.*.example"}},
		"two wildcards":       {AllowOrigins: []string{"https://*.*.example"}},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("no panic")
				}
			}()
			New(cfg)
		})
	}
}
