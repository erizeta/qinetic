# qinetic

qinetic is a Go web framework with a [Fiber](https://github.com/gofiber/fiber)-style API, built on the
[qilat](https://github.com/erizeta/qilat) HTTP engine instead of fasthttp.

From qilat it inherits:

- HTTP/1.1, HTTP/2, and HTTP/3 served by one app;
- a request context that is canceled when the client disconnects;
- an allocation-free hot path and a `net/http` adapter.

qinetic adds routing with `:param` and `*` patterns, groups, a middleware chain with `c.Next()`, and ready-made
middleware.

## Status

qinetic is in early development and has no release yet; the API will change until v1.0.0. Routing, groups,
the `c.Next()` chain, error handling, and in-memory testing with `app.Test` work today. An `App` is a
`qilat.Handler`, so it can already be served by a `qilat.Server`; `app.Listen` and the request/response helpers
come next. See the [changelog](CHANGELOG.md).

## Installation

Requires Go 1.26 or newer.

```sh
go get github.com/erizeta/qinetic
```

## Example

A small REST API. Until `app.Listen` lands, an `App` is served by a `qilat.Server`, since it is a
`qilat.Handler`.

```go
package main

import (
	"log"
	"strconv"
	"sync"

	"github.com/erizeta/qilat"
	"github.com/erizeta/qinetic"
)

type user struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type store struct {
	mu    sync.Mutex
	next  int
	users map[int]user
}

func newApp() *qinetic.App {
	s := &store{users: map[int]user{}}
	app := qinetic.New()

	api := app.Group("/api")
	api.Get("/users/:id", func(c *qinetic.Ctx) error {
		id, err := c.ParamsInt("id")
		if err != nil {
			return qinetic.ErrBadRequest
		}
		s.mu.Lock()
		u, ok := s.users[id]
		s.mu.Unlock()
		if !ok {
			return qinetic.ErrNotFound
		}
		return c.JSON(u)
	})
	api.Post("/users", func(c *qinetic.Ctx) error {
		var u user
		if err := c.Bind().JSON(&u); err != nil {
			return err
		}
		if u.Name == "" {
			return qinetic.NewError(422, "name is required")
		}
		s.mu.Lock()
		s.next++
		u.ID = s.next
		s.users[u.ID] = u
		s.mu.Unlock()
		c.Set("Location", "/api/users/"+strconv.Itoa(u.ID))
		return c.Status(201).JSON(u)
	})
	api.Delete("/users/:id", func(c *qinetic.Ctx) error {
		id, err := c.ParamsInt("id")
		if err != nil {
			return qinetic.ErrBadRequest
		}
		s.mu.Lock()
		delete(s.users, id)
		s.mu.Unlock()
		return c.SendStatus(204)
	})
	return app
}

func main() {
	srv := &qilat.Server{Handler: newApp()}
	log.Fatal(srv.ListenAndServe(":3000"))
}
```

Run it with `go run ./examples/rest`, or test it without a network through `app.Test`, as
[`examples/rest/main_test.go`](examples/rest/main_test.go) does.

### Strings and lifetimes

- `Params`, `Query`, `Get`, `Cookies`, and `FormValue` return copies, safe to keep after the handler returns.
  Unlike Fiber, there is no unsafe string mode.
- `PeekParam`, `PeekQuery`, and `PeekHeader` return `[]byte` without copying; they are valid only until the
  handler chain returns, like everything from qilat's `Request`.
- `c.Body()` returns `([]byte, error)`: the body is streamed, so reading can fail. Past `Config.BodyLimit`
  (4 MiB by default) it fails with `ErrRequestEntityTooLarge`.
- `c.Context()` is canceled when the client disconnects and may be kept; `c` itself may not.

### Behind a proxy

Set `Config.TrustedProxies` to your proxies' addresses. Only requests from them have their `X-Forwarded-Proto`
and `X-Forwarded-Host` believed by `c.Secure()` and `c.Host()`, and their `Config.ProxyHeader` (for example
`X-Forwarded-For`) by `c.IP()`. `c.IP()` returns the rightmost address that is not a trusted proxy and
`c.Host()` the last forwarded host, so a client cannot choose either by sending the headers itself. Fiber
takes the first forwarded host instead.

### Errors

A handler error goes to `Config.ErrorHandler`. The default sends an `*Error`'s code and message (`ErrNotFound`,
`NewError(422, "name is required")`) and turns any other error into a plain 500, logging the details instead
of sending them. Bind errors are 400s that never include decoder output.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Report security issues through [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE)
