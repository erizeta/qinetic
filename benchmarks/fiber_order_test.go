package benchmarks

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
)

const orderFile = "../testdata/fiber_order.json"

// orderOp registers one handler. Group is the Name of an earlier group op, or "" for the app.
// Middleware and groups, and routes with Next, call c.Next; other routes end the chain.
type orderOp struct {
	Op    string `json:"op"`
	Group string `json:"group,omitempty"`
	Path  string `json:"path,omitempty"`
	Name  string `json:"name,omitempty"`
	Label string `json:"label"`
	Next  bool   `json:"next,omitempty"`
}

type orderRequest struct {
	Method string   `json:"method"`
	Path   string   `json:"path"`
	Trace  []string `json:"trace"`
	Status int      `json:"status"`
	Allow  string   `json:"allow,omitempty"`
}

type orderGolden struct {
	Ops      []orderOp      `json:"ops"`
	Requests []orderRequest `json:"requests"`
}

var orderOps = []orderOp{
	{Op: "use", Label: "root"},
	{Op: "use", Path: "/api", Label: "api"},
	{Op: "get", Path: "/api/users", Label: "users", Next: true},
	{Op: "group", Path: "/api/v1", Name: "v1", Label: "v1"},
	{Op: "get", Group: "v1", Path: "/users/:id", Label: "v1-user"},
	{Op: "group", Group: "v1", Path: "/admin", Name: "admin", Label: "admin"},
	{Op: "post", Group: "admin", Path: "/", Label: "admin-post"},
	{Op: "use", Group: "v1", Path: "/users", Label: "v1-users-mw"},
	{Op: "get", Path: "/api/*", Label: "api-wild", Next: true},
	{Op: "get", Path: "/:page", Label: "page"},
	{Op: "all", Path: "/any", Label: "any"},
	{Op: "use", Path: "/apiv2", Label: "apiv2"},
	{Op: "get", Path: "/api/users", Label: "users-2"},
	{Op: "put", Path: "/api/users/:id", Label: "put-user"},
}

var orderPaths = []struct{ method, path string }{
	{"GET", "/api/users"}, {"HEAD", "/api/users"}, {"GET", "/api/v1/users/7"}, {"POST", "/api/v1/admin"},
	{"GET", "/api/v1/admin"}, {"DELETE", "/api/users"}, {"DELETE", "/api/users/7"}, {"GET", "/apiv2/x"},
	{"GET", "/about"}, {"PUT", "/any"}, {"PROPFIND", "/any"}, {"GET", "/"}, {"GET", "/api"}, {"GET", "/API/USERS"},
	{"POST", "/api/v1/users/7"},
}

func fiberOrder(t *testing.T) []orderRequest {
	app := fiber.New()
	var trace []string
	groups := map[string]fiber.Router{"": app}
	for _, op := range orderOps {
		h := func(c fiber.Ctx) error {
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
			r.Add([]string{strings.ToUpper(op.Op)}, op.Path, h)
			if op.Op == "get" {
				r.Add([]string{http.MethodHead}, op.Path, h)
			}
		}
	}
	var out []orderRequest
	for _, p := range orderPaths {
		trace = nil
		resp, err := app.Test(httptest.NewRequest(p.method, p.path, nil))
		if err != nil {
			t.Fatalf("%s %s: %v", p.method, p.path, err)
		}
		_ = resp.Body.Close()
		out = append(out, orderRequest{
			Method: p.method, Path: p.path, Trace: trace, Status: resp.StatusCode, Allow: resp.Header.Get("Allow"),
		})
	}
	return out
}

// TestFiberOrder checks the golden handler order still describes Fiber; -update-parity rewrites it.
func TestFiberOrder(t *testing.T) {
	got := orderGolden{Ops: orderOps, Requests: fiberOrder(t)}
	if *updateParity {
		b, err := json.MarshalIndent(got, "", "\t")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(orderFile, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	b, err := os.ReadFile(orderFile)
	if err != nil {
		t.Fatalf("%v (run with -update-parity)", err)
	}
	var want orderGolden
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Fiber's handler order differs from %s; rerun with -update-parity and review the diff", orderFile)
	}
}
