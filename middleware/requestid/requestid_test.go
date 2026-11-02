package requestid

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erizeta/qinetic"
)

func TestRequestID(t *testing.T) {
	tests := []struct {
		name, incoming string
		keep           bool
	}{
		{"valid", "abc-123_X.y:z", true},
		{"none", "", false},
		{"max length", strings.Repeat("a", 128), true},
		{"too long", strings.Repeat("a", 129), false},
		{"space", "a b", false},
		{"unicode", "idé", false},
		{"slash", "a/b", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seen string
			app := qinetic.New()
			app.Use(New())
			app.Get("/", func(c *qinetic.Ctx) error {
				seen = FromContext(c)
				return nil
			})
			req := httptest.NewRequest("GET", "/", nil)
			if tt.incoming != "" {
				req.Header["X-Request-Id"] = []string{tt.incoming}
			}
			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			got := resp.Header.Get(DefaultHeader)
			if got != seen || got == "" {
				t.Fatalf("response ID %q, FromContext %q", got, seen)
			}
			if tt.keep != (got == tt.incoming) {
				t.Fatalf("incoming %q, got %q, want kept=%v", tt.incoming, got, tt.keep)
			}
			if !tt.keep && (len(got) != 26 || !valid(got)) {
				t.Fatalf("generated ID %q", got)
			}
		})
	}
}

func TestRequestIDConfig(t *testing.T) {
	app := qinetic.New()
	app.Use(New(Config{Header: "X-Trace", Generator: func() string { return "fixed" },
		Next: func(c *qinetic.Ctx) bool { return c.Path() == "/skip" }}))
	app.Get("/*", func(c *qinetic.Ctx) error { return c.SendString(FromContext(c)) })
	resp, _ := app.Test(httptest.NewRequest("GET", "/", nil))
	_ = resp.Body.Close()
	if resp.Header.Get("X-Trace") != "fixed" {
		t.Fatalf("X-Trace = %q", resp.Header.Get("X-Trace"))
	}
	resp, _ = app.Test(httptest.NewRequest("GET", "/skip", nil))
	_ = resp.Body.Close()
	if resp.Header.Get("X-Trace") != "" {
		t.Fatal("Next did not skip")
	}
}
