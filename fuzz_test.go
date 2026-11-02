package qinetic

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erizeta/qilat"
)

var discardLogger = slog.New(slog.DiscardHandler)

type fuzzTarget struct {
	S  string   `query:"s"`
	I  int8     `query:"i"`
	U  uint16   `query:"u"`
	F  float32  `query:"f"`
	B  bool     `query:"b"`
	SS []string `query:"ss"`
	IS []int64  `query:"is"`
}

// FuzzBind feeds arbitrary query strings to the form and query decoder: it must never panic, and every
// failure must be a 400 *Error, never a 500 or a decoder message.
func FuzzBind(f *testing.F) {
	for _, seed := range []string{"s=x&i=1&u=2&f=1.5&b=on&ss=a&ss=b&is=1&is=-2", "i=128", "u=-1", "b=maybe", "f=NaN",
		"%zz=1", "S=X&I=1", "is=1&is=x", ""} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, query string) {
		var args qilat.Args
		args.Parse([]byte(query))
		var v fuzzTarget
		err := decodeValues(&v, "query", foldArgs(&args))
		if err == nil {
			return
		}
		var e *Error
		if !errors.As(err, &e) || e.Code != http.StatusBadRequest || !strings.HasPrefix(e.Message, "invalid value for ") {
			t.Fatalf("query %q: error %v, want a 400 *Error", query, err)
		}
	})
}

// FuzzCookie checks that whatever name and value a handler passes, a Set-Cookie it sends parses back to
// that same cookie, so a value from the request cannot inject attributes, and that any Cookie header is
// read without panicking.
func FuzzCookie(f *testing.F) {
	f.Add("session", "abc", "a=1; b=2")
	f.Add("x", "a; Domain=evil.example", "x=\"quoted\"")
	f.Add("bad name", "v\r\nSet-Cookie: y=1", "=; ;=")
	f.Add("n", "ü", "n=ü")
	f.Fuzz(func(t *testing.T, name, value, header string) {
		app := New(Config{Logger: discardLogger})
		var read string
		app.Get("/", func(c *Ctx) error {
			read = c.Cookies(name)
			c.Cookie(&Cookie{Name: name, Value: value, Path: "/"})
			return nil
		})
		req := httptest.NewRequest("GET", "/", nil)
		req.Header["Cookie"] = []string{header}
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		_ = read
		for _, line := range resp.Header.Values("Set-Cookie") {
			ck, err := http.ParseSetCookie(line)
			if err != nil {
				t.Fatalf("sent an unparsable Set-Cookie %q for name %q value %q: %v", line, name, value, err)
			}
			if ck.Name != name || ck.Value != value || ck.Path != "/" || ck.Domain != "" {
				t.Fatalf("Set-Cookie %q parses as %q=%q (Path %q, Domain %q), want %q=%q", line, ck.Name, ck.Value,
					ck.Path, ck.Domain, name, value)
			}
		}
	})
}
