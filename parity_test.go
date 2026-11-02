package qinetic

import (
	"encoding/json"
	"maps"
	"os"
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
		// else, including in Fiber's own "/api/*". qinetic ignores it consistently.
		if c.Pattern == "/*" && !c.Strict && strings.HasSuffix(want["*1"], "/") {
			want = map[string]string{"*1": strings.TrimSuffix(want["*1"], "/")}
		}
		if ok != c.Match || ok && !maps.Equal(got, want) {
			t.Errorf("%+v %s on %s: got match=%v %v, Fiber match=%v %v", o, c.Pattern, c.Path, ok, got, c.Match, want)
		}
	}
}
