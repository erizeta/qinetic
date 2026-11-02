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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/erizeta/qilat"
)

const helloBody = "Hello, World!"

type message struct {
	Message string `json:"message"`
}

var wantJSON = `{"message":"Hello, World!"}` + "\n"

// baselineHandler is plain qilat with the routing and encoding a framework handler would do,
// so later Qinetic numbers show only the framework's own cost.
func baselineHandler(w qilat.ResponseWriter, r *qilat.Request) {
	switch string(r.Path()) {
	case "/hello":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.WriteString(helloBody)
	case "/json":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(message{Message: helloBody})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

var scenarios = []struct {
	name, path, want string
}{
	{"hello", "/hello", helloBody},
	{"json", "/json", wantJSON},
}

func startQilat(tb testing.TB, h qilat.Handler) string {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("listen: %v", err)
	}
	srv := &qilat.Server{
		Handler:     h,
		ReadTimeout: 10 * time.Second,
		Logger:      slog.New(slog.DiscardHandler),
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	tb.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			tb.Errorf("shutdown: %v", err)
		}
		if err := <-done; !errors.Is(err, qilat.ErrServerClosed) {
			tb.Errorf("serve: %v", err)
		}
	})
	return ln.Addr().String()
}

// h1Conn is a keep-alive HTTP/1.1 client without net/http's transport, which would dominate the profile.
type h1Conn struct {
	conn net.Conn
	br   *bufio.Reader
	req  []byte
}

func dialH1(addr, path string) (*h1Conn, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &h1Conn{
		conn: conn,
		br:   bufio.NewReader(conn),
		req:  fmt.Appendf(nil, "GET %s HTTP/1.1\r\nHost: bench\r\n\r\n", path),
	}, nil
}

func (c *h1Conn) roundTrip(body io.Writer) error {
	if _, err := c.conn.Write(c.req); err != nil {
		return err
	}
	resp, err := http.ReadResponse(c.br, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	if resp.ContentLength < 0 {
		return errors.New("response is chunked, want Content-Length")
	}
	_, err = io.Copy(body, resp.Body)
	return err
}

// TestBaseline checks both transports answer every scenario correctly, so benchmarks compare equal work.
func TestBaseline(t *testing.T) {
	std := qilat.ToStd(qilat.HandlerFunc(baselineHandler))
	addr := startQilat(t, qilat.HandlerFunc(baselineHandler))
	for _, sc := range scenarios {
		t.Run("inmem/"+sc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			std.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, sc.path, nil))
			if rec.Code != http.StatusOK || rec.Body.String() != sc.want {
				t.Fatalf("got %d %q, want 200 %q", rec.Code, rec.Body.String(), sc.want)
			}
		})
		t.Run("h1/"+sc.name, func(t *testing.T) {
			c, err := dialH1(addr, sc.path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.conn.Close() }()
			// Two round trips on one connection also check keep-alive.
			for range 2 {
				var body bytesBuffer
				if err := c.roundTrip(&body); err != nil {
					t.Fatal(err)
				}
				if string(body) != sc.want {
					t.Fatalf("body %q, want %q", body, sc.want)
				}
			}
		})
	}
}

type bytesBuffer []byte

func (b *bytesBuffer) Write(p []byte) (int, error) {
	*b = append(*b, p...)
	return len(p), nil
}

// BenchmarkBaselineInmem calls the handler through qilat.ToStd, the path app.Test will use.
func BenchmarkBaselineInmem(b *testing.B) {
	std := qilat.ToStd(qilat.HandlerFunc(baselineHandler))
	for _, sc := range scenarios {
		b.Run(sc.name, func(b *testing.B) {
			req := httptest.NewRequest(http.MethodGet, sc.path, nil)
			b.ReportAllocs()
			for b.Loop() {
				rec := httptest.NewRecorder()
				std.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					b.Fatalf("status %d", rec.Code)
				}
			}
		})
	}
}

// BenchmarkBaselineH1 drives a loopback qilat server with one keep-alive connection per goroutine.
func BenchmarkBaselineH1(b *testing.B) {
	addr := startQilat(b, qilat.HandlerFunc(baselineHandler))
	for _, sc := range scenarios {
		b.Run(sc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetParallelism(8)
			b.RunParallel(func(pb *testing.PB) {
				c, err := dialH1(addr, sc.path)
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
