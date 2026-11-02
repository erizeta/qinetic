package qinetic

import (
	"fmt"
	"net/http"
	"strings"
)

// Router registers routes. *App and *Group implement it.
type Router interface {
	Use(args ...any) Router
	Get(path string, handler Handler, handlers ...Handler) Router
	Head(path string, handler Handler, handlers ...Handler) Router
	Post(path string, handler Handler, handlers ...Handler) Router
	Put(path string, handler Handler, handlers ...Handler) Router
	Patch(path string, handler Handler, handlers ...Handler) Router
	Delete(path string, handler Handler, handlers ...Handler) Router
	Options(path string, handler Handler, handlers ...Handler) Router
	Connect(path string, handler Handler, handlers ...Handler) Router
	Trace(path string, handler Handler, handlers ...Handler) Router
	All(path string, handler Handler, handlers ...Handler) Router
	Add(methods []string, path string, handler Handler, handlers ...Handler) Router
	Group(prefix string, handlers ...Handler) Router
	Route(prefix string, fn func(Router)) Router
}

// allMethods are the methods All registers.
var allMethods = []string{
	http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
	http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace,
}

// Group is a set of routes under a common prefix, created by Router.Group.
type Group struct {
	app    *App
	prefix string
}

var _ Router = (*Group)(nil)
var _ Router = (*App)(nil)

// joinPath appends path to a group prefix as Fiber does: the prefix loses its trailing slash.
func joinPath(prefix, path string) string {
	if path == "" {
		return prefix
	}
	if path[0] != '/' {
		path = "/" + path
	}
	return strings.TrimRight(prefix, "/") + path
}

// Use registers middleware that runs for every method on paths under a prefix, "/" by default.
// Arguments are an optional prefix (string or []string) followed by one or more Handlers.
// Without strict routing, Use("/api") matches /api and /api/x but not /apiv2.
func (g *Group) Use(args ...any) Router {
	var prefixes []string
	var handlers []Handler
	for i, arg := range args {
		switch a := arg.(type) {
		case string:
			if i != 0 {
				panic(fmt.Sprintf("qinetic: Use: prefix %q must be the first argument", a))
			}
			prefixes = []string{a}
		case []string:
			if i != 0 {
				panic("qinetic: Use: prefixes must be the first argument")
			}
			prefixes = a
		case Handler:
			if a == nil {
				panic("qinetic: Use: nil handler")
			}
			handlers = append(handlers, a)
		default:
			panic(fmt.Sprintf("qinetic: Use: unsupported argument of type %T", arg))
		}
	}
	if len(handlers) == 0 {
		panic("qinetic: Use: no handlers")
	}
	if prefixes == nil {
		prefixes = []string{"/"}
	}
	for _, p := range prefixes {
		g.app.router.add("", joinPath(g.prefix, p), true, handlers)
	}
	return g
}

// Get registers a GET route, and the same handlers for HEAD.
func (g *Group) Get(path string, handler Handler, handlers ...Handler) Router {
	return g.Add([]string{http.MethodGet, http.MethodHead}, path, handler, handlers...)
}

// Head registers a HEAD route.
func (g *Group) Head(path string, handler Handler, handlers ...Handler) Router {
	return g.Add([]string{http.MethodHead}, path, handler, handlers...)
}

// Post registers a POST route.
func (g *Group) Post(path string, handler Handler, handlers ...Handler) Router {
	return g.Add([]string{http.MethodPost}, path, handler, handlers...)
}

// Put registers a PUT route.
func (g *Group) Put(path string, handler Handler, handlers ...Handler) Router {
	return g.Add([]string{http.MethodPut}, path, handler, handlers...)
}

// Patch registers a PATCH route.
func (g *Group) Patch(path string, handler Handler, handlers ...Handler) Router {
	return g.Add([]string{http.MethodPatch}, path, handler, handlers...)
}

// Delete registers a DELETE route.
func (g *Group) Delete(path string, handler Handler, handlers ...Handler) Router {
	return g.Add([]string{http.MethodDelete}, path, handler, handlers...)
}

// Options registers an OPTIONS route.
func (g *Group) Options(path string, handler Handler, handlers ...Handler) Router {
	return g.Add([]string{http.MethodOptions}, path, handler, handlers...)
}

// Connect registers a CONNECT route.
func (g *Group) Connect(path string, handler Handler, handlers ...Handler) Router {
	return g.Add([]string{http.MethodConnect}, path, handler, handlers...)
}

// Trace registers a TRACE route.
func (g *Group) Trace(path string, handler Handler, handlers ...Handler) Router {
	return g.Add([]string{http.MethodTrace}, path, handler, handlers...)
}

// All registers a route for every standard method.
func (g *Group) All(path string, handler Handler, handlers ...Handler) Router {
	return g.Add(allMethods, path, handler, handlers...)
}

// Add registers a route for each of methods. A route matches the whole path; its handlers run in order
// when each calls c.Next.
func (g *Group) Add(methods []string, path string, handler Handler, handlers ...Handler) Router {
	all := append([]Handler{handler}, handlers...)
	for _, h := range all {
		if h == nil {
			panic(fmt.Sprintf("qinetic: nil handler for route %q", path))
		}
	}
	full := joinPath(g.prefix, path)
	for _, m := range methods {
		if m == "" {
			panic(fmt.Sprintf("qinetic: empty method for route %q", path))
		}
		g.app.router.add(strings.ToUpper(m), full, false, all)
	}
	return g
}

// Group returns a Router for routes under prefix. handlers, if any, run as middleware for that prefix.
func (g *Group) Group(prefix string, handlers ...Handler) Router {
	sub := &Group{app: g.app, prefix: joinPath(g.prefix, prefix)}
	if len(handlers) > 0 {
		args := make([]any, len(handlers))
		for i, h := range handlers {
			args[i] = h
		}
		sub.Use(args...)
	}
	return sub
}

// Route calls fn with a Router for routes under prefix and returns that Router.
func (g *Group) Route(prefix string, fn func(Router)) Router {
	sub := g.Group(prefix)
	fn(sub)
	return sub
}

// Use registers middleware; see Group.Use.
func (app *App) Use(args ...any) Router { return app.group.Use(args...) }

// Get registers a GET route, and the same handlers for HEAD.
func (app *App) Get(path string, handler Handler, handlers ...Handler) Router {
	return app.group.Get(path, handler, handlers...)
}

// Head registers a HEAD route.
func (app *App) Head(path string, handler Handler, handlers ...Handler) Router {
	return app.group.Head(path, handler, handlers...)
}

// Post registers a POST route.
func (app *App) Post(path string, handler Handler, handlers ...Handler) Router {
	return app.group.Post(path, handler, handlers...)
}

// Put registers a PUT route.
func (app *App) Put(path string, handler Handler, handlers ...Handler) Router {
	return app.group.Put(path, handler, handlers...)
}

// Patch registers a PATCH route.
func (app *App) Patch(path string, handler Handler, handlers ...Handler) Router {
	return app.group.Patch(path, handler, handlers...)
}

// Delete registers a DELETE route.
func (app *App) Delete(path string, handler Handler, handlers ...Handler) Router {
	return app.group.Delete(path, handler, handlers...)
}

// Options registers an OPTIONS route.
func (app *App) Options(path string, handler Handler, handlers ...Handler) Router {
	return app.group.Options(path, handler, handlers...)
}

// Connect registers a CONNECT route.
func (app *App) Connect(path string, handler Handler, handlers ...Handler) Router {
	return app.group.Connect(path, handler, handlers...)
}

// Trace registers a TRACE route.
func (app *App) Trace(path string, handler Handler, handlers ...Handler) Router {
	return app.group.Trace(path, handler, handlers...)
}

// All registers a route for every standard method.
func (app *App) All(path string, handler Handler, handlers ...Handler) Router {
	return app.group.All(path, handler, handlers...)
}

// Add registers a route for each of methods; see Group.Add.
func (app *App) Add(methods []string, path string, handler Handler, handlers ...Handler) Router {
	return app.group.Add(methods, path, handler, handlers...)
}

// Group returns a Router for routes under prefix; see Group.Group.
func (app *App) Group(prefix string, handlers ...Handler) Router {
	return app.group.Group(prefix, handlers...)
}

// Route calls fn with a Router for routes under prefix; see Group.Route.
func (app *App) Route(prefix string, fn func(Router)) Router {
	return app.group.Route(prefix, fn)
}
