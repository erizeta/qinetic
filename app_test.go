package qinetic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erizeta/qilat"
	"github.com/erizeta/qinetic/internal/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

// testResponse is the part of an *http.Response tests check, read and closed by get.
type testResponse struct {
	StatusCode    int
	Header        http.Header
	ContentLength int64
}

func get(t *testing.T, app *App, method, target string) (testResponse, string) {
	t.Helper()
	return do(t, app, httptest.NewRequest(method, target, nil))
}

func do(t *testing.T, app *App, req *http.Request) (testResponse, string) {
	t.Helper()
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return testResponse{resp.StatusCode, resp.Header, resp.ContentLength}, string(body)
}

func testLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func TestHello(t *testing.T) {
	app := New()
	app.Get("/", func(c *Ctx) error { return c.SendString("Hello, " + c.Protocol()) })
	resp, body := get(t, app, "GET", "/")
	if resp.StatusCode != 200 || body != "Hello, HTTP/1.1" || resp.ContentLength != int64(len(body)) {
		t.Fatalf("got %d %q (Content-Length %d)", resp.StatusCode, body, resp.ContentLength)
	}
}

func TestNotFoundAndMethodNotAllowed(t *testing.T) {
	app := New()
	app.Post("/items", func(c *Ctx) error { return nil })
	resp, body := get(t, app, "GET", "/nope")
	if resp.StatusCode != 404 || body != "Not Found" || resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("got %d %q %q", resp.StatusCode, body, resp.Header.Get("Content-Type"))
	}
	resp, body = get(t, app, "GET", "/items")
	if resp.StatusCode != 405 || body != "Method Not Allowed" || resp.Header.Get("Allow") != "POST" {
		t.Fatalf("got %d %q Allow %q", resp.StatusCode, body, resp.Header.Get("Allow"))
	}
}

func TestContextIsRequestContext(t *testing.T) {
	type key struct{}
	app := New()
	var got any
	app.Get("/", func(c *Ctx) error {
		got = c.Context().Value(key{})
		return nil
	})
	req := httptest.NewRequestWithContext(context.WithValue(context.Background(), key{}, "v"), "GET", "/", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got != "v" {
		t.Fatalf("Context().Value = %v, want the request context's value", got)
	}
}

func TestErrorHandlerReplacesResponse(t *testing.T) {
	var handled error
	app := New(Config{ErrorHandler: func(c *Ctx, err error) error {
		handled = err
		return c.Status(503).SendString("custom")
	}})
	dbErr := errors.New("db down")
	app.Get("/", func(c *Ctx) error {
		_ = c.Status(201).SendString("partial")
		return dbErr
	})
	resp, body := get(t, app, "GET", "/")
	if resp.StatusCode != 503 || body != "custom" || handled != dbErr {
		t.Fatalf("got %d %q, handled %v", resp.StatusCode, body, handled)
	}
}

func TestDefaultErrorHandler(t *testing.T) {
	logger, logs := testLogger()
	app := New(Config{Logger: logger})
	app.Get("/internal", func(c *Ctx) error {
		_ = c.SendString("half written")
		return fmt.Errorf("query: %w", errors.New("password=hunter2"))
	})
	app.Get("/teapot", func(c *Ctx) error { return NewError(418, "short and stout") })
	app.Get("/wrapped", func(c *Ctx) error { return fmt.Errorf("auth: %w", ErrForbidden) })

	resp, body := get(t, app, "GET", "/internal")
	if resp.StatusCode != 500 || body != "Internal Server Error" {
		t.Fatalf("internal error: got %d %q", resp.StatusCode, body)
	}
	if !strings.Contains(logs.String(), "password=hunter2") {
		t.Fatalf("internal error not logged: %s", logs)
	}
	if resp, body = get(t, app, "GET", "/teapot"); resp.StatusCode != 418 || body != "short and stout" {
		t.Fatalf("*Error: got %d %q", resp.StatusCode, body)
	}
	if resp, body = get(t, app, "GET", "/wrapped"); resp.StatusCode != 403 || body != "Forbidden" {
		t.Fatalf("wrapped *Error: got %d %q", resp.StatusCode, body)
	}
}

func TestErrorHandlerFailure(t *testing.T) {
	logger, logs := testLogger()
	app := New(Config{Logger: logger, ErrorHandler: func(c *Ctx, err error) error {
		_ = c.Status(400).SendString("leaky detail")
		return errors.New("template broken")
	}})
	app.Get("/", func(c *Ctx) error { return ErrBadRequest })
	resp, body := get(t, app, "GET", "/")
	if resp.StatusCode != 500 || body != "Internal Server Error" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	if !strings.Contains(logs.String(), "template broken") {
		t.Fatalf("error handler failure not logged: %s", logs)
	}
}

func TestCommittedResponseIsNotRewritten(t *testing.T) {
	logger, logs := testLogger()
	handlerCalled := false
	app := New(Config{Logger: logger, ErrorHandler: func(c *Ctx, err error) error {
		handlerCalled = true
		return DefaultErrorHandler(c, err)
	}})
	app.Get("/", func(c *Ctx) error {
		_ = c.Status(202).SendString("a")
		if err := c.commit(); err != nil {
			return err
		}
		_ = c.SendString("b")
		return errors.New("late failure")
	})
	resp, body := get(t, app, "GET", "/")
	if resp.StatusCode != 202 || body != "ab" {
		t.Fatalf("got %d %q, want the committed response", resp.StatusCode, body)
	}
	if handlerCalled || !strings.Contains(logs.String(), "late failure") {
		t.Fatalf("ErrorHandler called = %v, logs: %s", handlerCalled, logs)
	}
}

func TestNoBodyStatuses(t *testing.T) {
	logger, logs := testLogger()
	app := New(Config{Logger: logger})
	app.Get("/204", func(c *Ctx) error { return c.Status(204).JSON(map[string]int{"a": 1}) })
	app.Get("/304", func(c *Ctx) error { return c.Status(304).SendString("cached") })
	for _, path := range []string{"/204", "/304"} {
		resp, body := get(t, app, "GET", path)
		if resp.StatusCode != int(path[1]-'0')*100+4 || body != "" || resp.Header.Get("Content-Length") != "" {
			t.Fatalf("%s: got %d %q Content-Length %q", path, resp.StatusCode, body, resp.Header.Get("Content-Length"))
		}
	}
	if logs.Len() != 0 {
		t.Fatalf("unexpected errors: %s", logs)
	}
}

func TestUncleanPathRunsOnlyRootMiddleware(t *testing.T) {
	app := New()
	var trace []string
	app.Use(func(c *Ctx) error {
		trace = append(trace, "root")
		return c.Next()
	})
	app.Use("/a", func(c *Ctx) error {
		trace = append(trace, "a")
		return c.Next()
	})
	app.Get("/a/*", func(c *Ctx) error {
		trace = append(trace, "route")
		return nil
	})
	for _, target := range []string{"/a//b", "/a/../b", "/a/%2e%2E/b"} {
		trace = nil
		resp, _ := get(t, app, "GET", target)
		if resp.StatusCode != 404 || len(trace) != 1 || trace[0] != "root" {
			t.Errorf("%s: got %d, trace %v; want 404 after root middleware only", target, resp.StatusCode, trace)
		}
	}
}

func TestGroupsAndRoute(t *testing.T) {
	app := New(Config{StrictRouting: true})
	var trace []string
	api := app.Group("/api", func(c *Ctx) error {
		trace = append(trace, "api")
		return c.Next()
	})
	api.Get("/x", func(c *Ctx) error { return c.SendString("x") })
	app.Route("/r", func(r Router) {
		r.Get("/y", func(c *Ctx) error { return c.SendString("y") })
	})
	if resp, body := get(t, app, "GET", "/api/x"); resp.StatusCode != 200 || body != "x" || len(trace) != 1 {
		t.Fatalf("group route: got %d %q, trace %v", resp.StatusCode, body, trace)
	}
	if resp, body := get(t, app, "GET", "/r/y"); resp.StatusCode != 200 || body != "y" {
		t.Fatalf("Route: got %d %q", resp.StatusCode, body)
	}
}

func TestUseArgumentsPanic(t *testing.T) {
	h := func(c *Ctx) error { return nil }
	tests := map[string]func(app *App){
		"no handlers":   func(app *App) { app.Use("/x") },
		"bad type":      func(app *App) { app.Use(42) },
		"prefix last":   func(app *App) { app.Use(h, "/x") },
		"nil handler":   func(app *App) { app.Use(Handler(nil)) },
		"nil route":     func(app *App) { app.Get("/", nil) },
		"empty method":  func(app *App) { app.Add([]string{""}, "/", h) },
		"invalid route": func(app *App) { app.Get("/:a/:a", h) },
	}
	for name, register := range tests {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("no panic")
				}
			}()
			register(New())
		})
	}
}

func TestDebugPoison(t *testing.T) {
	app := New(Config{DebugPoison: true})
	kept := make(chan *Ctx, 1)
	app.Get("/", func(c *Ctx) error {
		kept <- c
		return nil
	})
	get(t, app, "GET", "/")
	c := <-kept
	msg := make(chan any)
	go func() {
		defer func() { msg <- recover() }()
		_ = c.Protocol()
	}()
	if got := fmt.Sprint(<-msg); !strings.Contains(got, "Ctx used after its handler returned") {
		t.Fatalf("panic = %q, want a use-after-return panic", got)
	}
}

func TestPanicDoesNotPoolCtx(t *testing.T) {
	app := New()
	var panicked *Ctx
	app.Get("/panic", func(c *Ctx) error {
		panicked = c
		panic("boom")
	})
	func() {
		defer func() {
			if v := recover(); v != "boom" {
				t.Fatalf("recovered %v, want the handler's panic", v)
			}
		}()
		resp, _ := app.Test(httptest.NewRequest("GET", "/panic", nil))
		_ = resp.Body.Close()
	}()
	// A released Ctx is reset; this one still points at its app, so it never went back to the pool.
	if panicked.app != app {
		t.Fatal("Ctx was reset and pooled after a panic")
	}
}

func TestRegisterAfterServePanics(t *testing.T) {
	app := New()
	app.Get("/", func(c *Ctx) error { return nil })
	get(t, app, "GET", "/")
	defer func() {
		if msg := fmt.Sprint(recover()); !strings.Contains(msg, "after the app started serving") {
			t.Fatalf("panic = %q", msg)
		}
	}()
	app.Get("/late", func(c *Ctx) error { return nil })
}

// TestRegisterRacesFirstServe must be clean under -race: each registration lands or panics.
func TestRegisterRacesFirstServe(t *testing.T) {
	app := New()
	app.Get("/", func(c *Ctx) error { return nil })
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			defer func() {
				if v := recover(); v != nil && !strings.Contains(fmt.Sprint(v), "after the app started serving") {
					t.Errorf("unexpected panic: %v", v)
				}
			}()
			app.Get(fmt.Sprintf("/r%d", i), func(c *Ctx) error { return nil })
		})
	}
	get(t, app, "GET", "/")
	wg.Wait()
}

// serve runs app on a loopback qilat.Server and returns its base URL.
func serve(t *testing.T, app *App) string {
	t.Helper()
	return serveWith(t, &qilat.Server{Handler: app})
}

// serveWith runs srv on a loopback listener until the test ends and returns its base URL.
func serveWith(t *testing.T, srv *qilat.Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv.Logger = slog.New(slog.DiscardHandler)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		if err := <-done; !errors.Is(err, qilat.ErrServerClosed) {
			t.Errorf("serve: %v", err)
		}
	})
	return "http://" + ln.Addr().String()
}

// TestLargeBodyAndHead checks over a real connection that bodies past qilat's 4 KiB buffer keep a
// Content-Length, and that HEAD gets GET's Content-Length without a body.
func TestLargeBodyAndHead(t *testing.T) {
	big := strings.Repeat("x", 10<<10)
	app := New()
	app.Get("/big", func(c *Ctx) error { return c.SendString(big) })
	base := serve(t, app)
	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()
	for _, method := range []string{"GET", "HEAD"} {
		req, err := http.NewRequest(method, base+"/big", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		wantBody := big
		if method == "HEAD" {
			wantBody = ""
		}
		if resp.ContentLength != int64(len(big)) || len(resp.TransferEncoding) != 0 || string(body) != wantBody {
			t.Fatalf("%s: Content-Length %d, Transfer-Encoding %v, body %d bytes", method, resp.ContentLength,
				resp.TransferEncoding, len(body))
		}
	}
}

// TestHelloAllocs checks that hello routes, static, with a parameter read through PeekParam, and with a
// wildcard, allocate nothing beyond plain qilat on the same in-memory path.
func TestHelloAllocs(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool drops items at random under the race detector")
	}
	app := New()
	hello := func(c *Ctx) error { return c.SendString("Hello, World!") }
	app.Get("/hello", hello)
	app.Get("/users/:id", func(c *Ctx) error {
		if len(c.PeekParam("id")) == 0 {
			return ErrBadRequest
		}
		return hello(c)
	})
	app.Get("/static/*", hello)
	plain := qilat.HandlerFunc(func(w qilat.ResponseWriter, r *qilat.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Length", "13")
		_, _ = w.WriteString("Hello, World!")
	})
	measure := func(h qilat.Handler, target string) float64 {
		std := qilat.ToStd(h)
		req := httptest.NewRequest("GET", target, nil)
		std.ServeHTTP(httptest.NewRecorder(), req) // freeze the router and warm the pools
		return testing.AllocsPerRun(200, func() {
			std.ServeHTTP(httptest.NewRecorder(), req)
		})
	}
	for _, target := range []string{"/hello", "/users/42", "/static/css/site.css"} {
		if got, base := measure(app, target), measure(plain, target); got > base {
			t.Errorf("%s: qinetic %v allocs/op, plain qilat %v", target, got, base)
		}
	}
}

func TestEveryMethodHelper(t *testing.T) {
	type register func(path string, h Handler, hs ...Handler) Router
	app := New()
	g := app.Group("/g")
	helpers := map[string][2]register{
		"GET": {app.Get, g.Get}, "HEAD": {app.Head, g.Head}, "POST": {app.Post, g.Post}, "PUT": {app.Put, g.Put},
		"PATCH": {app.Patch, g.Patch}, "DELETE": {app.Delete, g.Delete}, "OPTIONS": {app.Options, g.Options},
		"CONNECT": {app.Connect, g.Connect}, "TRACE": {app.Trace, g.Trace},
	}
	for method, rs := range helpers {
		for _, r := range rs {
			r("/"+strings.ToLower(method), func(c *Ctx) error { return c.SendString(c.Method()) })
		}
	}
	for method := range helpers {
		for _, prefix := range []string{"", "/g"} {
			req := httptest.NewRequest(method, prefix+"/"+strings.ToLower(method), nil)
			if method == "CONNECT" {
				req.URL.Path = prefix + "/connect"
			}
			resp, body := do(t, app, req)
			if resp.StatusCode != 200 || method != "HEAD" && body != method {
				t.Errorf("%s %s: got %d %q", method, req.URL.Path, resp.StatusCode, body)
			}
		}
	}
}
