# Qinetic

Qinetic is a Go web framework with a [Fiber](https://github.com/gofiber/fiber)-style API, built on the
[qilat](https://github.com/erizeta/qilat) HTTP engine instead of fasthttp.

From qilat it inherits:

- HTTP/1.1, HTTP/2, and HTTP/3 served by one app;
- a request context that is canceled when the client disconnects;
- an allocation-free hot path and a `net/http` adapter.

Qinetic adds routing with `:param` and `*` patterns, groups, a middleware chain with `c.Next()`, and ready-made
middleware.

## Status

Qinetic is pre-1.0: until v1.0.0, a minor version may change the API, with what to change listed in the
[changelog](CHANGELOG.md). It pins qilat v0.2.2. Coming from Fiber? See
[docs/migrating-from-fiber.md](docs/migrating-from-fiber.md).

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

### Routing

| Pattern | Matches | `c.Params` |
|---|---|---|
| `/users` | `/users`, and `/users/` unless `StrictRouting` | |
| `/users/:id` | `/users/42`, not `/users` or `/users/42/x` | `id` |
| `/users/:id?` | `/users` and `/users/42` (last segment only) | `id`, maybe empty |
| `/files/*` | `/files`, `/files/a/b` | `*` (or `*1`), maybe empty |
| `/files/+` | `/files/a/b`, not `/files` | `+` (or `+1`) |
| `/a/*/b/*` | `/a/x/y/b/z`; inner wildcards are greedy | `*1`, `*2` |

- Handlers run in registration order. `Use(prefix, ...)` matches at segment boundaries (`/api` matches
  `/api/x`, not `/apiv2`), method routes match the whole path, and `c.Next()` moves to the next match.
- `Get` also registers HEAD. When nothing matches, the response is 404, or 405 with `Allow` when the path
  matches routes of other methods.
- Routing uses the raw path, so `%2F` stays inside one segment; `c.Params` returns it decoded. Paths with
  empty or dot segments, encoded or not, are not routed: only middleware registered on `/` runs, then 404.
- Matching is case-insensitive unless `Config.CaseSensitive` is set. Parameters and wildcards are whole
  segments; patterns Fiber accepts within a segment, such as `/:from-:to`, panic at registration.

### The Ctx

- Request: `Method`, `Path`, `OriginalURL`, `Host`, `Hostname`, `IP`, `IPs`, `Secure`, `Protocol`,
  `Params`, `ParamsInt`, `Query`, `QueryInt`, `QueryBool`, `Queries`, `Get`, `Cookies`, `Body`,
  `BodyReader`, `FormValue`, `FormFile`, `MultipartForm`, `Bind()`, `Locals`, `Route`, `Context`, `Request`.
- Response: `Status`, `Set`, `Append`, `Vary`, `Type`, `Send`, `SendString`, `SendStatus`, `JSON`, `XML`,
  `Redirect`, `Cookie`, `ClearCookie`, `Attachment`, `Download`, `SendFile`, `SendStream`, `Writer`,
  `Flush`, `Trailer`.
- For middleware: `Next`, `StatusCode`, `ResponseSize`, `ResponseHeader`, `ResponseBody`, `Committed`,
  `HandleError`.

The response is buffered and sent with a `Content-Length` when the handler chain returns, so an
`ErrorHandler` can still replace it. `SendStream`, `Writer`, `Flush`, and `SendFile` send it as they go.

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
| `limiter` | Fixed or sliding window per key (IP by default), `X-RateLimit-*` headers, 429 with `Retry-After` |
| `csrf` | Refuses cross-origin unsafe requests (`Sec-Fetch-Site`, `Origin`) and requires a stored token for every one |
| `cache` | Stores GET responses and serves them to GET and HEAD, one copy per value of each `Vary` header |

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

Compressing a page that holds a secret, such as a CSRF token, together with input the client controls lets an
attacker who can watch response sizes guess the secret (BREACH). Skip `compress` for such pages with its
`Next` option, or keep reflected input out of them.

### Rate limiting, CSRF, and storage

```go
package main

import (
	"html/template"
	"log"
	"time"

	"github.com/erizeta/qinetic"
	"github.com/erizeta/qinetic/middleware/csrf"
	"github.com/erizeta/qinetic/middleware/limiter"
)

var page = template.Must(template.New("form").Parse(`<form method="post" action="/comments">
<input type="hidden" name="_csrf" value="{{.}}">
<textarea name="body"></textarea>
<button>Send</button>
</form>`))

func newApp() *qinetic.App {
	app := qinetic.New()
	// 20 requests per minute per IP, counted over a sliding window.
	app.Use(limiter.New(limiter.Config{Max: 20, Expiration: time.Minute, Algorithm: limiter.SlidingWindow}))
	app.Use(csrf.New())

	app.Get("/", func(c *qinetic.Ctx) error {
		c.Type("html")
		return page.Execute(c.Writer(), csrf.TokenFromContext(c))
	})
	app.Post("/comments", func(c *qinetic.Ctx) error {
		// Only reached with the token from the form and a same-origin request.
		return c.SendString("saved: " + c.FormValue("body"))
	})
	return app
}

func main() {
	log.Fatal(newApp().Listen(":3000"))
}
```

`csrf` checks every POST, PUT, PATCH, and DELETE twice. First the origin: a browser-marked cross-site
request is refused, as is one whose `Origin` differs from the request's own scheme and host and is not in
`TrustedOrigins`; over HTTPS, a request without `Origin` needs a matching `Referer`. Then the token sent in
the `X-Csrf-Token` header or `_csrf` form field must match the `csrf_` cookie and still be stored. The cookie is HttpOnly by
default, so pages get the token from `csrf.TokenFromContext(c)`; call `csrf.DeleteToken(c)` on login and
logout. Rejected requests do not use up the rate limit.

`limiter` and `csrf` keep state in a `qinetic.Storage`, in memory by default and per middleware. The memory
store starts no goroutine: expired entries go on access, and it holds at most 100,000 entries. To share limits
and tokens between instances, pass a `Storage` backed by Redis or similar; if it also implements
`qinetic.Incrementer`, limits are counted atomically across them.

### Caching

`cache.New()` stores 200 responses to GET for a minute, keyed by host, path, and query, and marks responses
`X-Cache: hit` or `miss`. Requests with `Authorization`, `Cookie`, or `Cache-Control: no-store` bypass it,
since with cookie sessions their responses are often personal; set `CacheCookieRequests` only with a
`KeyGenerator` that separates users. Nothing is stored for responses with `Set-Cookie`, `Cache-Control`
private, no-store, or no-cache, or a streamed body.

Register it after `compress`, as in `app.Use(compress.New(), cache.New())`. The cache then runs inside
compress: it stores the body before compression, and compress compresses each hit for the client's
`Accept-Encoding`. Registered before compress, it would see responses already sent and store nothing.
Headers set by middleware registered before the cache, such as `cors` and `requestid`, are not stored,
since they run again on every hit.

A response with `Vary` is stored once per combination of the request headers it names, so a response for
one `Origin` (which `cors` always varies on) or `Accept-Language` is never served to another. A header that
is absent differs from one that is empty. `Vary: *` is never stored, and at most `MaxVariants` copies (16)
are kept per key, so random header values cannot fill the storage. The default storage is in memory and
holds at most 64 MiB; with an external `Storage`, entries are versioned, and an unreadable one is a miss.
If the storage fails, requests are served uncached and the error is logged.

### Strings and lifetimes

- `Params`, `Query`, `Get`, `Cookies`, and `FormValue` return copies, safe to keep after the handler returns.
  Unlike Fiber, there is no unsafe string mode.
- `PeekParam`, `PeekQuery`, and `PeekHeader` return `[]byte` without copying; they are valid only until the
  handler chain returns, like everything from qilat's `Request`.
- `c.Body()` returns `([]byte, error)`: the body is streamed, so reading can fail. Past `Config.BodyLimit`
  (4 MiB by default) it fails with `ErrRequestEntityTooLarge`.
- `c.Context()` is canceled when the client disconnects and may be kept; `c` itself may not.

#### Work that outlives the request

Once the handler chain returns, its `Ctx` is reset and reused for another request, and the request body
buffer with it. A goroutine that is still running then must not touch `c`: copy what it needs first, and give
it a context that is not canceled when the request ends.

```go
package main

import (
	"bytes"
	"context"
	"log"
	"log/slog"

	"github.com/erizeta/qinetic"
)

// sendReceipt stands in for slow work that outlives the request, such as sending an email.
var sendReceipt = func(ctx context.Context, orderID string, payload []byte) {
	slog.InfoContext(ctx, "receipt sent", "order", orderID, "bytes", len(payload))
}

func newApp() *qinetic.App {
	app := qinetic.New()
	app.Post("/orders/:id", func(c *qinetic.Ctx) error {
		// c is reused once this handler returns, so copy what the goroutine needs first.
		id := c.Params("id") // strings are already copies
		body, err := c.Body()
		if err != nil {
			return err
		}
		payload := bytes.Clone(body) // the body buffer is reused: copy it
		// Keep the request's values without its cancellation, which fires when the handler returns.
		ctx := context.WithoutCancel(c.Context())

		go sendReceipt(ctx, id, payload) // never touches c
		return c.SendStatus(202)
	})
	return app
}

func main() {
	log.Fatal(newApp().Listen(":3000"))
}
```

- Use `c` from one goroutine at a time; it has no locks. Goroutines the handler waits for, through a
  `sync.WaitGroup` or `errgroup`, may use it as long as they do not use it at once.
- Strings from `Params`, `Query`, `Get`, `Cookies`, and `FormValue` can be kept as they are; `[]byte` from
  `c.Body()` and the `Peek*` methods must be copied.
- `Config.DebugPoison` makes any use of a `Ctx` after its handler returned panic, so these mistakes show up
  in tests instead of as one request reading another's data.

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

## Performance

`benchmarks/` compares plain qilat (its `ServeMux` and `qilat.Chain`), Qinetic, and Fiber v3.6.1 on the same
scenarios with the same minimal keep-alive HTTP/1.1 client, 8 connections per GOMAXPROCS. Fiber gets a 10 s
read timeout and a 120 s idle timeout, as qilat applies by default, so all three pay for the same slowloris
protection; without them Fiber skips a per-request deadline and looks about 35% faster in memory. In memory, the
listener skips the kernel, so the numbers show server CPU per request; over loopback, the socket path
dominates. Median of 10 runs on an Apple M5 (10 cores), Go 1.27.3, qilat v0.2.2, in ns/op:

| Scenario | In memory: qilat | Qinetic | Fiber | Loopback: qilat | Qinetic | Fiber |
|---|---|---|---|---|---|---|
| Plaintext | 253 | 249 | 240 | 5862 | 5856 | 6488 |
| JSON | 292 | 284 | 280 | 5832 | 5843 | 6723 |
| Param route | 255 | 249 | 244 | 5886 | 5888 | 6484 |
| Last of 100 routes | 262 | 256 | 250 | 5882 | 5884 | 6557 |
| 5 middleware | 249 | 259 | 235 | 5876 | 5902 | 6508 |

In memory, Fiber spends 1–4% less CPU per request, and 9% less through five middleware; over a real socket,
in this setup, Qinetic is about 10% faster than Fiber. Qinetic costs no measurable time over plain qilat.
The client allocates nothing, so allocations are the server's: Qinetic makes none for plaintext and the route
lookups, one for the copied `Params` string, and two for `json.Marshal`. A static, parameter, or wildcard hello
route allocates nothing beyond qilat (`TestHelloAllocs`), and route matching allocates nothing
(`BenchmarkRouterMatch`).

Reproduce with:

```sh
cd benchmarks && go test -run '^$' -bench BenchmarkCompare -benchmem -count 10 .
```

`make bench` runs these and the router and baseline benchmarks.

## Differences from Fiber

| Fiber | Qinetic |
|---|---|
| fasthttp, HTTP/1.1 only | qilat: HTTP/1.1, HTTP/2, and HTTP/3 from one `App` |
| `c.Context()` is not canceled when the client disconnects | It is, so work stops when nobody waits for it |
| Strings point into reused buffers unless `Immutable` | `Params`, `Query`, `Get`, and the rest return copies; `Peek*` variants return `[]byte` |
| `c.Body()` returns `[]byte` | `c.Body()` returns `([]byte, error)`, since the body is streamed |
| A non-`*Error` error sends `err.Error()` to the client | It sends a generic 500 and logs the details |
| `fiber.Ctx` is an interface | `*qinetic.Ctx` is a struct |
| Paths are normalized, and `%2F` may split segments | Routing uses the raw path; unclean paths get 404 |

[docs/migrating-from-fiber.md](docs/migrating-from-fiber.md) covers routing syntax, middleware, serving, and
the other changes in detail.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Report security issues through [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE)
