package qinetic

import (
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
)

// route is one entry of the route stack. Middleware (use) routes match any method by path prefix;
// method routes match one method and the whole path.
type route struct {
	method   string
	path     string
	use      bool
	pat      *pattern
	handlers []Handler
	info     Route
}

// Route describes a registered route, as returned by c.Route.
type Route struct {
	// Method is the request method, or "USE" for middleware.
	Method string
	// Path is the pattern as registered, including group prefixes.
	Path string
	// Params are the parameter names in order; wildcards are named "*1", "+1", and so on.
	Params []string
}

// router keeps routes in registration order, which is the order Next visits them. The candidate lists
// are built once, on the first lookup, and the stack is read-only after that.
type router struct {
	caseSensitive bool
	strict        bool

	mu     sync.Mutex
	frozen atomic.Bool
	once   sync.Once
	routes []*route

	tables  map[string]*methodTable
	useOnly *methodTable // for methods without routes of their own
	// rootUse is middleware registered on "/", the only routes run for paths that are not routable.
	rootUse []int
}

// methodTable lists, in stack order, the routes that can match a request with one method.
type methodTable struct {
	// buckets is keyed by a static first segment; routes with a static second segment go one level down.
	buckets map[string]*bucket
	// general holds routes whose first segment is not static, so they can match any path.
	general []int
	all     []int
}

type bucket struct {
	cands []int            // routes keyed by this first segment only, plus general
	sub   map[string][]int // by second segment: those routes plus cands
}

// maxBucketKey bounds a decoded segment looked up in buckets; longer ones scan every route.
const maxBucketKey = 64

func newRouter(caseSensitive, strict bool) *router {
	return &router{caseSensitive: caseSensitive, strict: strict}
}

// add registers a route and returns its stack index. It panics on an invalid pattern or once the router
// has started serving; the mutex makes a registration racing the first lookup either land or panic.
func (r *router) add(method, path string, use bool, handlers []Handler) int {
	// A middleware prefix means the same with or without a trailing slash, even under strict routing.
	pat, err := parsePattern(path, r.caseSensitive, r.strict && !use)
	if err != nil {
		panic(fmt.Sprintf("qinetic: invalid route %q: %v", path, err))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen.Load() {
		panic(fmt.Sprintf("qinetic: route %s %q registered after the app started serving", method, path))
	}
	info := Route{Method: method, Path: path, Params: slices.Clone(pat.names)}
	if use {
		info.Method = "USE"
	}
	r.routes = append(r.routes, &route{method: method, path: path, use: use, pat: pat, handlers: handlers, info: info})
	return len(r.routes) - 1
}

func (r *router) freeze() {
	r.once.Do(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.frozen.Store(true)
		r.tables = make(map[string]*methodTable)
		for _, rt := range r.routes {
			if !rt.use && r.tables[rt.method] == nil {
				r.tables[rt.method] = r.buildTable(rt.method)
			}
		}
		r.useOnly = r.buildTable("")
		for i, rt := range r.routes {
			if rt.use && len(rt.pat.segs) == 0 {
				r.rootUse = append(r.rootUse, i)
			}
		}
	})
}

func (r *router) buildTable(method string) *methodTable {
	t := &methodTable{buckets: make(map[string]*bucket)}
	type keyed struct {
		top []int
		sub map[string][]int
	}
	byFirst := make(map[string]*keyed)
	for i, rt := range r.routes {
		if !rt.use && (method == "" || rt.method != method) {
			continue
		}
		t.all = append(t.all, i)
		first, ok := rt.pat.staticText(0)
		if !ok {
			t.general = append(t.general, i)
			continue
		}
		k := byFirst[first]
		if k == nil {
			k = &keyed{sub: make(map[string][]int)}
			byFirst[first] = k
		}
		if second, ok := rt.pat.staticText(1); ok {
			k.sub[second] = append(k.sub[second], i)
		} else {
			k.top = append(k.top, i)
		}
	}
	merge := func(lists ...[]int) []int {
		merged := slices.Concat(lists...)
		slices.Sort(merged)
		return merged
	}
	for first, k := range byFirst {
		b := &bucket{cands: merge(k.top, t.general), sub: make(map[string][]int, len(k.sub))}
		for second, idx := range k.sub {
			b.sub[second] = merge(idx, b.cands)
		}
		t.buckets[first] = b
	}
	return t
}

func (r *router) table(method string) *methodTable {
	if t, ok := r.tables[method]; ok {
		return t
	}
	return r.useOnly
}

// candidates returns the routes that may match path, a clean routing path.
func (t *methodTable) candidates(path []byte, fold bool) []int {
	if len(t.buckets) == 0 || len(path) == 1 {
		return t.general
	}
	var buf [maxBucketKey]byte
	end := segEnd(path, 1)
	n, ok := decodeKey(&buf, path[1:end], fold)
	if !ok {
		return t.all
	}
	b, found := t.buckets[string(buf[:n])]
	if !found {
		return t.general
	}
	if end >= len(path) || len(b.sub) == 0 {
		return b.cands
	}
	second := path[end+1 : segEnd(path, end+1)]
	if n, ok = decodeKey(&buf, second, fold); !ok {
		return t.all
	}
	if idx, found := b.sub[string(buf[:n])]; found {
		return idx
	}
	return b.cands
}

// decodeKey decodes and folds a raw segment into buf, reporting false when it does not fit.
func decodeKey(buf *[maxBucketKey]byte, raw []byte, fold bool) (int, bool) {
	n := 0
	for i := 0; i < len(raw); {
		if n == len(buf) {
			return 0, false
		}
		c, w := decodeAt(raw, i)
		if fold {
			c = lowerASCII(c)
		}
		buf[n] = c
		n++
		i += w
	}
	return n, true
}

// matcher walks the candidate routes of one request in stack order. It is reused across requests.
type matcher struct {
	r      *router
	path   []byte
	cands  []int
	pos    int
	params paramOffsets
}

var rootPath = []byte("/")

// reset prepares m for a request. It returns false when rawPath is not routable: only root middleware
// is then visited, so it can still log or recover the 404.
func (m *matcher) reset(r *router, method string, rawPath []byte) bool {
	r.freeze()
	m.r, m.pos = r, 0
	path, ok := routingPath(rawPath, r.strict)
	if !ok {
		m.path, m.cands = rootPath, r.rootUse
		return false
	}
	m.path = path
	m.cands = r.table(method).candidates(path, !r.caseSensitive)
	return true
}

// clear drops references to the request so a pooled matcher does not retain it.
func (m *matcher) clear() {
	m.path, m.cands = nil, nil
}

// next returns the next matching route and fills m.params for it, or nil when none is left.
func (m *matcher) next() *route {
	for m.pos < len(m.cands) {
		rt := m.r.routes[m.cands[m.pos]]
		m.pos++
		if rt.pat.match(m.path, rt.use, &m.params) {
			return rt
		}
	}
	return nil
}

// allowed returns the sorted methods whose routes match rawPath, for the Allow header of a 405.
// It runs only on the 404/405 path.
func (r *router) allowed(rawPath []byte) []string {
	r.freeze()
	path, ok := routingPath(rawPath, r.strict)
	if !ok {
		return nil
	}
	var params paramOffsets
	var methods []string
	for _, rt := range r.routes {
		if rt.use || slices.Contains(methods, rt.method) {
			continue
		}
		if rt.pat.match(path, false, &params) {
			methods = append(methods, rt.method)
		}
	}
	slices.Sort(methods)
	return methods
}
