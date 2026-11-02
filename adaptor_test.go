package qinetic

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erizeta/qilat"
)

// observe is middleware that records what handlers before an adapter see once the chain returns.
func observe(status *int, size *int64, committed *bool) Handler {
	return func(c *Ctx) error {
		err := c.Next()
		*status, *size, *committed = c.StatusCode(), c.ResponseSize(), c.committed
		return err
	}
}

// tagHeader is qilat middleware that adds a header, standing in for compress and similar wrappers.
func tagHeader(next qilat.Handler) qilat.Handler {
	return qilat.HandlerFunc(func(w qilat.ResponseWriter, r *qilat.Request) {
		w.Header().Set("X-Wrapped", "yes")
		next.ServeHTTP(w, r)
	})
}

func TestFromQilatMiddlewareRunsTheChainInside(t *testing.T) {
	var status int
	var size int64
	var committed bool
	app := New()
	app.Use(observe(&status, &size, &committed))
	app.Use(FromQilatMiddleware(tagHeader))
	app.Get("/", func(c *Ctx) error { return c.Status(201).SendString("hello") })
	resp, body := get(t, app, "GET", "/")
	if resp.StatusCode != 201 || body != "hello" || resp.Header.Get("X-Wrapped") != "yes" {
		t.Fatalf("got %d %q, X-Wrapped %q", resp.StatusCode, body, resp.Header.Get("X-Wrapped"))
	}
	if status != 201 || size != 5 || !committed {
		t.Fatalf("outer handler saw status %d size %d committed %v", status, size, committed)
	}
}

func TestFromQilatMiddlewareErrorsUseErrorHandler(t *testing.T) {
	app := New(Config{ErrorHandler: func(c *Ctx, err error) error {
		return c.Status(418).SendString("handled: " + err.Error())
	}})
	app.Use(FromQilatMiddleware(tagHeader))
	app.Get("/", func(c *Ctx) error {
		_ = c.SendString("discarded")
		return errors.New("boom")
	})
	resp, body := get(t, app, "GET", "/")
	// The error was rendered inside the middleware, so it wrapped the error response too.
	if resp.StatusCode != 418 || body != "handled: boom" || resp.Header.Get("X-Wrapped") != "yes" {
		t.Fatalf("got %d %q, X-Wrapped %q", resp.StatusCode, body, resp.Header.Get("X-Wrapped"))
	}
}

func TestFromQilatMiddlewareThatAnswersItself(t *testing.T) {
	deny := func(qilat.Handler) qilat.Handler {
		return qilat.HandlerFunc(func(w qilat.ResponseWriter, r *qilat.Request) {
			w.WriteHeader(401)
			_, _ = w.WriteString("no entry")
		})
	}
	var status int
	var size int64
	var committed bool
	ran := false
	app := New()
	app.Use(observe(&status, &size, &committed))
	app.Use(FromQilatMiddleware(deny))
	app.Get("/", func(c *Ctx) error {
		ran = true
		return nil
	})
	resp, body := get(t, app, "GET", "/")
	if resp.StatusCode != 401 || body != "no entry" || ran {
		t.Fatalf("got %d %q, route ran %v", resp.StatusCode, body, ran)
	}
	if status != 401 || size != 8 || !committed {
		t.Fatalf("outer handler saw status %d size %d committed %v", status, size, committed)
	}
}

func TestFromQilatMiddlewareReplacesRequest(t *testing.T) {
	var path, param, outerPath string
	app := New()
	app.Use(func(c *Ctx) error {
		err := c.Next()
		outerPath = c.Path()
		return err
	})
	app.Use("/api", FromQilatMiddleware(func(next qilat.Handler) qilat.Handler { return qilat.StripPrefix("/api", next) }))
	app.Get("/api/users/:id", func(c *Ctx) error {
		path, param = c.Path(), c.Params("id")
		return nil
	})
	get(t, app, "GET", "/api/users/7")
	// Inside, the request is StripPrefix's; routing and parameters still come from the original path.
	if path != "/users/7" || param != "7" || outerPath != "/api/users/7" {
		t.Fatalf("inner path %q param %q, outer path %q", path, param, outerPath)
	}
}

func TestFromQilatMiddlewarePanicReachesOuterRecover(t *testing.T) {
	app := New()
	app.Use(func(c *Ctx) (err error) {
		defer func() {
			if v := recover(); v != nil {
				err = fmt.Errorf("recovered: %v", v)
			}
		}()
		return c.Next()
	})
	app.Use(FromQilatMiddleware(tagHeader))
	app.Get("/", func(c *Ctx) error { panic("inside") })
	logger, _ := testLogger()
	app.config.Logger = logger
	resp, body := get(t, app, "GET", "/")
	if resp.StatusCode != 500 || body != "Internal Server Error" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
}

// rangeHandler answers 206, as qilat.ServeContent does for a Range request, and streams with ReadFrom.
type rangeHandler struct{ sawReaderFrom bool }

func (h *rangeHandler) ServeHTTP(w qilat.ResponseWriter, r *qilat.Request) {
	_, h.sawReaderFrom = w.(io.ReaderFrom)
	w.Header().Set("Content-Length", "4")
	w.WriteHeader(206)
	_, _ = io.Copy(w, strings.NewReader("part"))
}

func TestFromQilatRecordsStatusAndSize(t *testing.T) {
	var status int
	var size int64
	var committed bool
	h := &rangeHandler{}
	app := New()
	app.Use(observe(&status, &size, &committed))
	app.Get("/", func(c *Ctx) error {
		_ = c.SendString("discarded")
		return FromQilat(h)(c)
	})
	resp, body := get(t, app, "GET", "/")
	if resp.StatusCode != 206 || body != "part" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	if status != 206 || size != 4 || !committed || !h.sawReaderFrom {
		t.Fatalf("status %d size %d committed %v ReaderFrom %v", status, size, committed, h.sawReaderFrom)
	}
}

func TestFromStdAndFromStdMiddleware(t *testing.T) {
	app := New()
	app.Use(FromStdMiddleware(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Std", "mw")
			next.ServeHTTP(w, r)
		})
	}))
	app.Get("/std", FromStd(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(202)
		_, _ = io.WriteString(w, "from net/http "+r.URL.Path)
	})))
	resp, body := do(t, app, httptest.NewRequest("GET", "/std", nil))
	if resp.StatusCode != 202 || body != "from net/http /std" || resp.Header.Get("X-Std") != "mw" {
		t.Fatalf("got %d %q, X-Std %q", resp.StatusCode, body, resp.Header.Get("X-Std"))
	}
}

func TestHandleErrorAndStatusCode(t *testing.T) {
	var before, after int
	app := New()
	app.Use(func(c *Ctx) error {
		err := c.Next()
		before = c.StatusCode()
		c.HandleError(err)
		c.HandleError(nil)
		after = c.StatusCode()
		return nil
	})
	resp, _ := get(t, app, "GET", "/missing")
	if before != 200 || after != 404 || resp.StatusCode != 404 {
		t.Fatalf("status before %d after %d, response %d", before, after, resp.StatusCode)
	}
}

// TestWritesAfterAdapterAreRefused guards against appending raw bytes after a body another handler framed
// or encoded, such as a gzip stream, which would corrupt the response.
func TestWritesAfterAdapterAreRefused(t *testing.T) {
	var errs []error
	app := New()
	app.Use(func(c *Ctx) error {
		if err := c.Next(); err != nil {
			return err
		}
		_, werr := c.Writer().Write([]byte("w"))
		errs = append(errs, c.SendString("late"), c.Send([]byte("late")), c.JSON(1), c.Flush(), werr,
			c.SendStream(strings.NewReader("late")), FromQilat(&rangeHandler{})(c))
		return nil
	})
	app.Use(FromQilatMiddleware(tagHeader))
	app.Get("/", func(c *Ctx) error { return c.SendString("body") })
	app.Get("/file", FromQilat(&rangeHandler{}))
	resp, body := get(t, app, "GET", "/")
	if resp.StatusCode != 200 || body != "body" {
		t.Fatalf("got %d %q, want the response untouched", resp.StatusCode, body)
	}
	for i, err := range errs {
		if !errors.Is(err, ErrResponseSent) {
			t.Errorf("late write %d: %v, want ErrResponseSent", i, err)
		}
	}
	errs = nil
	if _, body = get(t, app, "GET", "/file"); body != "part" {
		t.Fatalf("after FromQilat: body %q", body)
	}
	if len(errs) != 7 || !errors.Is(errs[0], ErrResponseSent) {
		t.Fatalf("after FromQilat: %v", errs)
	}
}

func TestFromQilatAfterCommit(t *testing.T) {
	var err error
	app := New()
	app.Get("/", func(c *Ctx) error {
		_ = c.SendString("streamed")
		_ = c.Flush()
		err = FromQilat(&rangeHandler{})(c)
		return nil
	})
	if _, body := get(t, app, "GET", "/"); body != "streamed" || err == nil {
		t.Fatalf("body %q, err %v", body, err)
	}
}
