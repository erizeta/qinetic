package qinetic

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erizeta/qilat"
)

func respond(t *testing.T, h Handler, req ...*http.Request) (testResponse, string) {
	t.Helper()
	app := New()
	app.All("/*", h)
	r := httptest.NewRequest("GET", "/", nil)
	if len(req) > 0 {
		r = req[0]
	}
	return do(t, app, r)
}

func TestSendVariants(t *testing.T) {
	tests := []struct {
		name   string
		h      Handler
		code   int
		ctype  string
		body   string
		header [2]string
	}{
		{"SendString", func(c *Ctx) error { return c.SendString("hi") }, 200, "text/plain; charset=utf-8", "hi", [2]string{}},
		{"SendString keeps type", func(c *Ctx) error { return c.Type("html").SendString("<b>") }, 200, "text/html; charset=utf-8", "<b>", [2]string{}},
		{"Send has no type", func(c *Ctx) error { return c.Send([]byte{1, 2}) }, 200, "", "\x01\x02", [2]string{}},
		{"SendStatus", func(c *Ctx) error { return c.SendStatus(418) }, 418, "text/plain; charset=utf-8", "I'm a teapot", [2]string{}},
		{"SendStatus keeps body", func(c *Ctx) error { _ = c.SendString("mine"); return c.SendStatus(201) }, 201, "text/plain; charset=utf-8", "mine", [2]string{}},
		{"JSON", func(c *Ctx) error { return c.JSON(map[string]int{"a": 1}) }, 200, "application/json", `{"a":1}`, [2]string{}},
		{"XML", func(c *Ctx) error {
			return c.XML(struct {
				XMLName struct{} `xml:"x"`
				A       int      `xml:"a"`
			}{A: 1})
		}, 200, "application/xml", "<x><a>1</a></x>", [2]string{}},
		{"Type ext", func(c *Ctx) error { c.Type(".json"); return c.Send(nil) }, 200, "application/json", "", [2]string{}},
		{"Type charset", func(c *Ctx) error { c.Type("html", "iso-8859-1"); return nil }, 200, "text/html; charset=iso-8859-1", "", [2]string{}},
		{"Type unknown", func(c *Ctx) error { c.Type("nope-ext"); return nil }, 200, "application/octet-stream", "", [2]string{}},
		{"Type full", func(c *Ctx) error { c.Type("application/x-custom"); return nil }, 200, "application/x-custom", "", [2]string{}},
		{"Set sanitizes", func(c *Ctx) error { c.Set("X-A", "v\r\nX-Injected: 1"); return nil }, 200, "", "", [2]string{"X-A", "v  X-Injected: 1"}},
		{"Append", func(c *Ctx) error {
			c.Append("X-L", "a", "b")
			c.Append("X-L", "b", "c", "")
			return nil
		}, 200, "", "", [2]string{"X-L", "a, b, c"}},
		{"Vary", func(c *Ctx) error { c.Vary("Origin"); c.Vary("Accept", "Origin"); return nil }, 200, "", "", [2]string{"Vary", "Origin, Accept"}},
		{"GetRespHeader", func(c *Ctx) error {
			c.Set("X-A", "1")
			return c.SendString(c.GetRespHeader("X-A") + c.GetRespHeader("X-B", "d"))
		}, 200, "text/plain; charset=utf-8", "1d", [2]string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := respond(t, tt.h)
			if resp.StatusCode != tt.code || resp.Header.Get("Content-Type") != tt.ctype || body != tt.body {
				t.Fatalf("got %d %q %q, want %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), body,
					tt.code, tt.ctype, tt.body)
			}
			if tt.header[0] != "" && resp.Header.Get(tt.header[0]) != tt.header[1] {
				t.Fatalf("%s = %q, want %q", tt.header[0], resp.Header.Get(tt.header[0]), tt.header[1])
			}
		})
	}
}

func TestJSONEncoderConfig(t *testing.T) {
	app := New(Config{JSONEncoder: func(v any) ([]byte, error) { return []byte(`"custom"`), nil }})
	app.Get("/", func(c *Ctx) error { return c.JSON(1) })
	if _, body := get(t, app, "GET", "/"); body != `"custom"` {
		t.Fatalf("body %q", body)
	}
	app = New()
	app.Get("/", func(c *Ctx) error { return c.JSON(make(chan int)) })
	if resp, _ := get(t, app, "GET", "/"); resp.StatusCode != 500 {
		t.Fatalf("unencodable value: status %d", resp.StatusCode)
	}
}

func TestRedirect(t *testing.T) {
	resp, body := respond(t, func(c *Ctx) error {
		_ = c.SendString("discarded")
		return c.Redirect("/next?a=1")
	})
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/next?a=1" || body != "" {
		t.Fatalf("got %d Location %q body %q", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	if resp, _ = respond(t, func(c *Ctx) error { return c.Redirect("/x", 301) }); resp.StatusCode != 301 {
		t.Fatalf("code: got %d", resp.StatusCode)
	}
	for _, loc := range []string{"/x\r\nSet-Cookie: a=b", "/x\nX: y", "/x\x00", "/x\x7f"} {
		resp, _ := respond(t, func(c *Ctx) error { return c.Redirect(loc) })
		if resp.StatusCode != 500 || resp.Header.Get("Location") != "" || resp.Header.Get("Set-Cookie") != "" {
			t.Errorf("Redirect(%q): got %d Location %q", loc, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	if resp, _ = respond(t, func(c *Ctx) error { return c.Redirect("/x", 200) }); resp.StatusCode != 500 {
		t.Fatalf("non-3xx code: got %d", resp.StatusCode)
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSendFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "hello file")
	secret := writeFile(t, t.TempDir(), "secret.txt", "secret")
	app := New()
	app.Get("/files/:name", func(c *Ctx) error {
		return c.SendFile(filepath.Join(dir, c.Params("name")))
	})
	app.Get("/download", func(c *Ctx) error { return c.Download(filepath.Join(dir, "a.txt"), "report final.txt") })
	app.Get("/dir", func(c *Ctx) error { return c.SendFile(dir) })

	resp, body := get(t, app, "GET", "/files/a.txt")
	if resp.StatusCode != 200 || body != "hello file" || resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("got %d %q %q", resp.StatusCode, body, resp.Header.Get("Content-Type"))
	}
	req := httptest.NewRequest("GET", "/files/a.txt", nil)
	req.Header.Set("Range", "bytes=0-4")
	if resp, body = do(t, app, req); resp.StatusCode != 206 || body != "hello" {
		t.Fatalf("range: got %d %q", resp.StatusCode, body)
	}
	if resp, _ = get(t, app, "GET", "/files/missing.txt"); resp.StatusCode != 404 {
		t.Fatalf("missing: got %d", resp.StatusCode)
	}
	if resp, _ = get(t, app, "GET", "/dir"); resp.StatusCode != 404 {
		t.Fatalf("directory: got %d", resp.StatusCode)
	}
	// The router never sees a ".." segment, but a decoded parameter can contain one.
	rel, err := filepath.Rel(dir, secret)
	if err != nil {
		t.Fatal(err)
	}
	escaped := strings.ReplaceAll(filepath.ToSlash(rel), "/", "%2F")
	if resp, body = get(t, app, "GET", "/files/"+escaped); resp.StatusCode != 400 || strings.Contains(body, "secret") {
		t.Fatalf("traversal %s: got %d %q", escaped, resp.StatusCode, body)
	}
	resp, body = get(t, app, "GET", "/download")
	if body != "hello file" || resp.Header.Get("Content-Disposition") != `attachment; filename="report final.txt"` {
		t.Fatalf("download: %q Content-Disposition %q", body, resp.Header.Get("Content-Disposition"))
	}
}

func TestAttachment(t *testing.T) {
	resp, _ := respond(t, func(c *Ctx) error { c.Attachment(); return nil })
	if resp.Header.Get("Content-Disposition") != "attachment" {
		t.Fatalf("got %q", resp.Header.Get("Content-Disposition"))
	}
	resp, _ = respond(t, func(c *Ctx) error { c.Attachment("dir/résumé.pdf"); return nil })
	if resp.Header.Get("Content-Disposition") != "attachment; filename*=utf-8''r%C3%A9sum%C3%A9.pdf" ||
		resp.Header.Get("Content-Type") != "application/pdf" {
		t.Fatalf("got %q %q", resp.Header.Get("Content-Disposition"), resp.Header.Get("Content-Type"))
	}
}

type closeTracker struct {
	io.Reader
	closed bool
}

func (c *closeTracker) Close() error {
	c.closed = true
	return nil
}

func TestSendStreamAndWriter(t *testing.T) {
	src := &closeTracker{Reader: strings.NewReader("streamed")}
	resp, body := respond(t, func(c *Ctx) error {
		_ = c.SendString("discarded")
		return c.Status(201).SendStream(src, 8)
	})
	if resp.StatusCode != 201 || body != "streamed" || resp.ContentLength != 8 || !src.closed {
		t.Fatalf("SendStream: %d %q length %d closed %v", resp.StatusCode, body, resp.ContentLength, src.closed)
	}
	resp, body = respond(t, func(c *Ctx) error {
		_ = c.SendString("a")
		w := c.Writer()
		_, _ = io.WriteString(w, "b")
		if err := c.Flush(); err != nil {
			return err
		}
		_ = c.SendString("c") // after commit, Send* appends
		return nil
	})
	if resp.StatusCode != 200 || body != "abc" {
		t.Fatalf("Writer: %d %q", resp.StatusCode, body)
	}
	if _, body = respond(t, func(c *Ctx) error {
		_ = c.Writer()
		if err := c.SendStream(strings.NewReader("x")); err == nil {
			return errors.New("SendStream after commit succeeded")
		}
		return nil
	}); body != "" {
		t.Fatalf("SendStream after commit wrote %q", body)
	}
}

// TestTrailers checks trailers reach a client over a real HTTP/1.1 connection and over h2c.
func TestTrailers(t *testing.T) {
	app := New()
	app.Get("/", func(c *Ctx) error {
		tr := c.Trailer()
		if err := c.SendString("body"); err != nil {
			return err
		}
		tr.Set("X-Checksum", "abc")
		return nil
	})
	base := serveWith(t, &qilat.Server{Handler: app, H2C: true})
	h1 := &http.Transport{}
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	h2 := &http.Transport{Protocols: &protocols}
	for name, tr := range map[string]*http.Transport{"HTTP/1.1": h1, "HTTP/2.0": h2} {
		t.Run(name, func(t *testing.T) {
			defer tr.CloseIdleConnections()
			resp, err := (&http.Client{Transport: tr}).Get(base)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if resp.Proto != name || string(body) != "body" || resp.Trailer.Get("X-Checksum") != "abc" {
				t.Fatalf("%s: body %q, trailer %v", resp.Proto, body, resp.Trailer)
			}
		})
	}
}

func TestCookie(t *testing.T) {
	logger, logs := testLogger()
	app := New(Config{Logger: logger})
	app.Get("/", func(c *Ctx) error {
		c.Cookie(&Cookie{Name: "session", Value: "abc", Path: "/", MaxAge: 60, Secure: true, HTTPOnly: true,
			SameSite: CookieSameSiteStrict, Partitioned: true})
		c.Cookie(&Cookie{Name: "bad name;", Value: "x"})
		c.Cookie(&Cookie{Name: "inject", Value: "a; Domain=evil.example"})
		return nil
	})
	resp, _ := get(t, app, "GET", "/")
	got := resp.Header.Values("Set-Cookie")
	want := "session=abc; Path=/; Max-Age=60; HttpOnly; Secure; SameSite=Strict; Partitioned"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("Set-Cookie = %q, want only %q", got, want)
	}
	if strings.Count(logs.String(), "invalid cookie") != 2 {
		t.Fatalf("invalid cookies not logged: %s", logs)
	}
}

func TestClearCookie(t *testing.T) {
	app := New()
	app.Get("/one", func(c *Ctx) error { c.ClearCookie("a"); return nil })
	app.Get("/all", func(c *Ctx) error { c.ClearCookie(); return nil })
	resp, _ := get(t, app, "GET", "/one")
	if got := resp.Header.Values("Set-Cookie"); len(got) != 1 || !strings.HasPrefix(got[0], "a=; Expires=Thu, 01 Jan 1970") ||
		!strings.Contains(got[0], "Max-Age=0") {
		t.Fatalf("Set-Cookie = %q", got)
	}
	req := httptest.NewRequest("GET", "/all", nil)
	req.Header.Set("Cookie", "x=1; y=2")
	resp, _ = do(t, app, req)
	if got := fmt.Sprint(resp.Header.Values("Set-Cookie")); !strings.Contains(got, "x=;") || !strings.Contains(got, "y=;") {
		t.Fatalf("Set-Cookie = %s", got)
	}
}
