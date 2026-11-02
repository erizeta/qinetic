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

qinetic is in early development and has no release yet; the API will change until v1.0.0. Routing, the
request/response API, serving over HTTP/1.1, HTTP/2, and HTTP/3, and the basic middleware work today; rate
limiting, CSRF, and caching come next. See the [changelog](CHANGELOG.md).

## Installation

Requires Go 1.26 or newer.

```sh
go get github.com/erizeta/qinetic
```

## Quick start

```go
package main

import (
	"log"

	"github.com/erizeta/qinetic"
)

func main() {
	app := qinetic.New()
	app.Get("/", func(c *qinetic.Ctx) error {
		return c.SendString("Hello, " + c.Protocol())
	})
	log.Fatal(app.Listen(":3000"))
}
```

## Example

A small REST API.

```go
package main

import (
	"log"
	"strconv"
	"sync"

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
	log.Fatal(newApp().Listen(":3000"))
}
```

Run it with `go run ./examples/rest`, or test it without a network through `app.Test`, as
[`examples/rest/main_test.go`](examples/rest/main_test.go) does.

### Middleware

Middleware lives under `middleware/`, and each package has `New(config ...Config) qinetic.Handler` with a
`Next` function to skip it:

| Package | Does |
|---|---|
| `recovery` | Turns panics into a 500 through the ErrorHandler; `qilat.ErrAbortHandler` still aborts |
| `logger` | One `slog` record per request: method, raw path (no query), status, latency, bytes, protocol, IP, request ID |
| `requestid` | Keeps a valid incoming `X-Request-ID` (at most 128 safe characters) or generates one; `requestid.FromContext(c)` |
| `cors` | Exact and `https://*.example.com` origins, preflight, credentials (refuses `*` with credentials at startup), always `Vary: Origin` |
| `compress` | gzip or other codings through `qilat.CompressWith`, skipping small bodies, images, and Range requests |
| `static` | Files from a directory or `fs.FS`, with Range, 304, and index files; listings only with `Browse`; `..` refused |

```go
package main

import (
	"log"

	"github.com/erizeta/qinetic"
	"github.com/erizeta/qinetic/middleware/compress"
	"github.com/erizeta/qinetic/middleware/cors"
	"github.com/erizeta/qinetic/middleware/logger"
	"github.com/erizeta/qinetic/middleware/recovery"
	"github.com/erizeta/qinetic/middleware/requestid"
	"github.com/erizeta/qinetic/middleware/static"
)

func main() {
	app := qinetic.New()
	// requestid first so the logger sees the ID; logger before recovery so panics are logged as 500s.
	app.Use(requestid.New(), logger.New(), recovery.New(), compress.New())

	api := app.Group("/api", cors.New(cors.Config{
		AllowOrigins:     []string{"https://app.example.com", "https://*.preview.example.com"},
		AllowCredentials: true,
	}))
	api.Get("/hello", func(c *qinetic.Ctx) error {
		return c.JSON(map[string]string{"request_id": requestid.FromContext(c)})
	})

	app.Use("/assets", static.New("./public", static.Config{MaxAge: 3600}))
	log.Fatal(app.Listen(":3000"))
}
```

Order matters: put `requestid` before `logger`, and `logger` before `recovery`. `compress` is qilat middleware run through
`qinetic.FromQilatMiddleware`: the handlers after it, the ErrorHandler, and the buffered response all run
inside it, so its output covers error pages too, and middleware registered before it see the response as
already sent: status and size are still available from `c.StatusCode()` and `c.ResponseSize()`, and a late
write returns `qinetic.ErrResponseSent` instead of corrupting the compressed body. Register middleware that
must change the body, such as a cache, after `compress`.
`FromStdMiddleware`, `FromQilat`, and `FromStd` bring in net/http and qilat middleware and handlers the same way.

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

### HTTP/3 and graceful shutdown

`ListenTLS` serves HTTP/1.1 and HTTP/2 over TCP and HTTP/3 over UDP on the same port, advertising HTTP/3 to TCP
clients with `Alt-Svc`. `ListenTLSWithCertificate` takes a certificate in memory, such as a development one:

```go
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/erizeta/qilat"
	"github.com/erizeta/qinetic"
)

func main() {
	cert, err := qilat.GenerateDevCert()
	if err != nil {
		log.Fatal(err)
	}
	app := qinetic.New()
	app.Get("/", func(c *qinetic.Ctx) error {
		return c.SendString("Hello over " + c.Protocol() + "\n")
	})
	app.OnShutdown(func(_ context.Context) error {
		log.Println("server stopped")
		return nil
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		if err := app.ShutdownWithTimeout(10 * time.Second); err != nil {
			log.Println(err)
		}
	}()
	// HTTP/1.1 and HTTP/2 on TCP, HTTP/3 on UDP, all on port 8443. After Shutdown, Listen returns once
	// in-flight requests and OnShutdown hooks are done.
	if err := app.ListenTLSWithCertificate(":8443", cert); !errors.Is(err, qilat.ErrServerClosed) {
		log.Fatal(err)
	}
}
```

Try it with `curl -k --http1.1 https://localhost:8443/`, `curl -k https://localhost:8443/` (HTTP/2), and, with
a curl built with HTTP/3, `curl -k --http3-only https://localhost:8443/`.

After `Shutdown`, `Listen` returns `qilat.ErrServerClosed` only once in-flight requests have finished and the
`OnShutdown` hooks have run, so returning from `main` right after it cuts nothing short.

Other ways to serve: `Listen` (HTTP/1.1, plus cleartext HTTP/2 with `Config.H2C`), `Listener(net.Listener)`,
`ListenerTLS(net.Listener)` and `ListenQUIC(net.PacketConn)` with certificates from `Config.TLSConfig`, or
`app.Handler()` with your own `qilat.Server`. Listen calls running at the same time share one server, so
`ListenerTLS` and `ListenQUIC` together serve HTTP/1.1, HTTP/2, and HTTP/3 and stop with one `Shutdown`.

### Server limits

The Listen methods build a `qilat.Server` from `Config`. Zero values keep qilat's defaults, so slow clients are
cut off by the header and stall timeouts even though a request or response as a whole has no time limit:

| Setting | Default | Set with |
|---|---|---|
| Request header | 10 s (`qilat.DefaultReadHeaderTimeout`) | `Config.Server` |
| Request body or response stalled | 60 s without progress (`qilat.DefaultStallTimeout`) | `Config.Server` |
| Keep-alive idle | 120 s (`qilat.DefaultIdleTimeout`) | `Config.IdleTimeout` |
| Whole request / whole response | no limit | `Config.ReadTimeout` / `Config.WriteTimeout` |
| Request body size | 4 MiB | `Config.BodyLimit` |
| Header size | 64 KiB (`qilat.DefaultMaxHeaderBytes`) | `Config.Server` |

`Config.Server` receives the `qilat.Server` before it starts, for anything else: TLS settings, connection
limits, HTTP/2 and HTTP/3 tuning.

### Errors

A handler error goes to `Config.ErrorHandler`. The default sends an `*Error`'s code and message (`ErrNotFound`,
`NewError(422, "name is required")`) and turns any other error into a plain 500, logging the details instead
of sending them. Bind errors are 400s that never include decoder output.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Report security issues through [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE)
