package benchmarks

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/erizeta/qilat"
	"github.com/erizeta/qinetic"
	"github.com/gofiber/fiber/v3"
	"github.com/valyala/fasthttp/fasthttputil"
)

// compareScenario is one request every framework answers identically.
type compareScenario struct {
	name, path, want string
}

var compareScenarios = []compareScenario{
	{"plaintext", "/plaintext", helloBody},
	{"json", "/json", `{"message":"Hello, World!"}`},
	{"param", "/users/42", "42"},
	{"routes100", "/r99/items/7", "r99 7"},
	{"middleware5", "/mw/plaintext", helloBody},
}

const routeCount = 100

// framework starts a server for every scenario on ln and returns a function that stops it.
type framework struct {
	name  string
	serve func(tb testing.TB, ln net.Listener) (stop func())
}

var frameworks = []framework{
	{"qilat", serveQilatCompare},
	{"qinetic", serveQineticCompare},
	{"fiber", serveFiberCompare},
}

func jsonBody() []byte {
	b, _ := json.Marshal(message{Message: helloBody})
	return b
}

// serveQilatCompare is plain qilat with its ServeMux and qilat.Chain, the floor Qinetic is measured against.
func serveQilatCompare(tb testing.TB, ln net.Listener) func() {
	text := func(w qilat.ResponseWriter, s string) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.WriteString(s)
	}
	mux := qilat.NewServeMux()
	mux.HandleFunc("GET /plaintext", func(w qilat.ResponseWriter, r *qilat.Request) { text(w, helloBody) })
	mux.HandleFunc("GET /json", func(w qilat.ResponseWriter, r *qilat.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jsonBody())
	})
	mux.HandleFunc("GET /users/{id}", func(w qilat.ResponseWriter, r *qilat.Request) { text(w, r.PathValue("id")) })
	for i := range routeCount {
		name := "r" + strconv.Itoa(i)
		mux.HandleFunc("GET /"+name+"/items/{id}", func(w qilat.ResponseWriter, r *qilat.Request) {
			text(w, name+" "+r.PathValue("id"))
		})
	}
	pass := func(next qilat.Handler) qilat.Handler { return next }
	stack := qilat.Chain(pass, pass, pass, pass, pass)
	mux.Handle("GET /mw/plaintext", stack(qilat.HandlerFunc(func(w qilat.ResponseWriter, r *qilat.Request) {
		text(w, helloBody)
	})))
	srv := &qilat.Server{Handler: mux, Logger: slog.New(slog.DiscardHandler)}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		if err := <-done; !errors.Is(err, qilat.ErrServerClosed) {
			tb.Errorf("qilat: %v", err)
		}
	}
}

func serveQineticCompare(tb testing.TB, ln net.Listener) func() {
	app := qinetic.New(qinetic.Config{Logger: slog.New(slog.DiscardHandler)})
	app.Get("/plaintext", func(c *qinetic.Ctx) error { return c.SendString(helloBody) })
	app.Get("/json", func(c *qinetic.Ctx) error { return c.JSON(message{Message: helloBody}) })
	app.Get("/users/:id", func(c *qinetic.Ctx) error { return c.SendString(c.Params("id")) })
	for i := range routeCount {
		name := "r" + strconv.Itoa(i)
		app.Get("/"+name+"/items/:id", func(c *qinetic.Ctx) error { return c.SendString(name + " " + c.Params("id")) })
	}
	mw := app.Group("/mw")
	for range 5 {
		mw.Use(func(c *qinetic.Ctx) error { return c.Next() })
	}
	mw.Get("/plaintext", func(c *qinetic.Ctx) error { return c.SendString(helloBody) })
	done := make(chan error, 1)
	go func() { done <- app.Listener(ln) }()
	return func() {
		_ = app.ShutdownWithTimeout(5 * time.Second)
		if err := <-done; !errors.Is(err, qilat.ErrServerClosed) {
			tb.Errorf("qinetic: %v", err)
		}
	}
}

func serveFiberCompare(tb testing.TB, ln net.Listener) func() {
	// Fiber has no read timeout by default; qilat bounds every request header (ReadHeaderTimeout, 10 s) and
	// idle connection (120 s). Matching them makes all three pay for the same slowloris protection.
	app := fiber.New(fiber.Config{ReadTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second})
	app.Get("/plaintext", func(c fiber.Ctx) error { return c.SendString(helloBody) })
	app.Get("/json", func(c fiber.Ctx) error { return c.JSON(message{Message: helloBody}) })
	app.Get("/users/:id", func(c fiber.Ctx) error { return c.SendString(c.Params("id")) })
	for i := range routeCount {
		name := "r" + strconv.Itoa(i)
		app.Get("/"+name+"/items/:id", func(c fiber.Ctx) error { return c.SendString(name + " " + c.Params("id")) })
	}
	mw := app.Group("/mw")
	for range 5 {
		mw.Use(func(c fiber.Ctx) error { return c.Next() })
	}
	mw.Get("/plaintext", func(c fiber.Ctx) error { return c.SendString(helloBody) })
	done := make(chan error, 1)
	go func() { done <- app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true}) }()
	return func() {
		_ = app.ShutdownWithTimeout(5 * time.Second)
		if err := <-done; err != nil {
			tb.Errorf("fiber: %v", err)
		}
	}
}

// transport is how the client reaches the server: an in-memory listener measures server CPU without the
// kernel, loopback TCP adds the real socket path.
type transport struct {
	name   string
	listen func(tb testing.TB) (net.Listener, func() (net.Conn, error))
}

var transports = []transport{
	{"inmem", func(testing.TB) (net.Listener, func() (net.Conn, error)) {
		ln := fasthttputil.NewInmemoryListener()
		return ln, ln.Dial
	}},
	{"loopback", func(tb testing.TB) (net.Listener, func() (net.Conn, error)) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			tb.Fatal(err)
		}
		addr := ln.Addr().String()
		return ln, func() (net.Conn, error) { return net.Dial("tcp", addr) }
	}},
}

func dialConn(dial func() (net.Conn, error), path string) (*h1Conn, error) {
	conn, err := dial()
	if err != nil {
		return nil, err
	}
	return &h1Conn{conn: conn, br: bufio.NewReader(conn), req: fmt.Appendf(nil, "GET %s HTTP/1.1\r\nHost: bench\r\n\r\n", path)}, nil
}

// TestCompare checks every framework gives every scenario the same answer, so the benchmarks compare
// equal work.
func TestCompare(t *testing.T) {
	for _, tr := range transports {
		for _, fw := range frameworks {
			t.Run(tr.name+"/"+fw.name, func(t *testing.T) {
				ln, dial := tr.listen(t)
				defer fw.serve(t, ln)()
				for _, sc := range compareScenarios {
					c, err := dialConn(dial, sc.path)
					if err != nil {
						t.Fatal(err)
					}
					for range 2 {
						var body bytesBuffer
						if err := c.roundTrip(&body); err != nil {
							t.Fatalf("%s: %v", sc.name, err)
						}
						if string(body) != sc.want {
							t.Fatalf("%s: body %q, want %q", sc.name, body, sc.want)
						}
					}
					_ = c.conn.Close()
				}
			})
		}
	}
}

// BenchmarkCompare is qilat, Qinetic, and Fiber v3 on the same scenarios, transports, and client:
// keep-alive HTTP/1.1, 8 connections per GOMAXPROCS.
func BenchmarkCompare(b *testing.B) {
	for _, tr := range transports {
		for _, sc := range compareScenarios {
			for _, fw := range frameworks {
				b.Run(tr.name+"/"+sc.name+"/"+fw.name, func(b *testing.B) {
					ln, dial := tr.listen(b)
					defer fw.serve(b, ln)()
					b.ReportAllocs()
					b.SetParallelism(8)
					b.RunParallel(func(pb *testing.PB) {
						c, err := dialConn(dial, sc.path)
						if err != nil {
							b.Error(err)
							return
						}
						defer func() { _ = c.conn.Close() }()
						for pb.Next() {
							if err := c.roundTrip(io.Discard); err != nil {
								b.Error(err)
								return
							}
						}
					})
				})
			}
		}
	}
}
