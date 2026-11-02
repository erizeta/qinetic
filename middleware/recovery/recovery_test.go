package recovery

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erizeta/qilat"
	"github.com/erizeta/qinetic"
)

func call(t *testing.T, app *qinetic.App, target string) (int, string) {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest("GET", target, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestRecoverGoesThroughErrorHandler(t *testing.T) {
	var logs bytes.Buffer
	var handled error
	app := qinetic.New(qinetic.Config{ErrorHandler: func(c *qinetic.Ctx, err error) error {
		handled = err
		return c.Status(503).SendString("custom")
	}})
	app.Use(New(Config{Logger: slog.New(slog.NewTextHandler(&logs, nil)), EnableStackTrace: true}))
	app.Get("/", func(c *qinetic.Ctx) error {
		_ = c.SendString("partial")
		panic("db exploded")
	})
	code, body := call(t, app, "/")
	if code != 503 || body != "custom" || handled != qinetic.ErrInternalServerError {
		t.Fatalf("got %d %q, handled %v", code, body, handled)
	}
	if !strings.Contains(logs.String(), "db exploded") || !strings.Contains(logs.String(), "stack=") {
		t.Fatalf("panic not logged with stack: %s", logs.String())
	}
}

func TestRecoverDefaultHandlerAndNoStack(t *testing.T) {
	var logs bytes.Buffer
	app := qinetic.New()
	app.Use(New(Config{Logger: slog.New(slog.NewTextHandler(&logs, nil))}))
	app.Get("/", func(c *qinetic.Ctx) error { panic(42) })
	if code, body := call(t, app, "/"); code != 500 || body != "Internal Server Error" {
		t.Fatalf("got %d %q", code, body)
	}
	if strings.Contains(logs.String(), "stack=") || !strings.Contains(logs.String(), "panic=42") {
		t.Fatalf("log: %s", logs.String())
	}
}

func TestRecoverRepanicsAbortHandler(t *testing.T) {
	app := qinetic.New()
	app.Use(New(Config{Logger: slog.New(slog.DiscardHandler)}))
	app.Get("/", func(c *qinetic.Ctx) error { panic(qilat.ErrAbortHandler) })
	defer func() {
		// ToStd, under app.Test, turns qilat's sentinel into net/http's.
		if v := recover(); v != http.ErrAbortHandler { //nolint:errorlint // panic values are compared
			t.Fatalf("recovered %v, want the abort sentinel to pass through", v)
		}
	}()
	call(t, app, "/")
}

func TestRecoverNext(t *testing.T) {
	app := qinetic.New()
	app.Use(New(Config{Next: func(c *qinetic.Ctx) bool { return c.Path() == "/skip" }}))
	app.Get("/skip", func(c *qinetic.Ctx) error { panic("unrecovered") })
	defer func() {
		if v := recover(); v != "unrecovered" {
			t.Fatalf("recovered %v", v)
		}
	}()
	call(t, app, "/skip")
}
