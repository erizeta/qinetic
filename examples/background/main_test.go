package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

type receipt struct {
	id, payload string
	canceled    bool
}

// TestBackgroundWork checks the goroutine gets intact copies after its handler returned and the Ctx, with
// its body buffer, served other requests.
func TestBackgroundWork(t *testing.T) {
	got := make(chan receipt, 2)
	release := make(chan struct{})
	sendReceipt = func(ctx context.Context, id string, payload []byte) {
		<-release // run only once both requests are done
		got <- receipt{id, string(payload), ctx.Err() != nil}
	}
	app := newApp()
	for _, r := range []struct{ id, body string }{{"1", "first order"}, {"2", "second, longer order body"}} {
		resp, err := app.Test(httptest.NewRequest("POST", "/orders/"+r.id, strings.NewReader(r.body)))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 202 {
			t.Fatalf("order %s: status %d", r.id, resp.StatusCode)
		}
	}
	close(release)
	want := map[string]string{"1": "first order", "2": "second, longer order body"}
	for range 2 {
		r := <-got
		if r.payload != want[r.id] || r.canceled {
			t.Fatalf("order %s: payload %q, context canceled %v", r.id, r.payload, r.canceled)
		}
	}
}
