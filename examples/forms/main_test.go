package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

var tokenField = regexp.MustCompile(`name="_csrf" value="([^"]+)"`)

func TestForm(t *testing.T) {
	app := newApp()
	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	m := tokenField.FindSubmatch(page)
	if resp.StatusCode != 200 || m == nil || len(resp.Cookies()) != 1 || resp.Cookies()[0].Value != string(m[1]) {
		t.Fatalf("form page: %d %s, cookies %v", resp.StatusCode, page, resp.Cookies())
	}
	cookie, token := resp.Cookies()[0], string(m[1])

	post := func(form string, hdr ...string) (int, string) {
		req := httptest.NewRequest("POST", "/comments", strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := post("_csrf="+token+"&body=hi", "Sec-Fetch-Site", "same-origin"); code != 200 || body != "saved: hi" {
		t.Fatalf("valid post: %d %q", code, body)
	}
	if code, _ := post("body=hi"); code != 403 {
		t.Fatalf("post without token: %d", code)
	}
	if code, _ := post("_csrf="+token+"&body=hi", "Sec-Fetch-Site", "cross-site"); code != 403 {
		t.Fatalf("cross-site post: %d", code)
	}
}
