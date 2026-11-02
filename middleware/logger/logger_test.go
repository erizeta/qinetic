package logger

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/erizeta/qinetic"
	"github.com/erizeta/qinetic/middleware/recovery"
	"github.com/erizeta/qinetic/middleware/requestid"
)

func records(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	dec := json.NewDecoder(buf)
	for dec.More() {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func TestLogger(t *testing.T) {
	var buf bytes.Buffer
	app := qinetic.New(qinetic.Config{Logger: slog.New(slog.DiscardHandler)})
	app.Use(requestid.New(), New(Config{Logger: slog.New(slog.NewJSONHandler(&buf, nil))}), recovery.New(recovery.Config{
		Logger: slog.New(slog.DiscardHandler)}))
	app.Get("/ok", func(c *qinetic.Ctx) error { return c.SendString("hello") })
	app.Get("/fail", func(c *qinetic.Ctx) error { return errors.New("internal detail") })
	app.Get("/panic", func(c *qinetic.Ctx) error { panic("boom") })

	for _, target := range []string{"/ok?token=secret", "/missing", "/fail", "/panic"} {
		req := httptest.NewRequest("GET", target, nil)
		req.Header.Set("X-Request-ID", "rid-1")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	recs := records(t, &buf)
	want := []struct {
		level, path string
		status      float64
		bytes       float64
	}{
		{"INFO", "/ok", 200, 5}, {"WARN", "/missing", 404, 9}, {"ERROR", "/fail", 500, 21}, {"ERROR", "/panic", 500, 21},
	}
	if len(recs) != len(want) {
		t.Fatalf("%d records: %v", len(recs), recs)
	}
	for i, w := range want {
		r := recs[i]
		if r["level"] != w.level || r["path"] != w.path || r["status"] != w.status || r["bytes"] != w.bytes ||
			r["method"] != "GET" || r["proto"] != "HTTP/1.1" || r["ip"] != "192.0.2.1" || r["request_id"] != "rid-1" ||
			r["msg"] != "request" || r["latency"] == nil {
			t.Errorf("record %d = %v, want %+v", i, r, w)
		}
	}
}

func TestLoggerFieldsAndMessage(t *testing.T) {
	var buf bytes.Buffer
	app := qinetic.New()
	app.Use(New(Config{Logger: slog.New(slog.NewJSONHandler(&buf, nil)), Message: "http", Fields: []string{FieldStatus, FieldRequestID}}))
	app.Get("/", func(c *qinetic.Ctx) error { return nil })
	resp, _ := app.Test(httptest.NewRequest("GET", "/", nil))
	_ = resp.Body.Close()
	recs := records(t, &buf)
	// request_id is left out when the requestid middleware did not run.
	if len(recs) != 1 || recs[0]["msg"] != "http" || recs[0]["status"] != float64(200) || len(recs[0]) != 4 {
		t.Fatalf("records %v", recs)
	}
}

func TestLoggerUnknownFieldPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	New(Config{Fields: []string{"user_agent"}})
}

func TestLoggerNext(t *testing.T) {
	var buf bytes.Buffer
	app := qinetic.New()
	app.Use(New(Config{Logger: slog.New(slog.NewJSONHandler(&buf, nil)), Next: func(c *qinetic.Ctx) bool { return true }}))
	app.Get("/", func(c *qinetic.Ctx) error { return nil })
	resp, _ := app.Test(httptest.NewRequest("GET", "/", nil))
	_ = resp.Body.Close()
	if buf.Len() != 0 {
		t.Fatalf("logged despite Next: %s", buf.String())
	}
}
