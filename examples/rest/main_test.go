package main

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestREST(t *testing.T) {
	app := newApp()
	steps := []struct {
		method, target, body string
		code                 int
		want                 string
	}{
		{"POST", "/api/users", `{"name":"Ann"}`, 201, `{"id":1,"name":"Ann"}`},
		{"GET", "/api/users/1", "", 200, `{"id":1,"name":"Ann"}`},
		{"POST", "/api/users", `{"name":""}`, 422, "name is required"},
		{"POST", "/api/users", `{"name":`, 400, "invalid JSON body"},
		{"GET", "/api/users/x", "", 400, "Bad Request"},
		{"DELETE", "/api/users/1", "", 204, ""},
		{"GET", "/api/users/1", "", 404, "Not Found"},
		{"PUT", "/api/users/1", "", 405, "Method Not Allowed"},
	}
	for _, s := range steps {
		req := httptest.NewRequest(s.method, s.target, strings.NewReader(s.body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != s.code || string(body) != s.want {
			t.Fatalf("%s %s: got %d %q, want %d %q", s.method, s.target, resp.StatusCode, body, s.code, s.want)
		}
	}
}
