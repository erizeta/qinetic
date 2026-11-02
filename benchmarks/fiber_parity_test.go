package benchmarks

import (
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"

	"github.com/gofiber/fiber/v3"
)

var updateParity = flag.Bool("update-parity", false, "rewrite ../testdata/fiber_parity.json from Fiber")

const parityFile = "../testdata/fiber_parity.json"

// parityCase is one routing decision taken by Fiber. The qinetic module replays the file against its
// router, so Fiber never enters qinetic's go.mod.
type parityCase struct {
	Pattern       string            `json:"pattern"`
	Path          string            `json:"path"`
	Use           bool              `json:"use,omitempty"`
	CaseSensitive bool              `json:"caseSensitive,omitempty"`
	Strict        bool              `json:"strict,omitempty"`
	Match         bool              `json:"match"`
	Params        map[string]string `json:"params,omitempty"`
}

// The syntax subset both routers share: whole-segment params and wildcards, clean unescaped paths.
var (
	parityRoutes = []string{
		"/", "/users", "/users/", "/Users/:id", "/users/:id", "/users/:id/books/:book", "/:a/:b",
		"/u/:name?", "/api/*", "/api/+", "/*", "/a/*/c", "/a/*/b/*", "/a/*/c/:id",
	}
	parityUses  = []string{"/", "/api", "/Api", "/users/:id"}
	parityPaths = []string{
		"/", "/users", "/users/", "/Users", "/USERS/AbC", "/users/42", "/users/42/", "/users/42/books/7",
		"/users/42/books", "/u", "/u/", "/u/bob", "/u/bob/x", "/api", "/api/", "/api/x", "/api/x/y", "/api/x/y/",
		"/apix", "/apiv2/x", "/a/x/c", "/a/x/y/c", "/a/c", "/a/x/c/y/c/z", "/a/1/b/2/b", "/a/1/b", "/x/y",
		"/x/y/z", "/x",
	}
	parityOpts = []struct{ caseSensitive, strict bool }{{false, false}, {true, false}, {false, true}, {true, true}}
)

func fiberCases(t *testing.T) []parityCase {
	var cases []parityCase
	run := func(pattern string, use bool) {
		for _, o := range parityOpts {
			app := fiber.New(fiber.Config{CaseSensitive: o.caseSensitive, StrictRouting: o.strict})
			h := func(c fiber.Ctx) error {
				params := map[string]string{}
				if !use {
					for _, name := range c.Route().Params {
						params[name] = c.Params(name)
					}
				}
				return c.JSON(params)
			}
			if use {
				app.Use(pattern, h)
			} else {
				app.Get(pattern, h)
			}
			for _, path := range parityPaths {
				resp, err := app.Test(httptest.NewRequest(http.MethodGet, path, nil))
				if err != nil {
					t.Fatalf("%s %s: %v", pattern, path, err)
				}
				pc := parityCase{Pattern: pattern, Path: path, Use: use, CaseSensitive: o.caseSensitive, Strict: o.strict}
				switch resp.StatusCode {
				case http.StatusOK:
					pc.Match = true
					if err := json.NewDecoder(resp.Body).Decode(&pc.Params); err != nil {
						t.Fatal(err)
					}
				case http.StatusNotFound:
				default:
					t.Fatalf("%s %s: status %d", pattern, path, resp.StatusCode)
				}
				_ = resp.Body.Close()
				if len(pc.Params) == 0 {
					pc.Params = nil // as the golden file decodes it
				}
				cases = append(cases, pc)
			}
		}
	}
	for _, p := range parityRoutes {
		run(p, false)
	}
	for _, p := range parityUses {
		run(p, true)
	}
	return cases
}

// TestFiberParity checks the golden file still describes Fiber; -update-parity rewrites it.
func TestFiberParity(t *testing.T) {
	got := fiberCases(t)
	if *updateParity {
		b, err := json.MarshalIndent(got, "", "\t")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityFile, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	b, err := os.ReadFile(parityFile)
	if err != nil {
		t.Fatalf("%v (run with -update-parity)", err)
	}
	var want []parityCase
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Fiber's routing differs from %s; rerun with -update-parity and review the diff", parityFile)
	}
}
