package qinetic

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
)

// fiberCase is a routing decision recorded from Fiber by benchmarks.TestFiberParity.
type fiberCase struct {
	Pattern       string            `json:"pattern"`
	Path          string            `json:"path"`
	Use           bool              `json:"use"`
	CaseSensitive bool              `json:"caseSensitive"`
	Strict        bool              `json:"strict"`
	Match         bool              `json:"match"`
	Params        map[string]string `json:"params"`
}

// TestFiberParity replays Fiber's decisions on the syntax both routers share.
func TestFiberParity(t *testing.T) {
	b, err := os.ReadFile("testdata/fiber_parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []fiberCase
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		o := routeOpts{caseSensitive: c.CaseSensitive, strict: c.Strict}
		got, ok := matchPattern(t, c.Pattern, c.Path, c.Use, o)
		if c.Use {
			got = map[string]string{}
		}
		want := c.Params
		if want == nil {
			want = map[string]string{}
		}
		// Fiber's fast path for a root "/*" keeps the slash that non-strict routing ignores everywhere
		// else, including in Fiber's own "/api/*". Qinetic ignores it consistently.
		if c.Pattern == "/*" && !c.Strict && strings.HasSuffix(want["*1"], "/") {
			want = map[string]string{"*1": strings.TrimSuffix(want["*1"], "/")}
		}
		if ok != c.Match || ok && !maps.Equal(got, want) {
			t.Errorf("%+v %s on %s: got match=%v %v, Fiber match=%v %v", o, c.Pattern, c.Path, ok, got, c.Match, want)
		}
	}
}

type orderOp struct {
	Op    string `json:"op"`
	Group string `json:"group"`
	Path  string `json:"path"`
	Name  string `json:"name"`
	Label string `json:"label"`
	Next  bool   `json:"next"`
}

type orderRequest struct {
	Method string   `json:"method"`
	Path   string   `json:"path"`
	Trace  []string `json:"trace"`
	Status int      `json:"status"`
	Allow  string   `json:"allow"`
}

// TestFiberOrder rebuilds the app recorded by benchmarks.TestFiberOrder and checks Qinetic runs the same
// handlers, in the same order, with the same status and Allow header.
func TestFiberOrder(t *testing.T) {
	b, err := os.ReadFile("testdata/fiber_order.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Ops      []orderOp      `json:"ops"`
		Requests []orderRequest `json:"requests"`
	}
	if err := json.Unmarshal(b, &golden); err != nil {
		t.Fatal(err)
	}
	app := New()
	var trace []string
	groups := map[string]Router{"": app}
	for _, op := range golden.Ops {
		h := func(c *Ctx) error {
			trace = append(trace, op.Label)
			if op.Op == "use" || op.Op == "group" || op.Next {
				return c.Next()
			}
			return c.SendString("ok")
		}
		r := groups[op.Group]
		switch op.Op {
		case "use":
			if op.Path == "" {
				r.Use(h)
			} else {
				r.Use(op.Path, h)
			}
		case "group":
			groups[op.Name] = r.Group(op.Path, h)
		case "all":
			r.All(op.Path, h)
		default:
			r.Add([]string{op.Op}, op.Path, h)
			if op.Op == "get" {
				r.Add([]string{http.MethodHead}, op.Path, h)
			}
		}
	}
	for _, want := range golden.Requests {
		// Fiber answers methods outside its list with 501 before routing; Qinetic routes any method.
		if want.Method == "PROPFIND" {
			want = orderRequest{Method: want.Method, Path: want.Path, Trace: []string{"root"}, Status: 405,
				Allow: "CONNECT, DELETE, GET, HEAD, OPTIONS, PATCH, POST, PUT, TRACE"}
		}
		trace = nil
		resp, err := app.Test(httptest.NewRequest(want.Method, want.Path, nil))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if !slices.Equal(trace, want.Trace) || resp.StatusCode != want.Status || resp.Header.Get("Allow") != want.Allow {
			t.Errorf("%s %s: trace %v, status %d, Allow %q; Fiber: %v, %d, %q", want.Method, want.Path,
				trace, resp.StatusCode, resp.Header.Get("Allow"), want.Trace, want.Status, want.Allow)
		}
	}
}
