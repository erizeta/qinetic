package qinetic

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
)

type testRoute struct {
	method, path string
	use          bool
}

func buildRouter(o routeOpts, routes []testRoute) *router {
	r := newRouter(o.caseSensitive, o.strict)
	for _, rt := range routes {
		r.add(rt.method, rt.path, rt.use)
	}
	return r
}

// walk returns the stack indexes Next would visit for a request.
func walk(r *router, method, path string) []int {
	var m matcher
	if !m.reset(r, []byte(method), []byte(path)) {
		return nil
	}
	var got []int
	for rt := m.next(); rt != nil; rt = m.next() {
		got = append(got, slices.Index(r.routes, rt))
	}
	return got
}

// bruteForce matches every route in order, without buckets.
func bruteForce(r *router, method, rawPath string) []int {
	path, ok := routingPath([]byte(rawPath), r.strict)
	if !ok {
		return nil
	}
	var params paramOffsets
	var got []int
	for i, rt := range r.routes {
		if (rt.use || rt.method == method) && rt.pat.match(path, rt.use, &params) {
			got = append(got, i)
		}
	}
	return got
}

var stackRoutes = []testRoute{
	{"", "/", true},                  // 0
	{"", "/api", true},               // 1
	{"GET", "/api/users", false},     // 2
	{"GET", "/api/users/:id", false}, // 3
	{"", "/api/users", true},         // 4
	{"POST", "/api/users", false},    // 5
	{"GET", "/:page", false},         // 6
	{"GET", "/static/*", false},      // 7
	{"GET", "/api/*", false},         // 8
	{"", "/apiv2", true},             // 9
	{"PUT", "/api/users/:id", false}, // 10
	{"GET", "/", false},              // 11
	{"PATCH", "/API/Users", false},   // 12
}

func TestRouterStackOrder(t *testing.T) {
	r := buildRouter(routeOpts{}, stackRoutes)
	tests := []struct {
		method, path string
		want         []int
	}{
		{"GET", "/api/users", []int{0, 1, 2, 4, 8}},
		{"GET", "/api/users/7", []int{0, 1, 3, 4, 8}},
		{"POST", "/api/users", []int{0, 1, 4, 5}},
		{"GET", "/api", []int{0, 1, 6, 8}},
		{"GET", "/apiv2/x", []int{0, 9}},
		{"GET", "/apiv2", []int{0, 6, 9}},
		{"GET", "/about", []int{0, 6}},
		{"GET", "/", []int{0, 11}},
		{"GET", "/static/a/b.css", []int{0, 7}},
		{"DELETE", "/api/users/7", []int{0, 1, 4}},
		{"PROPFIND", "/api/users", []int{0, 1, 4}},
		{"PATCH", "/api/users", []int{0, 1, 4, 12}},
		{"GET", "/a//b", nil},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			if got := walk(r, tt.method, tt.path); !slices.Equal(got, tt.want) {
				t.Fatalf("visited %v, want %v", got, tt.want)
			}
		})
	}
}

// TestRouterBucketsMatchBruteForce checks the candidate lists never drop or reorder a matching route.
func TestRouterBucketsMatchBruteForce(t *testing.T) {
	routes := append(slices.Clone(stackRoutes),
		testRoute{"GET", "/caf%C3%A9/:x", false},
		testRoute{"GET", "/" + strings.Repeat("l", maxBucketKey+5), false},
		testRoute{"", "/:tenant/admin", true},
		testRoute{"GET", "/a%2Fb/*", false},
	)
	paths := []string{
		"/", "/api", "/API", "/api/", "/api/users", "/Api/Users/1", "/apiv2", "/about", "/static/x",
		"/caf%C3%A9/1", "/caf%c3%a9/1", "/CAF%C3%A9/1", "/" + strings.Repeat("l", maxBucketKey+5),
		"/" + strings.Repeat("L", maxBucketKey+5), "/acme/admin/x", "/a%2Fb/c", "/a%2fb", "/%61pi/users",
	}
	routes = append(routes, benchRoutes()...)
	paths = append(paths, "/v1", "/v1/", "/v1/area3", "/V1/AREA3/items/9", "/v1/area3/files/a/b", "/v1/x/items",
		"/v1/%61rea3/items", "/static3/app.js", "/v1/"+strings.Repeat("a", maxBucketKey+1))
	for _, o := range []routeOpts{{}, {caseSensitive: true}, {strict: true}, {true, true}} {
		r := buildRouter(o, routes)
		for _, method := range []string{"GET", "POST", "PATCH", "OPTIONS"} {
			for _, path := range paths {
				got, want := walk(r, method, path), bruteForce(r, method, path)
				if !slices.Equal(got, want) {
					t.Errorf("%+v %s %s: visited %v, want %v", o, method, path, got, want)
				}
			}
		}
	}
}

func TestRouterAllowed(t *testing.T) {
	r := buildRouter(routeOpts{}, stackRoutes)
	tests := []struct{ path, want string }{
		{"/api/users", "GET, PATCH, POST"},
		{"/api/users/7", "GET, PUT"},
		{"/apiv2/x", ""},
		{"/a//b", ""},
	}
	for _, tt := range tests {
		if got := r.allowed([]byte(tt.path)); got != tt.want {
			t.Errorf("allowed(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestRouterInvalidPatternPanics(t *testing.T) {
	r := newRouter(false, false)
	defer func() {
		msg := fmt.Sprint(recover())
		if !strings.Contains(msg, `"/a/:id/:id"`) || !strings.Contains(msg, "duplicate") {
			t.Fatalf("panic = %q, want the pattern and the reason", msg)
		}
	}()
	r.add("GET", "/a/:id/:id", false)
}

func TestRouterAddAfterFreezePanics(t *testing.T) {
	r := newRouter(false, false)
	r.add("GET", "/", false)
	var m matcher
	m.reset(r, []byte("GET"), []byte("/"))
	defer func() {
		if msg := fmt.Sprint(recover()); !strings.Contains(msg, "after the app started serving") {
			t.Fatalf("panic = %q", msg)
		}
	}()
	r.add("GET", "/late", false)
}

// TestRouterAddRacesFreeze runs registration against the first lookup; under -race it must be clean,
// and every route either lands before the freeze or panics.
func TestRouterAddRacesFreeze(t *testing.T) {
	for range 50 {
		r := newRouter(false, false)
		var wg sync.WaitGroup
		var mu sync.Mutex
		var landed []string
		for i := range 8 {
			wg.Go(func() {
				path := fmt.Sprintf("/r%d", i)
				defer func() {
					if recover() == nil {
						mu.Lock()
						landed = append(landed, path)
						mu.Unlock()
					}
				}()
				r.add("GET", path, false)
			})
		}
		var m matcher
		m.reset(r, []byte("GET"), []byte("/r0"))
		_ = m.next()
		wg.Wait()
		if len(r.routes) != len(landed) {
			t.Fatalf("%d routes in the stack, %d registrations returned", len(r.routes), len(landed))
		}
		for _, path := range landed {
			if walk(r, "GET", path) == nil {
				t.Fatalf("route %s landed but does not match", path)
			}
		}
	}
}

func TestRouterMatchAllocs(t *testing.T) {
	r := buildRouter(routeOpts{}, benchRoutes())
	for _, tt := range benchLookups {
		var m matcher
		method, path := []byte(tt.method), []byte(tt.path)
		allocs := testing.AllocsPerRun(100, func() {
			m.reset(r, method, path)
			if m.next() == nil {
				t.Fatalf("%s %s: no match", tt.method, tt.path)
			}
		})
		if allocs != 0 {
			t.Errorf("%s: %v allocs/op, want 0", tt.name, allocs)
		}
	}
}

// benchRoutes is a GitHub-like API of 100+ routes behind 10 middleware prefixes.
func benchRoutes() []testRoute {
	var routes []testRoute
	for i := range 10 {
		routes = append(routes, testRoute{"", fmt.Sprintf("/v1/area%d", i), true})
	}
	for i := range 10 {
		base := fmt.Sprintf("/v1/area%d", i)
		routes = append(routes,
			testRoute{"GET", base + "/items", false},
			testRoute{"POST", base + "/items", false},
			testRoute{"GET", base + "/items/:id", false},
			testRoute{"PUT", base + "/items/:id", false},
			testRoute{"DELETE", base + "/items/:id", false},
			testRoute{"GET", base + "/items/:id/comments", false},
			testRoute{"GET", base + "/items/:id/comments/:cid", false},
			testRoute{"GET", base + "/files/*", false},
			testRoute{"GET", fmt.Sprintf("/static%d/app.js", i), false},
			testRoute{"GET", fmt.Sprintf("/static%d/app.css", i), false},
		)
	}
	return append(routes,
		testRoute{"GET", "/hello", false},
		testRoute{"GET", "/users/:id", false},
		testRoute{"GET", "/users/:id/books/:book", false},
		testRoute{"GET", "/assets/*", false},
	)
}

var benchLookups = []struct{ name, method, path string }{
	{"static", "GET", "/hello"},
	{"param1", "GET", "/users/42"},
	{"param2", "GET", "/users/42/books/7"},
	{"wildcard", "GET", "/assets/css/site/main.css"},
	{"stack", "GET", "/v1/area9/items/42/comments/7"},
}

func BenchmarkRouterMatch(b *testing.B) {
	r := buildRouter(routeOpts{}, benchRoutes())
	for _, tt := range benchLookups {
		b.Run(tt.name, func(b *testing.B) {
			method, path := []byte(tt.method), []byte(tt.path)
			var m matcher
			visited := 0
			b.ReportAllocs()
			for b.Loop() {
				m.reset(r, method, path)
				// Walk the whole stack, as a request whose middleware all call Next would.
				for m.next() != nil {
					visited++
				}
			}
			if visited == 0 {
				b.Fatal("no route matched")
			}
		})
	}
}
