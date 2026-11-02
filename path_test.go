package qinetic

import (
	"maps"
	"strings"
	"testing"
)

type routeOpts struct{ caseSensitive, strict bool }

// matchPattern compiles pattern and matches it against rawPath the way the router does.
func matchPattern(t testing.TB, pattern, rawPath string, use bool, o routeOpts) (map[string]string, bool) {
	t.Helper()
	pat, err := parsePattern(pattern, o.caseSensitive, o.strict)
	if err != nil {
		t.Fatalf("parsePattern(%q): %v", pattern, err)
	}
	path, ok := routingPath([]byte(rawPath), o.strict)
	if !ok {
		return nil, false
	}
	var params paramOffsets
	if !pat.match(path, use, &params) {
		return nil, false
	}
	got := make(map[string]string, len(pat.names))
	for i, name := range pat.names {
		off := params[i]
		got[name] = rawPath[off[0]:off[1]]
	}
	return got, true
}

func TestParsePatternInvalid(t *testing.T) {
	tests := []struct{ pattern, want string }{
		{"/a//b", "empty segment"},
		{"/:", "without a name"},
		{"/:?", "without a name"},
		{"/:id/:id", "duplicate"},
		{"/:a?/b", "must be the last"},
		{"/*/:a?", "cannot follow a wildcard"},
		{"/:from-:to", "one per segment"},
		{"/:file.ext", "one per segment"},
		{"/api*", "whole segment"},
		{"/v1/users:batch", "whole segment"},
		{"/a?", "whole segment"},
		{"/c++", "whole segment"},
		{"/a/%zz", "percent-encoding"},
		{"/a/%2", "percent-encoding"},
		{"/a/../b", "dot segment"},
		{"/a/%2e%2E", "dot segment"},
		{"/" + manyParams(maxParams+1), "more than"},
	}
	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			_, err := parsePattern(tt.pattern, false, false)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func manyParams(n int) string {
	var b strings.Builder
	for i := range n {
		if i > 0 {
			b.WriteByte('/')
		}
		b.WriteString(":p")
		b.WriteString(strings.Repeat("x", i))
	}
	return b.String()
}

func TestParsePatternValid(t *testing.T) {
	tests := []struct {
		pattern string
		names   []string
	}{
		{"", nil},
		{"/", nil},
		{"users", nil},
		{"/users/", nil},
		{"/users/:id", []string{"id"}},
		{"/:a/:b_2/:C3", []string{"a", "b_2", "C3"}},
		{"/u/:name?", []string{"name"}},
		{"/*", []string{"*1"}},
		{"/a/*/b/*", []string{"*1", "*2"}},
		{"/a/+/*/+", []string{"+1", "*1", "+2"}},
		{"/v1/users%3Abatch", nil},
		{"/" + manyParams(maxParams), nil},
	}
	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			pat, err := parsePattern(tt.pattern, false, false)
			if err != nil {
				t.Fatal(err)
			}
			if tt.names != nil && !equalStrings(pat.names, tt.names) {
				t.Fatalf("names = %q, want %q", pat.names, tt.names)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPatternMatch(t *testing.T) {
	type p = map[string]string
	def, strict, cs := routeOpts{}, routeOpts{strict: true}, routeOpts{caseSensitive: true}
	tests := []struct {
		pattern, path string
		use           bool
		opts          routeOpts
		want          p // nil means no match
	}{
		// static, root
		{"/", "/", false, def, p{}},
		{"/", "/a", false, def, nil},
		{"/users", "/users", false, def, p{}},
		{"/users", "/user", false, def, nil},
		{"/users", "/users/x", false, def, nil},
		{"/a/b/c", "/a/b/c", false, def, p{}},
		{"/v1/users%3Abatch", "/v1/users:batch", false, def, p{}},
		{"/v1/users%3Abatch", "/v1/users%3abatch", false, def, p{}},
		{"/users", "/us%65rs", false, def, p{}},

		// parameters
		{"/users/:id", "/users/42", false, def, p{"id": "42"}},
		{"/users/:id", "/users", false, def, nil},
		{"/users/:id", "/users/42/x", false, def, nil},
		{"/:a/:b", "/x/y", false, def, p{"a": "x", "b": "y"}},
		{"/users/:id/books/:book", "/users/1/books/2", false, def, p{"id": "1", "book": "2"}},
		{"/users/:id", "/users/J%C3%BCrgen", false, def, p{"id": "J%C3%BCrgen"}},

		// %2F stays inside one segment
		{"/files/:name", "/files/a%2Fb", false, def, p{"name": "a%2Fb"}},
		{"/files/:name", "/files/a/b", false, def, nil},
		{"/files/a%2Fb", "/files/a%2fb", false, def, p{}},
		{"/files/a%2Fb", "/files/a/b", false, def, nil},

		// optional
		{"/u/:name?", "/u", false, def, p{"name": ""}},
		{"/u/:name?", "/u/", false, def, p{"name": ""}},
		{"/u/:name?", "/u/", false, strict, p{"name": ""}},
		{"/u/:name?", "/u/bob", false, def, p{"name": "bob"}},
		{"/u/:name?", "/u/bob/x", false, def, nil},
		{"/:name?", "/", false, def, p{"name": ""}},

		// trailing wildcards
		{"/api/*", "/api", false, def, p{"*1": ""}},
		{"/api/*", "/api/", false, def, p{"*1": ""}},
		{"/api/*", "/api/", false, strict, p{"*1": ""}},
		{"/api/*", "/api/x/y", false, def, p{"*1": "x/y"}},
		{"/api/*", "/api/x/y/", false, strict, p{"*1": "x/y/"}},
		{"/api/*", "/apix", false, def, nil},
		{"/*", "/", false, def, p{"*1": ""}},
		{"/*", "/a/b%2Fc", false, def, p{"*1": "a/b%2Fc"}},
		{"/api/+", "/api", false, def, nil},
		{"/api/+", "/api/", false, def, nil},
		{"/api/+", "/api/", false, strict, nil},
		{"/api/+", "/api/x", false, def, p{"+1": "x"}},
		{"/api/+", "/api/x/y", false, def, p{"+1": "x/y"}},

		// inner wildcards are greedy and need at least one segment
		{"/a/*/c", "/a/x/c", false, def, p{"*1": "x"}},
		{"/a/*/c", "/a/x/y/c", false, def, p{"*1": "x/y"}},
		{"/a/*/c", "/a/c", false, def, nil},
		{"/a/*/c", "/a/x/y", false, def, nil},
		{"/a/*/c/:id", "/a/x/c/y/c/z", false, def, p{"*1": "x/c/y", "id": "z"}},
		{"/a/*/b/*/c", "/a/1/b/2/b/3/c", false, def, p{"*1": "1/b/2", "*2": "3"}},
		{"/a/*/b/*", "/a/1/b/2/b", false, def, p{"*1": "1/b/2", "*2": ""}},
		{"/a/*/b/*", "/a/1/b", false, def, p{"*1": "1", "*2": ""}},
		{"/a/*/b/+", "/a/1/b/2/b", false, def, p{"*1": "1", "+1": "2/b"}},
		{"/a/*/b/+", "/a/1/b", false, def, nil},
		{"/a/+/b", "/a/1/2/b", false, def, p{"+1": "1/2"}},
		{"/*/x", "/x", false, def, nil},
		{"/*/x", "/a/x", false, def, p{"*1": "a"}},

		// trailing slash
		{"/users", "/users/", false, def, p{}},
		{"/users/", "/users", false, def, p{}},
		{"/users", "/users/", false, strict, nil},
		{"/users/", "/users", false, strict, nil},
		{"/users/", "/users/", false, strict, p{}},
		{"/users/:id", "/users/1/", false, def, p{"id": "1"}},
		{"/users/:id", "/users/1/", false, strict, nil},

		// case
		{"/Users", "/users", false, def, p{}},
		{"/users", "/USERS", false, def, p{}},
		{"/users", "/%55sers", false, def, p{}},
		{"/users/:id", "/USERS/AbC", false, def, p{"id": "AbC"}},
		{"/Users", "/users", false, cs, nil},
		{"/Users", "/Users", false, cs, p{}},

		// middleware prefixes stop at segment boundaries
		{"/api", "/api", true, def, p{}},
		{"/api", "/api/", true, def, p{}},
		{"/api", "/api/x/y", true, def, p{}},
		{"/api", "/apiv2", true, def, nil},
		{"/api", "/ap", true, def, nil},
		{"/", "/anything/at/all", true, def, p{}},
		{"/users/:id", "/users/7/books", true, def, p{"id": "7"}},
		{"/users/:id", "/users", true, def, nil},
		{"/a/*/c", "/a/x/c/y/c/z", true, def, p{"*1": "x/c/y"}},
		{"/a/*/c", "/a/x/y", true, def, nil},

		// unclean paths are never routed
		{"/a/*", "/a//b", false, def, nil},
		{"/a/*", "/a/./b", false, def, nil},
		{"/*", "/a/../b", false, def, nil},
		{"/*", "/a/%2e%2e/b", false, def, nil},
		{"/*", "/a/.%2E", false, def, nil},
		{"/*", "/a/%2e", false, def, nil},
		{"/*", "//", false, def, nil},
		{"/*", "/a//", false, strict, nil},
		{"/*", "", false, def, nil},
		{"/*", "*", false, def, nil},
		{"/*", "/a/...", false, def, p{"*1": "a/..."}},
	}
	for _, tt := range tests {
		name := tt.pattern + " " + tt.path
		if tt.use {
			name = "use " + name
		}
		t.Run(name, func(t *testing.T) {
			got, ok := matchPattern(t, tt.pattern, tt.path, tt.use, tt.opts)
			if tt.want == nil {
				if ok {
					t.Fatalf("matched with %v, want no match", got)
				}
				return
			}
			if !ok {
				t.Fatalf("no match, want %v", tt.want)
			}
			if !maps.Equal(got, tt.want) {
				t.Fatalf("params = %v, want %v", got, tt.want)
			}
		})
	}
}

func FuzzMatch(f *testing.F) {
	f.Add("/users/:id", "/users/42", false, false, false)
	f.Add("/a/*/b/+", "/a/1/b/2/b", false, false, false)
	f.Add("/a/*/c/:x?", "/a/b/c", true, true, true)
	f.Add("/api", "/API/x%2F", true, false, true)
	f.Fuzz(func(t *testing.T, pattern, path string, use, caseSensitive, strict bool) {
		pat, err := parsePattern(pattern, caseSensitive, strict)
		if err != nil {
			return
		}
		routed, ok := routingPath([]byte(path), strict)
		if !ok {
			return
		}
		var params paramOffsets
		if !pat.match(routed, use, &params) {
			return
		}
		for i, name := range pat.names {
			off := params[i]
			if off[0] < 0 || off[0] > off[1] || off[1] > len(routed) {
				t.Fatalf("param %s offsets %v outside path %q", name, off, path)
			}
			v := path[off[0]:off[1]]
			if (strings.HasPrefix(name, "+") || !strings.ContainsAny(name[:1], "*+") && !isOptional(pat, i)) && v == "" {
				t.Fatalf("param %s is empty for %q on %q", name, pattern, path)
			}
		}
	})
}

func isOptional(p *pattern, param int) bool {
	for _, s := range p.segs {
		if s.param == param {
			return s.kind == segOptional
		}
	}
	return false
}
