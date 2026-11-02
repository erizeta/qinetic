package qinetic

import (
	"bytes"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"
)

type bindTarget struct {
	Name    string   `json:"name" form:"name" query:"name" xml:"name"`
	Age     int      `json:"age" form:"age" query:"age" xml:"age"`
	Score   float64  `form:"score" query:"score"`
	Admin   bool     `form:"admin" query:"admin"`
	Tags    []string `form:"tag" query:"tag"`
	IDs     []uint16 `form:"id" query:"id"`
	Skipped string   `form:"-" query:"-"`
	Plain   string
}

func bindApp(bind func(b *Bind, v *bindTarget) error) (*App, *bindTarget) {
	var got bindTarget
	app := New()
	app.All("/", func(c *Ctx) error {
		got = bindTarget{}
		if err := bind(c.Bind(), &got); err != nil {
			return err
		}
		return c.SendString("ok")
	})
	return app, &got
}

func TestBindQuery(t *testing.T) {
	app, got := bindApp(func(b *Bind, v *bindTarget) error { return b.Query(v) })
	resp, body := get(t, app, "GET", "/?name=Ann&age=30&score=1.5&admin=on&tag=a&tag=b&id=1&id=2&Skipped=x&Plain=p")
	want := bindTarget{Name: "Ann", Age: 30, Score: 1.5, Admin: true, Tags: []string{"a", "b"}, IDs: []uint16{1, 2}, Plain: "p"}
	if resp.StatusCode != 200 || fmt.Sprint(*got) != fmt.Sprint(want) {
		t.Fatalf("got %d %q %+v", resp.StatusCode, body, *got)
	}
	for _, q := range []string{"age=x", "admin=maybe", "id=70000", "id=1&id=-1", "score=abc"} {
		resp, body := get(t, app, "GET", "/?"+q)
		if resp.StatusCode != 400 || !strings.HasPrefix(body, "invalid value for") {
			t.Errorf("%s: got %d %q", q, resp.StatusCode, body)
		}
	}
}

func TestBindForm(t *testing.T) {
	app, got := bindApp(func(b *Bind, v *bindTarget) error { return b.Form(v) })
	req := httptest.NewRequest("POST", "/", strings.NewReader("name=Bo&age=4&tag=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if resp, body := do(t, app, req); resp.StatusCode != 200 || got.Name != "Bo" || got.Age != 4 || len(got.Tags) != 1 {
		t.Fatalf("urlencoded: %d %q %+v", resp.StatusCode, body, *got)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("name", "Cy")
	_ = mw.WriteField("tag", "p")
	_ = mw.WriteField("tag", "q")
	_ = mw.Close()
	req = httptest.NewRequest("POST", "/", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if resp, body := do(t, app, req); resp.StatusCode != 200 || got.Name != "Cy" || fmt.Sprint(got.Tags) != "[p q]" {
		t.Fatalf("multipart: %d %q %+v", resp.StatusCode, body, *got)
	}
}

func TestBindBody(t *testing.T) {
	app, got := bindApp(func(b *Bind, v *bindTarget) error { return b.Body(v) })
	tests := []struct {
		ctype, body string
		code        int
		name        string
	}{
		{"application/json", `{"name":"J","age":1}`, 200, "J"},
		{"application/problem+json; charset=utf-8", `{"name":"P"}`, 200, "P"},
		{"application/xml", `<bindTarget><name>X</name></bindTarget>`, 200, "X"},
		{"application/x-www-form-urlencoded", "name=F", 200, "F"},
		{"application/json", `{"name":`, 400, ""},
		{"application/json", `{"age":"not a number"}`, 400, ""},
		{"application/xml", `<bindTarget><name>`, 400, ""},
		{"text/plain", "name=F", 415, ""},
		{"", "name=F", 415, ""},
	}
	for _, tt := range tests {
		t.Run(tt.ctype+" "+tt.body, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", tt.ctype)
			resp, body := do(t, app, req)
			if resp.StatusCode != tt.code || tt.code == 200 && got.Name != tt.name {
				t.Fatalf("got %d %q, name %q", resp.StatusCode, body, got.Name)
			}
			// Decoder details, such as the offending JSON offset, stay out of the response.
			if tt.code == 400 && body != "invalid JSON body" && body != "invalid XML body" {
				t.Fatalf("400 body %q leaks decoder detail", body)
			}
		})
	}
}

func TestBindErrorKeepsCause(t *testing.T) {
	var bindErr error
	app := New()
	app.Post("/", func(c *Ctx) error {
		var v struct{ A int }
		bindErr = c.Bind().JSON(&v)
		return nil
	})
	req := httptest.NewRequest("POST", "/", strings.NewReader("{"))
	do(t, app, req)
	var e *Error
	if !errors.As(bindErr, &e) || e.Code != 400 || errors.Unwrap(e) == nil {
		t.Fatalf("Bind error %v, want a 400 *Error wrapping the decoder error", bindErr)
	}
}

func TestBindBodyLimit(t *testing.T) {
	app := New(Config{BodyLimit: 4})
	app.Post("/", func(c *Ctx) error {
		var v struct{ A int }
		return c.Bind().JSON(&v)
	})
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"A":1}`))
	if resp, _ := do(t, app, req); resp.StatusCode != 413 {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

func TestBindProgrammingErrors(t *testing.T) {
	for name, v := range map[string]any{
		"nil":         nil,
		"non-pointer": bindTarget{},
		"nil pointer": (*bindTarget)(nil),
		"not struct":  new(int),
		"bad field":   &struct{ M map[string]string }{},
	} {
		t.Run(name, func(t *testing.T) {
			logger, _ := testLogger()
			app := New(Config{Logger: logger})
			app.Get("/", func(c *Ctx) error { return c.Bind().Query(v) })
			if resp, body := get(t, app, "GET", "/?M=x"); resp.StatusCode != 500 || body != "Internal Server Error" {
				t.Fatalf("got %d %q", resp.StatusCode, body)
			}
		})
	}
}

func TestBindMatchesKeysCaseInsensitively(t *testing.T) {
	app, got := bindApp(func(b *Bind, v *bindTarget) error { return b.Query(v) })
	get(t, app, "GET", "/?NAME=Ann&Age=3&plain=p&TAG=a&tag=b")
	if got.Name != "Ann" || got.Age != 3 || got.Plain != "p" || fmt.Sprint(got.Tags) != "[a b]" {
		t.Fatalf("got %+v", *got)
	}

	app, got = bindApp(func(b *Bind, v *bindTarget) error { return b.Form(v) })
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("Name", "Mu")
	_ = mw.WriteField("PLAIN", "q")
	_ = mw.Close()
	req := httptest.NewRequest("POST", "/", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	do(t, app, req)
	if got.Name != "Mu" || got.Plain != "q" {
		t.Fatalf("multipart: got %+v", *got)
	}
}
