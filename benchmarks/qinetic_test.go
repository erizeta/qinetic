package benchmarks

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erizeta/qilat"
	"github.com/erizeta/qinetic"
)

// qineticApp serves the baseline scenarios through qinetic's router and Ctx.
func qineticApp() *qinetic.App {
	app := qinetic.New()
	app.Get("/hello", func(c *qinetic.Ctx) error {
		return c.SendString(helloBody)
	})
	app.Get("/json", func(c *qinetic.Ctx) error {
		return c.JSON(message{Message: helloBody})
	})
	return app
}

// json.Marshal, which c.JSON uses, omits the newline json.Encoder adds in the baseline.
func qineticWant(want string) string { return strings.TrimSuffix(want, "\n") }

func TestQinetic(t *testing.T) {
	app := qineticApp()
	addr := startQilat(t, app)
	for _, sc := range scenarios {
		t.Run("inmem/"+sc.name, func(t *testing.T) {
			resp, err := app.Test(httptest.NewRequest(http.MethodGet, sc.path, nil))
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK || string(body) != qineticWant(sc.want) {
				t.Fatalf("got %d %q", resp.StatusCode, body)
			}
		})
		t.Run("h1/"+sc.name, func(t *testing.T) {
			c, err := dialH1(addr, sc.path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.conn.Close() }()
			for range 2 {
				var body bytesBuffer
				if err := c.roundTrip(&body); err != nil {
					t.Fatal(err)
				}
				if string(body) != qineticWant(sc.want) {
					t.Fatalf("body %q", body)
				}
			}
		})
	}
}

// BenchmarkQineticInmem is BenchmarkBaselineInmem through qinetic; the difference is the framework's cost.
func BenchmarkQineticInmem(b *testing.B) {
	std := qilat.ToStd(qineticApp())
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

// BenchmarkQineticH1 is BenchmarkBaselineH1 through qinetic.
func BenchmarkQineticH1(b *testing.B) {
	addr := startQilat(b, qineticApp())
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
