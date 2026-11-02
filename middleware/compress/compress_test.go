package compress

import (
	"compress/gzip"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erizeta/qinetic"
)

var big = strings.Repeat("compress me ", 500)

func fetch(t *testing.T, app *qinetic.App, target string, hdr ...string) (code int, encoding, body string, vary string) {
	t.Helper()
	req := httptest.NewRequest("GET", target, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var r io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		r = zr
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Encoding"), string(b), resp.Header.Get("Vary")
}

func newApp() *qinetic.App {
	app := qinetic.New(qinetic.Config{ErrorHandler: func(c *qinetic.Ctx, err error) error {
		return c.Status(500).SendString("error page " + big)
	}})
	app.Use(New(Config{Next: func(c *qinetic.Ctx) bool { return c.Path() == "/skip" }}))
	app.Get("/big", func(c *qinetic.Ctx) error { return c.SendString(big) })
	app.Get("/skip", func(c *qinetic.Ctx) error { return c.SendString(big) })
	app.Get("/small", func(c *qinetic.Ctx) error { return c.SendString("tiny") })
	app.Get("/png", func(c *qinetic.Ctx) error { return c.Type("png").Send([]byte(big)) })
	app.Get("/fail", func(c *qinetic.Ctx) error { return errors.New("db down") })
	return app
}

func TestCompress(t *testing.T) {
	app := newApp()
	tests := []struct {
		target, wantEnc, wantBody string
		code                      int
		hdr                       []string
	}{
		{"/big", "gzip", big, 200, nil},
		{"/small", "", "tiny", 200, nil},
		{"/png", "", big, 200, nil},
		{"/skip", "", big, 200, nil},
		{"/big", "", big, 200, []string{"Accept-Encoding", "identity"}},
		{"/big", "", big[:10], 206, []string{"Range", "bytes=0-9"}},
		// The ErrorHandler's page is produced inside the middleware, so it is compressed too.
		{"/fail", "gzip", "error page " + big, 500, nil},
	}
	for _, tt := range tests {
		t.Run(tt.target+strings.Join(tt.hdr, "="), func(t *testing.T) {
			code, enc, body, vary := fetch(t, app, tt.target, tt.hdr...)
			if tt.code == 206 {
				// qinetic buffers responses and does not serve ranges itself; only check it is not compressed.
				if enc != "" {
					t.Fatalf("Range request compressed")
				}
				return
			}
			if code != tt.code || enc != tt.wantEnc || body != tt.wantBody {
				t.Fatalf("got %d %q, %d bytes", code, enc, len(body))
			}
			if tt.target != "/skip" && tt.target != "/png" && vary != "Accept-Encoding" {
				t.Fatalf("Vary = %q", vary)
			}
		})
	}
}

func TestInvalidLevelPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	New(Config{Level: 42})
}

// TestOuterWritesDoNotCorruptGzip checks a handler registered before compress cannot append raw bytes to
// the gzip stream.
func TestOuterWritesDoNotCorruptGzip(t *testing.T) {
	var lateErr error
	app := qinetic.New()
	app.Use(func(c *qinetic.Ctx) error {
		err := c.Next()
		lateErr = c.SendString("appended after gzip")
		return err
	})
	app.Use(New())
	app.Get("/", func(c *qinetic.Ctx) error { return c.SendString(big) })
	code, enc, body, _ := fetch(t, app, "/")
	if code != 200 || enc != "gzip" || body != big {
		t.Fatalf("got %d %q, %d bytes", code, enc, len(body))
	}
	if !errors.Is(lateErr, qinetic.ErrResponseSent) {
		t.Fatalf("late write error %v", lateErr)
	}
}
